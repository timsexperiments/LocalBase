import { basename, dirname, join } from "node:path";

const GGUF_MAGIC = 0x46554747;
const READ_CHUNK_BYTES = 1024 * 1024;
const MAX_KEPT_ARRAY_ELEMENTS = 4096;
const MAX_KEPT_STRING_BYTES = 256;
const MAX_KV_COUNT = 1_000_000;
/** Extra ubatch tokens llama.cpp keeps in each sliding-window cache. */
const SWA_UBATCH_TOKENS = 512;
/** llama.cpp pads KV cell counts (and each stream) to this many cells. */
const KV_CELL_PADDING = 256;
/** Default full-attention period of the hybrid qwen architectures. */
const HYBRID_QWEN_FULL_ATTENTION_INTERVAL = 4;
const HYBRID_QWEN_ARCHITECTURES = new Set(["qwen3next", "qwen35", "qwen35moe"]);
/**
 * Other recurrent or hybrid architectures (b11671 llm_arch_is_recurrent or
 * is_hybrid) whose per-slot state is not modelled; geometry is unknown so
 * callers fall back to a coarse estimate instead of under-counting.
 */
const UNMODELLED_RECURRENT_ARCHITECTURES = new Set([
  "mamba",
  "mamba2",
  "rwkv6",
  "rwkv6qwen2",
  "rwkv7",
  "arwkv7",
  "jamba",
  "falcon-h1",
  "plamo2",
  "granitehybrid",
  "lfm2",
  "lfm2moe",
  "nemotron_h",
  "nemotron_h_moe",
  "kimi-linear",
  "deepseek4",
  "bailingmoe3",
  "kimi-k3",
  "glm5-next",
  "qwen4exp",
  "minimax-01",
]);
/** llama.cpp stores recurrent conv/ssm state as F32. */
const RECURRENT_STATE_BYTES = 4;
const MAX_SSM_KERNEL = 64;
const MAX_SSM_SIZE = 65536;
/** Quantized cache head dims must be a multiple of the q8_0 block size. */
const Q8_0_BLOCK_SIZE = 32;
/** Total parse work (key/values plus array elements) before giving up. */
const MAX_PARSE_WORK = 8_000_000;

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
  /** K/V lengths of sliding-window layers (they differ on Gemma 4). */
  readonly swaKeyLength: number;
  readonly swaValueLength: number;
  /** True when every K and V head dim is a multiple of the q8_0 block size. */
  readonly q8Compatible: boolean;
  /** FP32 conv/ssm state held per slot by hybrid recurrent layers. */
  readonly recurrentBytesPerSlot: number;
  readonly contextLength: number | null;
  /**
   * `{arch}.decision.type` of a decision model (llama.cpp b11671 `clef`).
   * Decision servers allocate no KV cache or recurrent state, so the KV and
   * recurrent counts are zero.
   */
  readonly decisionType?: string;
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

  private work = 0;

  constructor(
    private readonly file: Blob,
    private readonly maxWork = MAX_PARSE_WORK,
  ) {}

  /** Charges parse work so hostile headers cannot keep the loop spinning. */
  spend(units: number): void {
    this.work += units;
    if (this.work > this.maxWork) {
      throw new Error("GGUF header exceeds the parse budget.");
    }
  }

  remaining(): number {
    return this.file.size - this.position;
  }

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
    if (!Number.isSafeInteger(length) || length < 0) {
      throw new Error("GGUF length is out of range.");
    }
    const end = this.position + length;
    if (!Number.isSafeInteger(end) || end > this.file.size) {
      throw new Error("Unexpected end of GGUF file.");
    }
    this.position = end;
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

  async string(
    keep: boolean,
    maxBytes = MAX_KEPT_STRING_BYTES,
  ): Promise<string | null> {
    const length = await this.u64();
    if (!keep || length > maxBytes) {
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

  async value(
    type: number,
    keep: boolean,
    maxStringBytes = MAX_KEPT_STRING_BYTES,
  ): Promise<GgufValue> {
    if (type === 8) return await this.string(keep, maxStringBytes);
    if (type !== 9) {
      return safeNumber(await this.scalar(type));
    }
    const elementType = await this.u32();
    const count = await this.u64();
    // Strings carry at least an 8-byte length prefix each.
    const size = elementType === 8 ? 8 : FIXED_SIZES[elementType];
    if (size === undefined) throw new Error("Unsupported GGUF array type.");
    if (count > this.remaining() / size) {
      throw new Error("GGUF array is larger than the file.");
    }
    if (elementType === 8) {
      this.spend(count);
      for (let index = 0; index < count; index += 1) {
        this.skip(await this.u64());
      }
      return null;
    }
    if (!keep || count > MAX_KEPT_ARRAY_ELEMENTS) {
      this.skip(size * count);
      return null;
    }
    this.spend(count);
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
  ".attention.key_length_swa",
  ".attention.value_length_swa",
  ".attention.shared_kv_layers",
  ".attention.recurrent_layers",
  ".full_attention_interval",
  ".nextn_predict_layers",
  ".ssm.conv_kernel",
  ".ssm.inner_size",
  ".ssm.state_size",
  ".ssm.group_count",
  ".decision.type",
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

interface KeySelection {
  /** Whether to keep a key's value. */
  readonly keep: (key: string) => boolean;
  /** Per-key string limit in bytes; longer strings are skipped. */
  readonly maxStringBytes?: (key: string) => number;
}

const GEOMETRY_KEYS: KeySelection = { keep: keepKey };

async function readKeyValues(
  path: string,
  maxWork?: number,
  selection: KeySelection = GEOMETRY_KEYS,
): Promise<Map<string, GgufValue>> {
  const reader = new GgufReader(Bun.file(path), maxWork);
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
    reader.spend(1);
    const key = (await reader.string(true)) ?? "";
    const type = await reader.u32();
    const keep = selection.keep(key);
    const value = await reader.value(
      type,
      keep,
      selection.maxStringBytes?.(key),
    );
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

/**
 * Per-layer integers from a scalar (repeated) or an array that must have one
 * entry per layer, like llama_model_loader::get_key_or_arr. Undefined when
 * absent, null when malformed.
 */
function layerValues(
  value: GgufValue | undefined,
  blockCount: number,
): number[] | null | undefined {
  if (value === undefined) return undefined;
  const valid = (entry: unknown): entry is number =>
    typeof entry === "number" &&
    Number.isSafeInteger(entry) &&
    entry >= 0 &&
    entry <= MAX_HEAD_COUNT;
  if (Array.isArray(value)) {
    return value.length === blockCount && value.every(valid) ? value : null;
  }
  return valid(value) ? new Array<number>(blockCount).fill(value) : null;
}

/** An optional integer in [min, max]: undefined when absent, null if invalid. */
function optionalInt(
  value: GgufValue | undefined,
  min: number,
  max: number,
): number | null | undefined {
  if (value === undefined) return undefined;
  return typeof value === "number" &&
    Number.isSafeInteger(value) &&
    value >= min &&
    value <= max
    ? value
    : null;
}

/**
 * Which layers use the sliding-window cache. Gemma 4 stores a per-layer
 * boolean pattern; other SWA architectures use a period (set_swa_pattern)
 * that a scalar `sliding_window_pattern` can override. Null if malformed.
 */
function swaLayerPredicate(
  pattern: GgufValue | undefined,
  blockCount: number,
  perLayer: boolean,
  defaultPeriod: number | null,
): ((layer: number) => boolean) | null {
  if (perLayer) {
    if (Array.isArray(pattern)) {
      return pattern.length === blockCount
        ? (layer) => pattern[layer] !== 0
        : null;
    }
    return () => false;
  }
  if (defaultPeriod === null) return () => false;
  const period =
    typeof pattern === "number" &&
    Number.isSafeInteger(pattern) &&
    pattern >= 0 &&
    pattern <= MAX_BLOCK_COUNT
      ? pattern
      : defaultPeriod;
  // period 0 means every layer is sliding-window.
  return (layer) => period === 0 || (layer + 1) % period !== 0;
}

/**
 * Recurrent layers of qwen3next/qwen35/qwen35moe and their FP32 conv/ssm state
 * (llama_hparams::n_embd_r/n_embd_s). An `attention.recurrent_layers` mask wins
 * over `full_attention_interval` (default 4).
 */
function hybridQwenRecurrence(
  values: Map<string, GgufValue>,
  architecture: string,
  blockCount: number,
  attentionLayers: number,
): { isRecurrent: (layer: number) => boolean; bytesPerSlot: number } | null {
  const get = (suffix: string) => values.get(`${architecture}.${suffix}`);
  const mask = get("attention.recurrent_layers");
  let isRecurrent: (layer: number) => boolean;
  if (mask !== undefined) {
    if (!Array.isArray(mask) || mask.length !== blockCount) return null;
    isRecurrent = (layer) => mask[layer] !== 0;
  } else {
    const interval = optionalInt(
      get("full_attention_interval"),
      1,
      MAX_BLOCK_COUNT,
    );
    if (interval === null) return null;
    const period = interval ?? HYBRID_QWEN_FULL_ATTENTION_INTERVAL;
    isRecurrent = (layer) => (layer + 1) % period !== 0;
  }
  const kernel = bounded(get("ssm.conv_kernel"), MAX_SSM_KERNEL);
  const inner = bounded(get("ssm.inner_size"), MAX_SSM_SIZE);
  const state = bounded(get("ssm.state_size"), MAX_SSM_SIZE);
  const groups = optionalInt(get("ssm.group_count"), 0, MAX_SSM_SIZE);
  if (!kernel || !inner || !state || groups === null || groups === undefined) {
    return null;
  }
  const recurrentOnly = (layer: number) =>
    layer < attentionLayers && isRecurrent(layer);
  let recurrentLayers = 0;
  for (let layer = 0; layer < attentionLayers; layer += 1) {
    if (recurrentOnly(layer)) recurrentLayers += 1;
  }
  const convElements = (kernel - 1) * (inner + 2 * groups * state);
  const stateElements = state * inner;
  return {
    isRecurrent: recurrentOnly,
    bytesPerSlot:
      recurrentLayers * (convElements + stateElements) * RECURRENT_STATE_BYTES,
  };
}

/** Derives which layers hold KV and how large their caches are per token. */
export function kvGeometryFromMetadata(
  values: Map<string, GgufValue>,
): LlmKvGeometry | null {
  const architecture = values.get("general.architecture");
  if (typeof architecture !== "string" || !architecture) return null;
  if (UNMODELLED_RECURRENT_ARCHITECTURES.has(architecture)) return null;
  const get = (suffix: string) => values.get(`${architecture}.${suffix}`);

  // Validate every untrusted size before it can drive a loop.
  const blockCount = bounded(get("block_count"), MAX_BLOCK_COUNT);
  if (!blockCount) return null;
  const headCounts = layerValues(get("attention.head_count"), blockCount);
  const kvHeadCounts = layerValues(get("attention.head_count_kv"), blockCount);
  if (!headCounts || kvHeadCounts === null) return null;
  const headsForLayer = (layer: number): number =>
    kvHeadCounts?.[layer] ?? headCounts[layer] ?? 0;

  // llama.cpp derives the fallback head length from layer 0's head count.
  const embeddingLength = bounded(get("embedding_length"), MAX_TOKENS);
  const firstHeadCount = headCounts[0] ?? 0;
  const fallbackLength =
    embeddingLength && firstHeadCount > 0
      ? Math.floor(embeddingLength / firstHeadCount)
      : null;
  const keyLength =
    bounded(get("attention.key_length"), MAX_HEAD_LENGTH) ?? fallbackLength;
  const declaredValueLength =
    bounded(get("attention.value_length"), MAX_HEAD_LENGTH) ??
    fallbackLength ??
    keyLength;
  if (!keyLength || !declaredValueLength || keyLength > MAX_HEAD_LENGTH) {
    return null;
  }

  // llama_hparams::is_mla(): both MLA lengths set. The cache then holds only
  // K (llama-kv-cache.cpp has_v = !is_mla), sized by the regular key length.
  const keyLengthMla = bounded(
    get("attention.key_length_mla"),
    MAX_HEAD_LENGTH,
  );
  const valueLengthMla = bounded(
    get("attention.value_length_mla"),
    MAX_HEAD_LENGTH,
  );
  const mla = keyLengthMla !== null && valueLengthMla !== null;

  const gemma4 = architecture === "gemma4";
  const hybridQwen = HYBRID_QWEN_ARCHITECTURES.has(architecture);
  const swaSpec = SWA_ARCHITECTURES[architecture];
  const slidingWindow =
    swaSpec || gemma4
      ? (bounded(get("attention.sliding_window"), MAX_TOKENS) ??
        swaSpec?.defaultWindow ??
        null)
      : null;

  let swaKeyLength = keyLength;
  let swaValueLength = declaredValueLength;
  if (gemma4) {
    // Gemma 4 requires the window and separate SWA head lengths.
    const swaKey = bounded(get("attention.key_length_swa"), MAX_HEAD_LENGTH);
    const swaValue = bounded(
      get("attention.value_length_swa"),
      MAX_HEAD_LENGTH,
    );
    if (slidingWindow === null || !swaKey || !swaValue) return null;
    swaKeyLength = swaKey;
    swaValueLength = swaValue;
  }

  // nextn (MTP) layers sit past n_layer() and hold no regular KV.
  const nextn = hybridQwen
    ? optionalInt(get("nextn_predict_layers"), 0, blockCount - 1)
    : 0;
  if (nextn === null) return null;
  const attentionLayers = blockCount - (nextn ?? 0);

  const isSwa = swaLayerPredicate(
    get("attention.sliding_window_pattern"),
    blockCount,
    gemma4,
    slidingWindow !== null ? (swaSpec?.period ?? 0) : null,
  );
  if (!isSwa) return null;

  // Gemma 3n/4 keep KV only in the first layers; later layers share them.
  let kvLayers = Math.min(
    attentionLayers,
    swaSpec?.kvLayers ?? attentionLayers,
  );
  if (gemma4) {
    const shared = optionalInt(
      get("attention.shared_kv_layers"),
      0,
      blockCount,
    );
    if (shared === null || blockCount - (shared ?? 0) < 1) return null;
    kvLayers = blockCount - (shared ?? 0);
  }

  let isRecurrent = (_layer: number): boolean => false;
  let recurrentBytesPerSlot = 0;
  if (hybridQwen) {
    const recurrence = hybridQwenRecurrence(
      values,
      architecture,
      blockCount,
      attentionLayers,
    );
    if (!recurrence) return null;
    isRecurrent = recurrence.isRecurrent;
    recurrentBytesPerSlot = recurrence.bytesPerSlot;
  }

  let fullKvHeads = 0;
  let swaKvHeads = 0;
  let anySwa = false;
  for (let layer = 0; layer < blockCount; layer += 1) {
    if (isSwa(layer)) anySwa = true;
  }
  for (let layer = 0; layer < kvLayers; layer += 1) {
    const heads = headsForLayer(layer);
    if (heads <= 0 || isRecurrent(layer)) continue;
    if (isSwa(layer)) swaKvHeads += heads;
    else fullKvHeads += heads;
  }

  // llama.cpp validates the quantized type against every layer, so check each
  // head dim that any layer kind can use, MLA included.
  const dims = [keyLength, declaredValueLength];
  if (anySwa) dims.push(swaKeyLength, swaValueLength);
  if (mla) dims.push(keyLengthMla, valueLengthMla);
  const q8Compatible = dims.every((dim) => dim % Q8_0_BLOCK_SIZE === 0);
  const decisionType = decisionTypeFromMetadata(values);
  if (decisionType) {
    fullKvHeads = 0;
    swaKvHeads = 0;
    recurrentBytesPerSlot = 0;
  }

  return Object.freeze({
    architecture,
    blockCount,
    fullKvHeads,
    swaKvHeads,
    slidingWindow,
    keyLength,
    valueLength: mla ? 0 : declaredValueLength,
    swaKeyLength,
    swaValueLength: mla ? 0 : swaValueLength,
    q8Compatible,
    recurrentBytesPerSlot,
    contextLength: bounded(get("context_length"), MAX_TOKENS),
    ...(decisionType ? { decisionType } : {}),
  });
}

/** The decision head type a decision GGUF declares, or null for other models. */
export function decisionTypeFromMetadata(
  values: Map<string, GgufValue>,
): string | null {
  const architecture = values.get("general.architecture");
  if (typeof architecture !== "string" || !architecture) return null;
  const value = values.get(`${architecture}.decision.type`);
  return typeof value === "string" && /^[a-z0-9_-]{1,32}$/.test(value)
    ? value
    : null;
}

/** Reads the GGUF training context independently of whether KV geometry is known. */
export function trainingContextLengthFromMetadata(
  values: Map<string, GgufValue>,
): number | null {
  const architecture = values.get("general.architecture");
  if (typeof architecture !== "string" || !architecture) return null;
  return bounded(values.get(`${architecture}.context_length`), MAX_TOKENS);
}

/** Reads only the GGUF header; returns null on any failure or missing metadata. */
export async function readLlmTrainingContextLength(
  path: string,
  options: { maxParseWork?: number } = {},
): Promise<number | null> {
  try {
    return trainingContextLengthFromMetadata(
      await readKeyValues(ggufMetadataPath(path), options.maxParseWork),
    );
  } catch {
    return null;
  }
}

/** Reads only the GGUF header; returns null on any failure. */
export async function readLlmKvGeometry(
  path: string,
  options: { maxParseWork?: number } = {},
): Promise<LlmKvGeometry | null> {
  try {
    return kvGeometryFromMetadata(
      await readKeyValues(ggufMetadataPath(path), options.maxParseWork),
    );
  } catch {
    return null;
  }
}

/** Chat templates run a few KB; this bounds what a hostile header can make us keep. */
const MAX_CHAT_TEMPLATE_BYTES = 256 * 1024;
const CHAT_TEMPLATE_KEY = "tokenizer.chat_template";

/** Header facts that say what a GGUF model or projector can do. */
export interface GgufCapabilityHeader {
  readonly architecture: string | null;
  readonly contextLength: number | null;
  /** llama.cpp pooling type id; embedding models carry one. */
  readonly poolingType: number | null;
  /** Routed experts per MoE layer; null for dense models. */
  readonly expertCount: number | null;
  /** The default template and any named variants (e.g. `.tool_use`), joined. */
  readonly chatTemplate: string | null;
  /** `{arch}.decision.type`; decision models answer typed questions only. */
  readonly decisionType: string | null;
  /** Projector (`clip`) encoder flags. */
  readonly hasVisionEncoder: boolean;
  readonly hasAudioEncoder: boolean;
}

function capabilityKey(key: string): boolean {
  return (
    key === "general.architecture" ||
    key === CHAT_TEMPLATE_KEY ||
    key.startsWith(`${CHAT_TEMPLATE_KEY}.`) ||
    key.endsWith(".context_length") ||
    key.endsWith(".pooling_type") ||
    key.endsWith(".expert_count") ||
    key.endsWith(".decision.type") ||
    key === "clip.has_vision_encoder" ||
    key === "clip.has_audio_encoder"
  );
}

const CAPABILITY_KEYS: KeySelection = {
  keep: capabilityKey,
  maxStringBytes: (key) =>
    key.startsWith(CHAT_TEMPLATE_KEY)
      ? MAX_CHAT_TEMPLATE_BYTES
      : MAX_KEPT_STRING_BYTES,
};

export function capabilityHeaderFromMetadata(
  values: Map<string, GgufValue>,
): GgufCapabilityHeader {
  const architecture = values.get("general.architecture");
  const arch =
    typeof architecture === "string" && architecture ? architecture : null;
  const pooling = arch ? values.get(`${arch}.pooling_type`) : undefined;
  const experts = arch ? values.get(`${arch}.expert_count`) : undefined;
  const templates = [...values.entries()]
    .filter(
      ([key, value]) =>
        (key === CHAT_TEMPLATE_KEY ||
          key.startsWith(`${CHAT_TEMPLATE_KEY}.`)) &&
        typeof value === "string" &&
        value.length > 0,
    )
    .map(([, value]) => value as string);
  return {
    architecture: arch,
    contextLength: arch
      ? bounded(values.get(`${arch}.context_length`), MAX_TOKENS)
      : null,
    poolingType:
      typeof pooling === "number" && Number.isSafeInteger(pooling)
        ? pooling
        : null,
    expertCount:
      typeof experts === "number" &&
      Number.isSafeInteger(experts) &&
      experts > 0
        ? experts
        : null,
    chatTemplate: templates.length > 0 ? templates.join("\n") : null,
    decisionType: decisionTypeFromMetadata(values),
    hasVisionEncoder: values.get("clip.has_vision_encoder") === true,
    hasAudioEncoder: values.get("clip.has_audio_encoder") === true,
  };
}

/** Reads only the GGUF header; returns null on any failure. */
export async function readGgufCapabilityHeader(
  path: string,
  options: { maxParseWork?: number } = {},
): Promise<GgufCapabilityHeader | null> {
  try {
    return capabilityHeaderFromMetadata(
      await readKeyValues(
        ggufMetadataPath(path),
        options.maxParseWork,
        CAPABILITY_KEYS,
      ),
    );
  } catch {
    return null;
  }
}

function pad(cells: number): number {
  return Math.ceil(cells / KV_CELL_PADDING) * KV_CELL_PADDING;
}

/**
 * Estimated cache bytes for a total context budget split across slots, with
 * the cell padding llama.cpp applies to the context and each stream, plus the
 * per-slot recurrent state of hybrid models.
 */
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
  const slots = Math.max(1, input.slots);
  const cellsPerStream = pad(Math.floor(pad(input.ctxTokens) / slots));
  const full =
    slots *
    cellsPerStream *
    geometry.fullKvHeads *
    (geometry.keyLength * bytesK + geometry.valueLength * bytesV);
  const swaCellsPerStream =
    geometry.slidingWindow === null
      ? 0
      : pad(
          Math.min(cellsPerStream, geometry.slidingWindow + SWA_UBATCH_TOKENS),
        );
  const swa =
    slots *
    swaCellsPerStream *
    geometry.swaKvHeads *
    (geometry.swaKeyLength * bytesK + geometry.swaValueLength * bytesV);
  return Math.ceil(full + swa + slots * geometry.recurrentBytesPerSlot);
}
