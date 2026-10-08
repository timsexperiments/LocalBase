import { basename, dirname, join } from "node:path";

const GGUF_MAGIC = 0x46554747;
const READ_CHUNK_BYTES = 1024 * 1024;
const MAX_KEPT_ARRAY_ELEMENTS = 4096;
const MAX_KEPT_STRING_BYTES = 256;
const MAX_KV_COUNT = 1_000_000;
/** Extra ubatch tokens llama.cpp keeps in each sliding-window cache. */
const SWA_UBATCH_TOKENS = 512;
/** llama.cpp hardcodes a 4-layer full-attention period for qwen3next. */
const QWEN3NEXT_FULL_ATTENTION_INTERVAL = 4;

const BYTES_PER_ELEMENT = {
  f32: 4,
  f16: 2,
  bf16: 2,
  q8_0: 34 / 32,
  q4_0: 18 / 32,
  q4_1: 20 / 32,
  q5_0: 22 / 32,
  q5_1: 24 / 32,
  iq4_nl: 18 / 32,
} as const;

export type KvCacheType = keyof typeof BYTES_PER_ELEMENT;

/** Per-model attention cache shape, summed over the layers that hold KV. */
export type LlmKvGeometry = {
  readonly architecture: string;
  readonly blockCount: number;
  /** Sum of KV heads over layers whose cache spans the full context. */
  readonly fullKvHeads: number;
  /** Sum of KV heads over sliding-window layers. */
  readonly swaKvHeads: number;
  readonly slidingWindow: number | null;
  readonly keyLength: number;
  readonly valueLength: number;
  readonly contextLength: number | null;
};

type GgufValue = number | bigint | boolean | string | number[] | null;

const FIXED_SIZES: Record<number, number> = {
  0: 1,
  1: 1,
  2: 2,
  3: 2,
  4: 4,
  5: 4,
  6: 4,
  7: 1,
  10: 8,
  11: 8,
  12: 8,
};

class GgufReader {
  private buffer = new Uint8Array(0);
  private bufferStart = 0;
  position = 0;

  constructor(private readonly file: Blob) {}

  private async take(length: number): Promise<DataView> {
    const end = this.position + length;
    const bufferEnd = this.bufferStart + this.buffer.length;
    if (this.position < this.bufferStart || end > bufferEnd) {
      const size = Math.max(length, READ_CHUNK_BYTES);
      const chunk = new Uint8Array(
        await this.file
          .slice(this.position, this.position + size)
          .arrayBuffer(),
      );
      if (chunk.length < length)
        throw new Error("Unexpected end of GGUF file.");
      this.buffer = chunk;
      this.bufferStart = this.position;
    }
    const offset = this.position - this.bufferStart;
    this.position = end;
    return new DataView(
      this.buffer.buffer,
      this.buffer.byteOffset + offset,
      length,
    );
  }

  skip(length: number): void {
    this.position += length;
  }

  async u32(): Promise<number> {
    return (await this.take(4)).getUint32(0, true);
  }

  async u64(): Promise<number> {
    const value = (await this.take(8)).getBigUint64(0, true);
    if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new Error("GGUF length is out of range.");
    }
    return Number(value);
  }

  async string(keep: boolean): Promise<string | null> {
    const length = await this.u64();
    if (!keep || length > MAX_KEPT_STRING_BYTES) {
      this.skip(length);
      return null;
    }
    const view = await this.take(length);
    return new TextDecoder().decode(
      new Uint8Array(view.buffer, view.byteOffset, length),
    );
  }

  async scalar(type: number): Promise<number | bigint | boolean> {
    const view = await this.take(FIXED_SIZES[type] ?? 0);
    switch (type) {
      case 0:
        return view.getUint8(0);
      case 1:
        return view.getInt8(0);
      case 2:
        return view.getUint16(0, true);
      case 3:
        return view.getInt16(0, true);
      case 4:
        return view.getUint32(0, true);
      case 5:
        return view.getInt32(0, true);
      case 6:
        return view.getFloat32(0, true);
      case 7:
        return view.getUint8(0) !== 0;
      case 10:
        return view.getBigUint64(0, true);
      case 11:
        return view.getBigInt64(0, true);
      case 12:
        return view.getFloat64(0, true);
      default:
        throw new Error(`Unsupported GGUF scalar type ${type}.`);
    }
  }

  async value(type: number, keep: boolean): Promise<GgufValue> {
    if (type === 8) return await this.string(keep);
    if (type !== 9) {
      const value = await this.scalar(type);
      return typeof value === "bigint" ? Number(value) : value;
    }
    const elementType = await this.u32();
    const count = await this.u64();
    if (elementType === 8) {
      for (let index = 0; index < count; index += 1) {
        this.skip(await this.u64());
      }
      return null;
    }
    const size = FIXED_SIZES[elementType];
    if (size === undefined) throw new Error("Unsupported GGUF array type.");
    if (!keep || count > MAX_KEPT_ARRAY_ELEMENTS) {
      this.skip(size * count);
      return null;
    }
    const values: number[] = [];
    for (let index = 0; index < count; index += 1) {
      values.push(Number(await this.scalar(elementType)));
    }
    return values;
  }
}

const KEPT_SUFFIXES = [
  ".block_count",
  ".context_length",
  ".embedding_length",
  ".attention.head_count",
  ".attention.head_count_kv",
  ".attention.key_length",
  ".attention.value_length",
  ".attention.sliding_window",
  ".full_attention_interval",
];

function keepKey(key: string): boolean {
  return (
    key === "general.architecture" ||
    KEPT_SUFFIXES.some((suffix) => key.endsWith(suffix))
  );
}

/** Shard 00001 of a split model holds the metadata for every shard. */
export function ggufMetadataPath(path: string): string {
  const match = /^(.*)-(\d{5})-of-(\d{5})\.gguf$/.exec(basename(path));
  if (!match) return path;
  return join(dirname(path), `${match[1]}-00001-of-${match[3]}.gguf`);
}

async function readKeyValues(path: string): Promise<Map<string, GgufValue>> {
  const reader = new GgufReader(Bun.file(path));
  if ((await reader.u32()) !== GGUF_MAGIC) throw new Error("Not a GGUF file.");
  const version = await reader.u32();
  if (version !== 2 && version !== 3) {
    throw new Error(`Unsupported GGUF version ${version}.`);
  }
  await reader.u64();
  const count = await reader.u64();
  if (count > MAX_KV_COUNT) throw new Error("Implausible GGUF key count.");
  const values = new Map<string, GgufValue>();
  for (let index = 0; index < count; index += 1) {
    const key = (await reader.string(true)) ?? "";
    const type = await reader.u32();
    const keep = keepKey(key);
    const value = await reader.value(type, keep);
    if (keep && value !== null) values.set(key, value);
  }
  return values;
}

function positive(value: GgufValue | undefined): number | null {
  return typeof value === "number" && Number.isFinite(value) && value > 0
    ? value
    : null;
}

/** Derives which layers hold KV and how large their caches are per token. */
export function kvGeometryFromMetadata(
  values: Map<string, GgufValue>,
): LlmKvGeometry | null {
  const architecture = values.get("general.architecture");
  if (typeof architecture !== "string" || !architecture) return null;
  const get = (suffix: string) => values.get(`${architecture}.${suffix}`);
  const blockCount = positive(get("block_count"));
  const headCount = positive(get("attention.head_count"));
  if (!blockCount || !headCount) return null;

  const kvHeadsValue = get("attention.head_count_kv");
  const headsForLayer = (layer: number): number => {
    if (Array.isArray(kvHeadsValue)) return kvHeadsValue[layer] ?? 0;
    return positive(kvHeadsValue) ?? headCount;
  };
  if (Array.isArray(kvHeadsValue) && kvHeadsValue.length < blockCount) {
    return null;
  }

  const embeddingLength = positive(get("embedding_length"));
  const fallbackLength = embeddingLength
    ? Math.floor(embeddingLength / headCount)
    : null;
  const keyLength = positive(get("attention.key_length")) ?? fallbackLength;
  const valueLength =
    positive(get("attention.value_length")) ?? keyLength ?? fallbackLength;
  if (!keyLength || !valueLength) return null;

  const slidingWindow = positive(get("attention.sliding_window"));
  const interval =
    positive(get("full_attention_interval")) ??
    (architecture === "qwen3next" ? QWEN3NEXT_FULL_ATTENTION_INTERVAL : null);
  const alternatesSwa = architecture === "gpt-oss" && slidingWindow !== null;

  let fullKvHeads = 0;
  let swaKvHeads = 0;
  for (let layer = 0; layer < blockCount; layer += 1) {
    const heads = headsForLayer(layer);
    if (heads <= 0) continue;
    if (interval !== null) {
      if ((layer + 1) % interval === 0) fullKvHeads += heads;
    } else if (alternatesSwa) {
      if (layer % 2 === 0) fullKvHeads += heads;
      else swaKvHeads += heads;
    } else {
      fullKvHeads += heads;
    }
  }

  return Object.freeze({
    architecture,
    blockCount,
    fullKvHeads,
    swaKvHeads,
    slidingWindow: alternatesSwa ? slidingWindow : null,
    keyLength,
    valueLength,
    contextLength: positive(get("context_length")),
  });
}

/** Reads only the GGUF header; returns null on any failure. */
export async function readLlmKvGeometry(
  path: string,
): Promise<LlmKvGeometry | null> {
  try {
    return kvGeometryFromMetadata(await readKeyValues(ggufMetadataPath(path)));
  } catch {
    return null;
  }
}

/** Estimated KV cache bytes for a total context budget split across slots. */
export function kvCacheBytes(
  geometry: LlmKvGeometry,
  input: {
    ctxTokens: number;
    slots: number;
    cacheTypeK: KvCacheType;
    cacheTypeV: KvCacheType;
  },
): number {
  const bytesK = BYTES_PER_ELEMENT[input.cacheTypeK];
  const bytesV = BYTES_PER_ELEMENT[input.cacheTypeV];
  const perHead = geometry.keyLength * bytesK + geometry.valueLength * bytesV;
  const full = input.ctxTokens * geometry.fullKvHeads * perHead;
  const swaTokens =
    geometry.slidingWindow === null
      ? 0
      : Math.min(
          input.ctxTokens,
          input.slots * (geometry.slidingWindow + SWA_UBATCH_TOKENS),
        );
  return Math.ceil(full + swaTokens * geometry.swaKvHeads * perHead);
}
