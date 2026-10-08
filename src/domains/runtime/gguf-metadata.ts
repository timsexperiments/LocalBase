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

/**
 * Sliding-window architectures. `period` is llama.cpp's swa_period default
 * (src/models/<arch>.cpp). set_swa_pattern(n) makes layer il sliding-window
 * unless il % n == n - 1, so layers with (il + 1) % n == 0 are full.
 * A scalar `attention.sliding_window_pattern` overrides the default.
 */
const SWA_ARCHITECTURES: Record<
  string,
  { period: number; defaultWindow?: number; kvLayers?: number }
> = {
  "gpt-oss": { period: 2 },
  gemma2: { period: 2, defaultWindow: 4096 },
  gemma3: { period: 6 },
  gemma3n: { period: 5, kvLayers: 20 },
  cohere2: { period: 4 },
};

/** Upper bounds that reject corrupt or hostile headers before any looping. */
const MAX_BLOCK_COUNT = 4096;
const MAX_HEAD_COUNT = 4096;
const MAX_HEAD_LENGTH = 65536;
const MAX_TOKENS = 2 ** 31;

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
  /** V length per head; 0 for MLA models, whose cache holds only K. */
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
      return safeNumber(await this.scalar(type));
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
      values.push(Number(safeNumber(await this.scalar(elementType))));
    }
    return values;
  }
}

/** 64-bit integers beyond 2^53 become NaN so validation rejects them. */
function safeNumber(value: number | bigint | boolean): number | boolean {
  if (typeof value !== "bigint") return value;
  return value >= BigInt(Number.MIN_SAFE_INTEGER) &&
    value <= BigInt(Number.MAX_SAFE_INTEGER)
    ? Number(value)
    : Number.NaN;
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
  ".attention.sliding_window_pattern",
  ".attention.key_length_mla",
  ".attention.value_length_mla",
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

/** A safe integer in [1, max], otherwise null. */
function bounded(value: GgufValue | undefined, max: number): number | null {
  return typeof value === "number" &&
    Number.isSafeInteger(value) &&
    value > 0 &&
    value <= max
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

  // Validate every untrusted size before it can drive a loop.
  const blockCount = bounded(get("block_count"), MAX_BLOCK_COUNT);
  const headCount = bounded(get("attention.head_count"), MAX_HEAD_COUNT);
  if (!blockCount || !headCount) return null;

  const kvHeadsValue = get("attention.head_count_kv");
  let kvHeadsArray: number[] | null = null;
  let kvHeadsScalar = headCount;
  if (Array.isArray(kvHeadsValue)) {
    if (kvHeadsValue.length < blockCount) return null;
    for (let layer = 0; layer < blockCount; layer += 1) {
      const heads = kvHeadsValue[layer];
      if (
        heads === undefined ||
        !Number.isSafeInteger(heads) ||
        heads < 0 ||
        heads > MAX_HEAD_COUNT
      ) {
        return null;
      }
    }
    kvHeadsArray = kvHeadsValue;
  } else if (kvHeadsValue !== undefined) {
    const scalar = bounded(kvHeadsValue, MAX_HEAD_COUNT);
    if (!scalar) return null;
    kvHeadsScalar = scalar;
  }
  const headsForLayer = (layer: number): number =>
    kvHeadsArray ? (kvHeadsArray[layer] ?? 0) : kvHeadsScalar;

  const embeddingLength = bounded(get("embedding_length"), MAX_TOKENS);
  const fallbackLength = embeddingLength
    ? Math.floor(embeddingLength / headCount)
    : null;
  const keyLength =
    bounded(get("attention.key_length"), MAX_HEAD_LENGTH) ?? fallbackLength;
  let valueLength =
    bounded(get("attention.value_length"), MAX_HEAD_LENGTH) ??
    keyLength ??
    fallbackLength;
  if (!keyLength || !valueLength || keyLength > MAX_HEAD_LENGTH) return null;

  // llama_hparams::is_mla(): both MLA lengths set. The cache then holds only
  // K (llama-kv-cache.cpp has_v = !is_mla), sized by the regular key length.
  const mla =
    bounded(get("attention.key_length_mla"), MAX_HEAD_LENGTH) !== null &&
    bounded(get("attention.value_length_mla"), MAX_HEAD_LENGTH) !== null;
  if (mla) valueLength = 0;

  const swaSpec = SWA_ARCHITECTURES[architecture];
  const slidingWindow = swaSpec
    ? (bounded(get("attention.sliding_window"), MAX_TOKENS) ??
      swaSpec.defaultWindow ??
      null)
    : null;
  const patternValue = get("attention.sliding_window_pattern");
  // Only a scalar pattern is honoured; llama.cpp falls back to the default
  // period otherwise.
  const period =
    typeof patternValue === "number" &&
    Number.isSafeInteger(patternValue) &&
    patternValue >= 0 &&
    patternValue <= MAX_BLOCK_COUNT
      ? patternValue
      : (swaSpec?.period ?? 0);
  const kvLayers = Math.min(blockCount, swaSpec?.kvLayers ?? blockCount);
  const interval =
    bounded(get("full_attention_interval"), MAX_BLOCK_COUNT) ??
    (architecture === "qwen3next" ? QWEN3NEXT_FULL_ATTENTION_INTERVAL : null);

  let fullKvHeads = 0;
  let swaKvHeads = 0;
  for (let layer = 0; layer < kvLayers; layer += 1) {
    const heads = headsForLayer(layer);
    if (heads <= 0) continue;
    if (interval !== null) {
      if ((layer + 1) % interval === 0) fullKvHeads += heads;
    } else if (slidingWindow !== null) {
      // period 0 means every layer is sliding-window.
      if (period > 0 && (layer + 1) % period === 0) fullKvHeads += heads;
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
    slidingWindow,
    keyLength,
    valueLength,
    contextLength: bounded(get("context_length"), MAX_TOKENS),
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
