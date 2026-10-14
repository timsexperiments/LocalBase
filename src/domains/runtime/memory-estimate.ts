/**
 * First-principles memory estimates for every runtime LocalBase launches.
 *
 * Every estimate is a sum of named terms (weights, caches, compute buffers,
 * fixed overheads) so the derivation is inspectable and testable. Formulas
 * mirror how the pinned backends allocate memory:
 *
 * - llama.cpp (llama-server): weights are mmapped/offloaded as stored; the KV
 *   cache is `cells x layers x heads x (k + v) x bytes(type)` with the cell
 *   padding and sliding-window sizing in `kvCacheBytes` (src/llama-kv-cache.cpp);
 *   the compute buffer is reserved for the worst-case ubatch
 *   (src/llama-context.cpp graph_reserve) and is dominated by the F32 logits
 *   of one ubatch (`n_ubatch x n_vocab x 4`) plus a handful of F32
 *   `n_ubatch x n_embd` activations. Without flash attention the F32 KQ score
 *   matrix (`n_ubatch x n_ctx x n_head x 4`, src/llama-graph.cpp
 *   build_attn_mha) is added on top.
 * - whisper.cpp: decoder self-attention KV over n_text_ctx = 448 tokens, the
 *   cross-attention KV over n_audio_ctx = 1500 encoder frames, and the encoder
 *   attention score matrix (`1500^2 x n_head x 4`) (src/whisper.cpp
 *   whisper_kv_cache_init / whisper_build_graph_encoder).
 * - stable-diffusion.cpp (sd-server): every model component is a separate
 *   ggml context, so components sum per memory pool; `--clip-on-cpu` and
 *   `--offload-to-cpu` move components to host memory (the latter streams each
 *   stage to the GPU only while it runs). Activations are the attention
 *   working set over latent tokens (without flash attention the F32 score
 *   matrix `T^2 x heads x 4`; with it only `O(T x D)` activations) plus the
 *   VAE decoder working set at full output resolution.
 *
 * Constants that are allowances rather than exact backend allocations are
 * named as such. Confidence is "derived" when the model geometry is known
 * (GGUF header or the shape table below) and "estimated" when it falls back
 * to a size-based shape or allowance. A "measured" demand recorded from a
 * real run (memory-measurements.ts) replaces the estimate through
 * `preferMeasured` when the launch shape matches the measured one.
 */
import type {
  ImageRuntimeProfile,
  ModelSpec,
  VideoRuntimeProfile,
} from "../../catalog";
import {
  kvCacheBytes,
  type KvCacheType,
  type LlmKvGeometry,
} from "./gguf-metadata";
import { gibibyte, type RuntimeMemoryDemand } from "./memory-safety";
import {
  findLlmMeasurement,
  findModelMeasurement,
  findVideoMeasurement,
  type MemoryMeasurement,
} from "./memory-measurements";

const mebibyte = 1024 * 1024;

/** One estimate plus the named terms it was summed from (bytes). */
export type MemoryEstimate = Readonly<{
  demand: RuntimeMemoryDemand;
  terms: Readonly<Record<string, number>>;
}>;

/**
 * Peak demand observed from a real run of the same model and launch shape.
 * Built from memory-measurements.ts records by `measuredDemand`.
 */
export type MeasuredMemoryDemand = Readonly<{
  unifiedBytes: number;
  hostBytes: number;
  acceleratorBytes: number;
}>;

/** A measured demand, when present, replaces the estimate. */
export function preferMeasured(
  estimate: MemoryEstimate,
  measured?: MeasuredMemoryDemand | null,
): RuntimeMemoryDemand {
  if (!measured) return estimate.demand;
  return Object.freeze({ ...measured, confidence: "measured" });
}

/**
 * Planning demand from a measured record.
 * - accelerator = backend GPU buffers + allowance for what the backend does
 *   not report (Metal context, pipeline cache, allocator slack), optionally
 *   rounded like the estimate it replaces
 * - host = peak RSS, at least the backend host buffers and the on-disk
 *   weights (mmapped pages the run never touched still count)
 * - unified = the larger of accelerator + host buffers, peak RSS and the
 *   system-wide free-memory drop, plus any prompt cache configured for the launch
 */
export function measuredDemand(
  record: MemoryMeasurement,
  options: {
    artifactBytes: number;
    acceleratorOverheadBytes?: number;
    acceleratorRoundingBytes?: number;
    promptCacheBytes?: number;
  },
): MeasuredMemoryDemand {
  const overhead =
    options.acceleratorOverheadBytes ?? ACCELERATOR_OVERHEAD_BYTES;
  const promptCache = options.promptCacheBytes ?? 0;
  const accelerator = record.backendAcceleratorBytes + overhead;
  const host = Math.max(
    record.peakRssBytes,
    record.hostBufferBytes,
    options.artifactBytes,
  );
  return Object.freeze({
    acceleratorBytes: options.acceleratorRoundingBytes
      ? roundUp(accelerator, options.acceleratorRoundingBytes)
      : accelerator,
    hostBytes: host + promptCache,
    unifiedBytes:
      Math.max(
        accelerator + record.hostBufferBytes,
        host,
        record.systemDeltaBytes ?? 0,
      ) + promptCache,
  });
}

/**
 * Host-side runtime overhead for every backend process: executable, backend
 * libraries, allocator slack and HTTP server state. Allowance.
 */
export const RUNTIME_HOST_OVERHEAD_BYTES = 512 * mebibyte;
/**
 * Accelerator-side fixed overhead: backend context, kernel/pipeline caches and
 * allocator fragmentation (CUDA context alone is ~300-500 MiB). Allowance.
 */
export const ACCELERATOR_OVERHEAD_BYTES = 512 * mebibyte;

// ---------------------------------------------------------------------------
// minVramGb tiers
// ---------------------------------------------------------------------------

/**
 * Hardware tiers minVramGb rounds up to. 8 GiB and above are common GPU and
 * unified-memory sizes; 1/2/4/6 extend the scale for small models so tiny
 * runtimes do not all claim an 8 GiB machine.
 */
export const MIN_VRAM_TIERS_GB = [
  1, 2, 4, 6, 8, 12, 16, 24, 32, 48, 64, 96, 128,
] as const;
/** Safety margin over the estimate: max(512 MiB, 10%). */
export const MIN_VRAM_MARGIN_BYTES = 512 * mebibyte;
export const MIN_VRAM_MARGIN_RATIO = 0.1;

/** Smallest hardware tier that holds `bytes` plus the safety margin. */
export function minVramTierGb(bytes: number): number {
  const required =
    bytes + Math.max(MIN_VRAM_MARGIN_BYTES, bytes * MIN_VRAM_MARGIN_RATIO);
  const requiredGb = required / gibibyte;
  for (const tier of MIN_VRAM_TIERS_GB) {
    if (requiredGb <= tier) return tier;
  }
  return Math.ceil(requiredGb / 64) * 64;
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

function freezeEstimate(
  demand: Omit<RuntimeMemoryDemand, "confidence">,
  confidence: RuntimeMemoryDemand["confidence"],
  terms: Record<string, number>,
): MemoryEstimate {
  return Object.freeze({
    demand: Object.freeze({
      unifiedBytes: Math.ceil(demand.unifiedBytes),
      hostBytes: Math.ceil(demand.hostBytes),
      acceleratorBytes: Math.ceil(demand.acceleratorBytes),
      confidence,
    }),
    terms: Object.freeze(
      Object.fromEntries(
        Object.entries(terms).map(([key, value]) => [key, Math.ceil(value)]),
      ),
    ),
  });
}

function artifactBytesByName(spec: ModelSpec): Map<string, number> {
  return new Map(
    spec.artifacts.map((artifact) => [
      artifact.filename,
      artifact.expectedSizeBytes ?? 0,
    ]),
  );
}

/** Sum of all catalog artifact sizes (every shard, projector, encoder). */
export function totalArtifactBytes(spec: ModelSpec): number {
  return spec.artifacts.reduce(
    (sum, artifact) => sum + (artifact.expectedSizeBytes ?? 0),
    0,
  );
}

/**
 * Last-resort demand when nothing but the artifact size is known (local or
 * test models without a catalog entry): weights on the accelerator plus the
 * fixed host overhead.
 */
export function genericArtifactDemand(artifactBytes: number): MemoryEstimate {
  return freezeEstimate(
    {
      unifiedBytes: artifactBytes + RUNTIME_HOST_OVERHEAD_BYTES,
      hostBytes: artifactBytes + RUNTIME_HOST_OVERHEAD_BYTES,
      acceleratorBytes: artifactBytes,
    },
    "estimated",
    { weights: artifactBytes, hostOverhead: RUNTIME_HOST_OVERHEAD_BYTES },
  );
}

// ---------------------------------------------------------------------------
// LLM (llama-server)
// ---------------------------------------------------------------------------

/** Physical batch llama-server reserves its compute graph for (`-ub` default). */
export const LLAMA_UBATCH_TOKENS = 512;
/** F32 `n_ubatch x n_embd` activations live at the compute-buffer peak. */
const LLAMA_LIVE_EMBD_ACTIVATIONS = 4;
/**
 * Per-slot sampler state: llama_token_data (id, logit, p = 12 B) candidates
 * plus a logits copy per vocab entry (~16 B/token), and a fixed 16 MiB of
 * slot bookkeeping (prompt tokens, speculative/n-gram state). Allowance.
 */
const LLAMA_SLOT_BYTES_PER_VOCAB = 16;
const LLAMA_SLOT_FIXED_BYTES = 16 * mebibyte;
/** Context used to size minVramGb: one slot, 8K tokens, no prompt cache. */
export const MIN_VRAM_LLM_CONTEXT_TOKENS = 8192;

/** Model dimensions the KV cache and compute buffer depend on. */
export type LlmShape = Readonly<{
  geometry: LlmKvGeometry;
  queryHeads: number;
  embeddingLength: number;
  vocabSize: number;
}>;

type DenseShape = {
  layers: number;
  queryHeads: number;
  kvHeads: number;
  headDim: number;
  embd: number;
  vocab: number;
  architecture?: string;
  /** Sliding-window layers and their window (Gemma 3, gpt-oss). */
  swa?: { layers: number; window: number };
};

function denseGeometry(shape: DenseShape): LlmKvGeometry {
  const swaLayers = shape.swa?.layers ?? 0;
  const fullLayers = shape.layers - swaLayers;
  return Object.freeze({
    architecture: shape.architecture ?? "llama",
    blockCount: shape.layers,
    fullKvHeads: fullLayers * shape.kvHeads,
    swaKvHeads: swaLayers * shape.kvHeads,
    slidingWindow: shape.swa ? shape.swa.window : null,
    keyLength: shape.headDim,
    valueLength: shape.headDim,
    swaKeyLength: shape.swa ? shape.headDim : 0,
    swaValueLength: shape.swa ? shape.headDim : 0,
    q8Compatible: shape.headDim % 32 === 0,
    recurrentBytesPerSlot: 0,
    contextLength: null,
  });
}

function dense(shape: DenseShape): LlmShape {
  return Object.freeze({
    geometry: denseGeometry(shape),
    queryHeads: shape.queryHeads,
    embeddingLength: shape.embd,
    vocabSize: shape.vocab,
  });
}

/*
 * Published config.json shapes (Hugging Face model cards; the GGUF headers of
 * the catalog files carry the same values). layers / q heads / kv heads /
 * head dim / hidden / vocab.
 */
const QWEN3_0_6B = dense({
  layers: 28,
  queryHeads: 16,
  kvHeads: 8,
  headDim: 128,
  embd: 1024,
  vocab: 151669,
});
/**
 * Qwen3-TTS 1.7B talker (28 layers x 8 KV heads x 128; the measured f16 32K
 * KV buffer is exactly 3584 MiB). Used by the TTS estimate, not the LLM table.
 */
const QWEN3_TTS_1_7B_TALKER = dense({
  layers: 28,
  queryHeads: 16,
  kvHeads: 8,
  headDim: 128,
  embd: 2048,
  vocab: 151669,
});
const QWEN3_4B = dense({
  layers: 36,
  queryHeads: 32,
  kvHeads: 8,
  headDim: 128,
  embd: 2560,
  vocab: 151669,
});
const QWEN3_8B = dense({
  layers: 36,
  queryHeads: 32,
  kvHeads: 8,
  headDim: 128,
  embd: 4096,
  vocab: 151669,
});
const QWEN25_1_5B = dense({
  layers: 28,
  queryHeads: 12,
  kvHeads: 2,
  headDim: 128,
  embd: 1536,
  vocab: 151936,
});
const QWEN25_3B = dense({
  layers: 36,
  queryHeads: 16,
  kvHeads: 2,
  headDim: 128,
  embd: 2048,
  vocab: 151936,
});
const QWEN25_7B = dense({
  layers: 28,
  queryHeads: 28,
  kvHeads: 4,
  headDim: 128,
  embd: 3584,
  vocab: 152064,
});
const QWEN25_14B = dense({
  layers: 48,
  queryHeads: 40,
  kvHeads: 8,
  headDim: 128,
  embd: 5120,
  vocab: 152064,
});
const QWEN25_32B = dense({
  layers: 64,
  queryHeads: 40,
  kvHeads: 8,
  headDim: 128,
  embd: 5120,
  vocab: 152064,
});
const QWEN25_72B = dense({
  layers: 80,
  queryHeads: 64,
  kvHeads: 8,
  headDim: 128,
  embd: 8192,
  vocab: 152064,
});
const LLAMA32_1B = dense({
  layers: 16,
  queryHeads: 32,
  kvHeads: 8,
  headDim: 64,
  embd: 2048,
  vocab: 128256,
});
const LLAMA32_3B = dense({
  layers: 28,
  queryHeads: 24,
  kvHeads: 8,
  headDim: 128,
  embd: 3072,
  vocab: 128256,
});
const LLAMA31_8B = dense({
  layers: 32,
  queryHeads: 32,
  kvHeads: 8,
  headDim: 128,
  embd: 4096,
  vocab: 128256,
});
const LLAMA_70B = dense({
  layers: 80,
  queryHeads: 64,
  kvHeads: 8,
  headDim: 128,
  embd: 8192,
  vocab: 128256,
});
const MISTRAL_40L_5120 = dense({
  layers: 40,
  queryHeads: 32,
  kvHeads: 8,
  headDim: 128,
  embd: 5120,
  vocab: 131072,
});

/**
 * Qwen3-Coder-Next ("qwen3next"): 48 layers, every 4th full attention
 * (12 layers x 2 KV heads x 256), the other 36 Gated DeltaNet layers hold F32
 * recurrent state per slot: ssm 128 x 128 x 32 heads plus conv
 * (kernel 4 - 1) x 8192 channels, x 4 bytes.
 */
const QWEN3_NEXT_RECURRENT_BYTES_PER_SLOT =
  36 * (128 * 128 * 32 * 4 + 3 * 8192 * 4);
const QWEN3_CODER_NEXT: LlmShape = Object.freeze({
  geometry: Object.freeze({
    ...denseGeometry({
      layers: 48,
      queryHeads: 16,
      kvHeads: 0,
      headDim: 256,
      embd: 2048,
      vocab: 151936,
      architecture: "qwen3next",
    }),
    fullKvHeads: 12 * 2,
    recurrentBytesPerSlot: QWEN3_NEXT_RECURRENT_BYTES_PER_SLOT,
  }),
  queryHeads: 16,
  embeddingLength: 2048,
  vocabSize: 151936,
});

/**
 * DeepSeek-Coder-V2-Lite uses MLA: llama.cpp caches one compressed
 * latent per layer (kv_lora_rank 512 + rope dim 64 = 576), K only.
 */
const DEEPSEEK_V2_LITE: LlmShape = Object.freeze({
  geometry: Object.freeze({
    architecture: "deepseek2",
    blockCount: 27,
    fullKvHeads: 27,
    swaKvHeads: 0,
    slidingWindow: null,
    keyLength: 576,
    valueLength: 0,
    swaKeyLength: 0,
    swaValueLength: 0,
    q8Compatible: true,
    recurrentBytesPerSlot: 0,
    contextLength: null,
  }),
  queryHeads: 16,
  embeddingLength: 2048,
  vocabSize: 102400,
});

/**
 * Gemma 3 interleaves 5 sliding-window layers per full layer
 * (llama.cpp swa_period 6); gpt-oss alternates (period 2).
 */
const LLM_SHAPES: Readonly<Record<string, LlmShape>> = {
  "qwen3-embedding-0.6b-q8_0": QWEN3_0_6B,
  "qwen3-embedding-4b-q4_k_m": QWEN3_4B,
  "qwen3-embedding-4b-q8_0": QWEN3_4B,
  "qwen3-embedding-8b-q4_k_m": QWEN3_8B,
  "qwen3-embedding-8b-q8_0": QWEN3_8B,
  "qwen2.5-coder-1.5b-instruct-q4_k_m": QWEN25_1_5B,
  "qwen2.5-coder-3b-instruct-q4_k_m": QWEN25_3B,
  "qwen2.5-coder-7b-instruct-q4_k_m": QWEN25_7B,
  "qwen2.5-coder-14b-instruct-q4_k_m": QWEN25_14B,
  "qwen2.5-coder-32b-instruct-q4_k_m": QWEN25_32B,
  "deepseek-r1-distill-qwen-14b-q4_k_m": QWEN25_14B,
  "deepseek-r1-distill-qwen-32b-q4_k_m": QWEN25_32B,
  "qwen2.5-72b-instruct-q4_k_m": QWEN25_72B,
  "qwen3-coder-next-q4_k_m": QWEN3_CODER_NEXT,
  "llama-3.2-1b-instruct-q4_k_m": LLAMA32_1B,
  "llama-3.2-3b-instruct-q4_k_m": LLAMA32_3B,
  "llama-3.1-8b-instruct-q4_k_m": LLAMA31_8B,
  "deepseek-r1-distill-llama-8b-q4_k_m": LLAMA31_8B,
  "llama-3.3-70b-instruct-q4_k_m": LLAMA_70B,
  "deepseek-r1-distill-llama-70b-q4_k_m": LLAMA_70B,
  "deepseek-coder-6.7b-instruct-q4_k_m": dense({
    layers: 32,
    queryHeads: 32,
    kvHeads: 32,
    headDim: 128,
    embd: 4096,
    vocab: 32256,
  }),
  "deepseek-coder-v2-lite-instruct-q4_k_m": DEEPSEEK_V2_LITE,
  "deepseek-coder-33b-instruct-q4_k_m": dense({
    layers: 62,
    queryHeads: 56,
    kvHeads: 8,
    headDim: 128,
    embd: 7168,
    vocab: 32256,
  }),
  "gemma-3-1b-it-q4_k_m": dense({
    layers: 26,
    queryHeads: 4,
    kvHeads: 1,
    headDim: 256,
    embd: 1152,
    vocab: 262144,
    architecture: "gemma3",
    swa: { layers: 22, window: 512 },
  }),
  "gemma-3-4b-it-q4_k_m": dense({
    layers: 34,
    queryHeads: 8,
    kvHeads: 4,
    headDim: 256,
    embd: 2560,
    vocab: 262208,
    architecture: "gemma3",
    swa: { layers: 29, window: 1024 },
  }),
  "gemma-3-12b-it-q4_k_m": dense({
    layers: 48,
    queryHeads: 16,
    kvHeads: 8,
    headDim: 256,
    embd: 3840,
    vocab: 262208,
    architecture: "gemma3",
    swa: { layers: 40, window: 1024 },
  }),
  "gemma-3-27b-it-q4_k_m": dense({
    layers: 62,
    queryHeads: 32,
    kvHeads: 16,
    headDim: 128,
    embd: 5376,
    vocab: 262208,
    architecture: "gemma3",
    swa: { layers: 52, window: 1024 },
  }),
  "phi-4-q4_k_m": dense({
    layers: 40,
    queryHeads: 40,
    kvHeads: 10,
    headDim: 128,
    embd: 5120,
    vocab: 100352,
  }),
  "phi-3.5-mini-instruct-q4_k_m": dense({
    layers: 32,
    queryHeads: 32,
    kvHeads: 32,
    headDim: 96,
    embd: 3072,
    vocab: 32064,
  }),
  "phi-3.5-moe-instruct-q4_k_m": dense({
    layers: 32,
    queryHeads: 32,
    kvHeads: 8,
    headDim: 128,
    embd: 4096,
    vocab: 32064,
  }),
  "gpt-oss-20b-q4_k_m": dense({
    layers: 24,
    queryHeads: 64,
    kvHeads: 8,
    headDim: 64,
    embd: 2880,
    vocab: 201088,
    architecture: "gpt-oss",
    swa: { layers: 12, window: 128 },
  }),
  // The catalog entry ships the Falcon3-10B-Instruct file.
  "falcon-2-11b-instruct-q4_k_m": dense({
    layers: 40,
    queryHeads: 12,
    kvHeads: 4,
    headDim: 256,
    embd: 3072,
    vocab: 131072,
  }),
  "codestral-22b-instruct-q4_k_m": dense({
    layers: 56,
    queryHeads: 48,
    kvHeads: 8,
    headDim: 128,
    embd: 6144,
    vocab: 32768,
  }),
  "mistral-nemo-12b-instruct-q4_k_m": MISTRAL_40L_5120,
  "devstral-small-2-24b-instruct-q4_k_m": MISTRAL_40L_5120,
  "mistral-small-3.2-24b-instruct-q4_k_m": MISTRAL_40L_5120,
};

/**
 * Catalog LLMs without a published shape in the table (newer hybrid or
 * unreleased-config models). They use the size-based fallback shape and stay
 * "estimated" until a GGUF header supplies real geometry at launch.
 */
export const LLM_FALLBACK_SHAPE_MODELS: ReadonlySet<string> = new Set([
  "qwen3.5-0.8b-q4_k_m",
  "qwen3.5-0.8b-q8_0",
  "qwen3.5-2b-q4_k_m",
  "qwen3.5-2b-q8_0",
  "qwen3.5-4b-q4_k_m",
  "qwen3.5-9b-q4_k_m",
  "qwen3.5-27b-q4_k_m",
  "qwen3.8-27b-q4_k_m",
  "ministral-3-14b-instruct-q4_k_m",
]);

export function hasPublishedLlmShape(modelId: string): boolean {
  return modelId in LLM_SHAPES;
}

/** Bits per weight of common GGUF quants (llama.cpp quantize --help). */
const QUANT_BITS_PER_WEIGHT: Readonly<Record<string, number>> = {
  Q4_0: 4.55,
  Q4_K_M: 4.89,
  Q5_0: 5.54,
  Q5_K_M: 5.69,
  Q6_K: 6.56,
  Q8_0: 8.5,
  F16: 16,
  BF16: 16,
};

/** Parameter count in billions from a catalog size string ("80B-A3B" -> 80). */
function parameterBillions(
  size: string | undefined,
  weightsBytes: number,
  quant: string | undefined,
): number {
  const match = /(\d+(?:\.\d+)?)\s*B/i.exec(size ?? "");
  if (match) return Number(match[1]);
  const bpw = QUANT_BITS_PER_WEIGHT[(quant ?? "").toUpperCase()] ?? 4.89;
  return (weightsBytes * 8) / bpw / 1e9;
}

/**
 * Size-based shape when geometry is unknown. Transformer scaling: depth grows
 * roughly as P^0.4 (Kaplan et al. 2020 shape fits; 0.5B ~ 24 layers, 70B ~
 * 80 layers), hidden size from P ~ 12 L d^2, GQA with 8 KV heads of 128 dims
 * (the modern default), and the Qwen vocabulary (152064) as a large-vocab
 * upper bound. Conservative: hybrid models hold less KV than this assumes.
 */
export function fallbackLlmShape(parametersB: number): LlmShape {
  const parameters = Math.max(0.1, parametersB) * 1e9;
  const layers = Math.ceil(16 * Math.max(0.1, parametersB) ** 0.4);
  const embd = Math.round(Math.sqrt(parameters / (12 * layers)));
  return dense({
    layers,
    queryHeads: Math.max(1, Math.round(embd / 128)),
    kvHeads: 8,
    headDim: 128,
    embd,
    vocab: 152064,
  });
}

export type ResolvedLlmShape = Readonly<{
  shape: LlmShape;
  source: "gguf" | "table" | "fallback";
}>;

/**
 * Shape for a model: a GGUF header's KV geometry wins (it describes the exact
 * file), then the published table, then the size-based fallback.
 */
export function resolveLlmShape(input: {
  modelId?: string;
  size?: string;
  quant?: string;
  weightsBytes: number;
  geometry?: LlmKvGeometry | null;
}): ResolvedLlmShape {
  const table = input.modelId ? LLM_SHAPES[input.modelId] : undefined;
  const base =
    table ??
    fallbackLlmShape(
      parameterBillions(input.size, input.weightsBytes, input.quant),
    );
  if (input.geometry) {
    return Object.freeze({
      shape: Object.freeze({ ...base, geometry: input.geometry }),
      source: "gguf",
    });
  }
  return Object.freeze({
    shape: base,
    source: table ? "table" : "fallback",
  });
}

/**
 * llama-server demand for one launch shape.
 *
 * - accelerator = weights + KV + compute buffer + accelerator overhead
 * - host        = weights (mmap) + host overhead + KV + prompt cache + slots
 *   (host holds the KV when layers stay on the CPU and mirrors weights for
 *   mmap; the prompt cache and slot sampler state are host-only)
 * - unified     = one copy of everything: weights + KV + compute + slots +
 *   prompt cache + overhead
 */
export function estimateLlmMemory(input: {
  weightsBytes: number;
  shape: ResolvedLlmShape;
  ctxTokens: number;
  slots: number;
  cacheTypeK: KvCacheType;
  cacheTypeV: KvCacheType;
  promptCacheBytes: number;
  /** llama-server runs with flash attention on/auto; false adds the KQ matrix. */
  flashAttention?: boolean;
}): MemoryEstimate {
  const { shape } = input.shape;
  const slots = Math.max(1, input.slots);
  const kv = kvCacheBytes(shape.geometry, {
    ctxTokens: input.ctxTokens,
    slots,
    cacheTypeK: input.cacheTypeK,
    cacheTypeV: input.cacheTypeV,
  });
  const attentionScores =
    input.flashAttention === false
      ? LLAMA_UBATCH_TOKENS * input.ctxTokens * shape.queryHeads * 4
      : 0;
  const compute =
    LLAMA_UBATCH_TOKENS *
      4 *
      (shape.vocabSize + LLAMA_LIVE_EMBD_ACTIVATIONS * shape.embeddingLength) +
    attentionScores;
  const slotState =
    slots *
    (shape.vocabSize * LLAMA_SLOT_BYTES_PER_VOCAB + LLAMA_SLOT_FIXED_BYTES);
  const w = input.weightsBytes;
  return freezeEstimate(
    {
      acceleratorBytes: w + kv + compute + ACCELERATOR_OVERHEAD_BYTES,
      hostBytes:
        w +
        RUNTIME_HOST_OVERHEAD_BYTES +
        kv +
        input.promptCacheBytes +
        slotState,
      unifiedBytes:
        w +
        kv +
        compute +
        slotState +
        input.promptCacheBytes +
        RUNTIME_HOST_OVERHEAD_BYTES,
    },
    input.shape.source === "fallback" ? "estimated" : "derived",
    {
      weights: w,
      kvCache: kv,
      compute,
      slotState,
      promptCache: input.promptCacheBytes,
      acceleratorOverhead: ACCELERATOR_OVERHEAD_BYTES,
      hostOverhead: RUNTIME_HOST_OVERHEAD_BYTES,
    },
  );
}

/** KV type the launcher picks: f16 for embeddings or q8-incompatible heads. */
export function llmKvCacheType(
  embedding: boolean,
  geometry: LlmKvGeometry | null,
): KvCacheType {
  return embedding || !geometry?.q8Compatible ? "f16" : "q8_0";
}

// ---------------------------------------------------------------------------
// STT (whisper-server)
// ---------------------------------------------------------------------------

/** Whisper text context (n_text_ctx) and encoder frames (n_audio_ctx). */
const WHISPER_TEXT_CTX = 448;
const WHISPER_AUDIO_CTX = 1500;
/** Decoder sequences kept for sampling (whisper.cpp default best_of = 5). */
const WHISPER_DECODERS = 5;
/** F32 encoder activations live at the encoder peak, per frame x state. */
const WHISPER_LIVE_ENCODER_ACTIVATIONS = 10;
const WHISPER_ACCELERATOR_OVERHEAD_BYTES = 128 * mebibyte;

type WhisperShape = { decoderLayers: number; heads: number; state: number };

/** OpenAI Whisper model dims (whisper/__init__.py _MODELS dims). */
function whisperShape(size: string, files: readonly string[]): WhisperShape {
  const name = size.toLowerCase().replace(/\.en$/, "");
  switch (name) {
    case "tiny":
      return { decoderLayers: 4, heads: 6, state: 384 };
    case "base":
      return { decoderLayers: 6, heads: 8, state: 512 };
    case "small":
      return { decoderLayers: 12, heads: 12, state: 768 };
    case "medium":
      return { decoderLayers: 24, heads: 16, state: 1024 };
    default:
      // large-v3-turbo keeps the large encoder but only 4 decoder layers.
      return {
        decoderLayers: files.some((file) => /turbo/i.test(file)) ? 4 : 32,
        heads: 20,
        state: 1280,
      };
  }
}

/**
 * whisper-server demand:
 * - self KV  = decoders x layers x 448 x state x (K + V) x f16
 * - cross KV = layers x 1500 x state x (K + V) x f16
 * - compute  = encoder F32 scores 1500^2 x heads x 4 + live activations
 */
export function estimateSttMemory(spec: ModelSpec): MemoryEstimate {
  const weights = totalArtifactBytes(spec);
  const shape = whisperShape(
    spec.size,
    spec.artifacts.map((artifact) => artifact.filename),
  );
  const selfKv =
    WHISPER_DECODERS *
    shape.decoderLayers *
    WHISPER_TEXT_CTX *
    shape.state *
    2 *
    2;
  const crossKv = shape.decoderLayers * WHISPER_AUDIO_CTX * shape.state * 2 * 2;
  const compute =
    WHISPER_AUDIO_CTX ** 2 * shape.heads * 4 +
    WHISPER_AUDIO_CTX * shape.state * 4 * WHISPER_LIVE_ENCODER_ACTIVATIONS;
  const kv = selfKv + crossKv;
  return freezeEstimate(
    {
      acceleratorBytes:
        weights + kv + compute + WHISPER_ACCELERATOR_OVERHEAD_BYTES,
      hostBytes: weights + RUNTIME_HOST_OVERHEAD_BYTES,
      unifiedBytes: weights + kv + compute + RUNTIME_HOST_OVERHEAD_BYTES,
    },
    "derived",
    {
      weights,
      selfKv,
      crossKv,
      compute,
      acceleratorOverhead: WHISPER_ACCELERATOR_OVERHEAD_BYTES,
      hostOverhead: RUNTIME_HOST_OVERHEAD_BYTES,
    },
  );
}

// ---------------------------------------------------------------------------
// TTS
// ---------------------------------------------------------------------------

/**
 * llama-tts runs without `-c`, so the talker allocates its full trained
 * context (measured n_ctx 32768 for Qwen3-TTS 1.7B).
 */
const TTS_CONTEXT_TOKENS = 32768;
/** Audio codec/vocoder decode working set. Allowance. */
const TTS_CODEC_BYTES = 1 * gibibyte;
/** llama-tts loads the projector twice (measured 2 x 426 MiB Metal buffers). */
const TTS_PROJECTOR_COPIES = 2;
/** Talker shapes by model; other TTS models use the size-based fallback. */
const TTS_TALKER_SHAPES: Readonly<Record<string, LlmShape>> = {
  "qwen3-tts-1.7b-base-q4_k_m": QWEN3_TTS_1_7B_TALKER,
};

/**
 * Qwen3-TTS runs a llama.cpp talker LLM plus the multimodal projector and the
 * audio codec. The talker uses its published shape (else the fallback LLM
 * shape at its parameter count) with an f16 32K context, and the projector is
 * loaded twice; every term is held in one process, so all pools
 * carry the full sum.
 */
function talkerShape(spec: ModelSpec, weightsBytes: number): ResolvedLlmShape {
  const published = TTS_TALKER_SHAPES[spec.modelId];
  return published
    ? Object.freeze({ shape: published, source: "table" })
    : resolveLlmShape({ size: spec.size, quant: spec.quant, weightsBytes });
}

export function estimateTtsMemory(spec: ModelSpec): MemoryEstimate {
  const sizes = artifactBytesByName(spec);
  const projector = spec.ttsRuntime
    ? (sizes.get(spec.ttsRuntime.projectorArtifactFilename) ?? 0)
    : 0;
  const weights = totalArtifactBytes(spec) - projector;
  const talker = estimateLlmMemory({
    weightsBytes: weights,
    shape: talkerShape(spec, weights),
    ctxTokens: TTS_CONTEXT_TOKENS,
    slots: 1,
    cacheTypeK: "f16",
    cacheTypeV: "f16",
    promptCacheBytes: 0,
  });
  const terms = {
    weights,
    projector: projector * TTS_PROJECTOR_COPIES,
    kvCache: talker.terms.kvCache ?? 0,
    compute: talker.terms.compute ?? 0,
    codec: TTS_CODEC_BYTES,
    overhead: RUNTIME_HOST_OVERHEAD_BYTES,
  };
  const total = Object.values(terms).reduce((sum, value) => sum + value, 0);
  return freezeEstimate(
    { acceleratorBytes: total, hostBytes: total, unifiedBytes: total },
    "estimated",
    terms,
  );
}

// ---------------------------------------------------------------------------
// Diffusion shared terms
// ---------------------------------------------------------------------------

/**
 * F32 per-token activations at the transformer/UNet attention peak: q, k, v,
 * attention output, residual and the 4x MLP hidden ~ 12 tensors x D x 4 B.
 */
const DIFFUSION_ACTIVATION_BYTES_PER_TOKEN_DIM = 48;
/**
 * Live F32 feature maps at the VAE decoder's full-resolution stage. Measured
 * 2026-10-13: the sd.cpp VAE compute buffer is 13.0 maps of 128 channels both for
 * SD1.5 at 512^2 (1664 MiB) and for Flux.2 at 1024^2 (6658 MiB).
 */
const VAE_LIVE_FEATURE_MAPS = 13;
/** Channel width of the SD/SDXL/Flux VAE decoder's last stage. */
const IMAGE_VAE_CHANNELS = 128;
/** Text encoder (CLIP/T5/Qwen3) compute working set. Allowance. */
const TEXT_ENCODER_COMPUTE_BYTES = 256 * mebibyte;

function attentionActivationBytes(input: {
  tokens: number;
  heads: number;
  width: number;
  flashAttention: boolean;
}): number {
  const scores = input.flashAttention ? 0 : input.tokens ** 2 * input.heads * 4;
  return (
    scores +
    input.tokens * input.width * DIFFUSION_ACTIVATION_BYTES_PER_TOKEN_DIM
  );
}

// ---------------------------------------------------------------------------
// Images (sd-server)
// ---------------------------------------------------------------------------

/**
 * Default output and attention peak per family. Tokens are latent tokens at
 * the widest attention block (SD1.5: 64x64 at 512^2; SDXL: 64x64 at 1024^2,
 * its 128x128 level has no attention; DiT families: (W/16)(H/16) image
 * tokens plus text tokens: Flux.1 256 T5, Flux.2/Z-Image 512 Qwen3).
 * Heads/width from each model's published config.
 */
type ImageFamily = {
  pixels: number;
  tokens: number;
  heads: number;
  width: number;
};

const IMAGE_FAMILIES = {
  sd15: { pixels: 512 * 512, tokens: 4096, heads: 8, width: 320 },
  sdxl: { pixels: 1024 * 1024, tokens: 4096, heads: 10, width: 640 },
  flux1: { pixels: 1024 * 1024, tokens: 4096 + 256, heads: 24, width: 3072 },
  klein4b: { pixels: 1024 * 1024, tokens: 4096 + 512, heads: 24, width: 3072 },
  klein9b: { pixels: 1024 * 1024, tokens: 4096 + 512, heads: 32, width: 4096 },
  zImage: { pixels: 1024 * 1024, tokens: 4096 + 512, heads: 30, width: 3840 },
} as const satisfies Record<string, ImageFamily>;

function imageFamily(spec: ModelSpec): ImageFamily {
  const runtime = spec.imageRuntime;
  if (runtime?.kind === "diffusion-flux1") return IMAGE_FAMILIES.flux1;
  if (runtime?.kind === "diffusion-qwen3") {
    if (/z.?image/i.test(spec.family)) return IMAGE_FAMILIES.zImage;
    return /9b/i.test(spec.size)
      ? IMAGE_FAMILIES.klein9b
      : IMAGE_FAMILIES.klein4b;
  }
  return /XL/i.test(spec.family) ? IMAGE_FAMILIES.sdxl : IMAGE_FAMILIES.sd15;
}

/** Artifacts the launcher keeps on the host (flux1 runs `--clip-on-cpu`). */
function imageHostOnlyArtifacts(runtime: ImageRuntimeProfile | undefined) {
  return runtime?.kind === "diffusion-flux1"
    ? [runtime.artifacts.clipL, runtime.artifacts.t5xxl]
    : [];
}

/**
 * sd-server image demand (no `--diffusion-fa`, untiled VAE):
 * - activation = T^2 x heads x 4 + T x D x 48
 * - VAE decode = pixels x 128 ch x 4 B x 13 live maps
 * - accelerator = GPU-resident weights + max(stage compute) + overhead
 * - host        = all weights + host overhead
 * - unified     = all weights + max(stage compute) + host overhead
 */
export function estimateImageMemory(spec: ModelSpec): MemoryEstimate {
  const family = imageFamily(spec);
  const sizes = artifactBytesByName(spec);
  const weights = totalArtifactBytes(spec);
  const hostOnly = imageHostOnlyArtifacts(spec.imageRuntime).reduce(
    (sum, file) => sum + (sizes.get(file) ?? 0),
    0,
  );
  const activation = attentionActivationBytes({
    tokens: family.tokens,
    heads: family.heads,
    width: family.width,
    flashAttention: false,
  });
  const vaeDecode =
    family.pixels * IMAGE_VAE_CHANNELS * 4 * VAE_LIVE_FEATURE_MAPS;
  const compute = Math.max(activation, vaeDecode, TEXT_ENCODER_COMPUTE_BYTES);
  return freezeEstimate(
    {
      acceleratorBytes:
        weights - hostOnly + compute + ACCELERATOR_OVERHEAD_BYTES,
      hostBytes: weights + RUNTIME_HOST_OVERHEAD_BYTES,
      unifiedBytes: weights + compute + RUNTIME_HOST_OVERHEAD_BYTES,
    },
    "derived",
    {
      weights,
      hostOnlyWeights: hostOnly,
      activation,
      vaeDecode,
      textEncoderCompute: TEXT_ENCODER_COMPUTE_BYTES,
      acceleratorOverhead: ACCELERATOR_OVERHEAD_BYTES,
      hostOverhead: RUNTIME_HOST_OVERHEAD_BYTES,
    },
  );
}

// ---------------------------------------------------------------------------
// Video (sd-server vid_gen)
// ---------------------------------------------------------------------------

/** Wan VAE decodes causally in 4-frame chunks (Wan2.1 vae.py CausalConv3d). */
const VIDEO_VAE_CHUNK_FRAMES = 4;
/** Last-stage decoder widths: Wan VAE 96 channels, TAEHV/taew 64. */
const WAN_VAE_CHANNELS = 96;
const TAE_CHANNELS = 64;
/**
 * Live F32 maps per 4-frame chunk during video decode. Measured 2026-10-13:
 * the TAEHV decode of 81 frames at 832x480 took 14.45 maps of 64 channels;
 * the Wan VAE is assumed to need the same per channel (unmeasured).
 */
const VIDEO_VAE_LIVE_FEATURE_MAPS = 15;
/** sd-server `--max-vram` and the GPU budget round to this granularity. */
const VIDEO_ACCELERATOR_ROUNDING_BYTES = 256 * mebibyte;
/** Reference workload for catalog-only video models (Wan/LTX 480p, 81 frames). */
const CATALOG_ONLY_VIDEO = { width: 832, height: 480, frames: 81 } as const;

type VideoFamily = {
  spatial: number;
  temporal: number;
  heads: number;
  width: number;
};

/** Patchified latent strides and DiT width per family (published configs). */
const VIDEO_FAMILIES = {
  /** Wan2.1 VAE 8x spatial x 2x2 patch, 4x temporal. */
  wan13b: { spatial: 16, temporal: 4, heads: 12, width: 1536 },
  wan14b: { spatial: 16, temporal: 4, heads: 40, width: 5120 },
  /** Wan2.2 TI2V-5B VAE 16x spatial x 2x2 patch. */
  wan5b: { spatial: 32, temporal: 4, heads: 24, width: 3072 },
  /** LTX-Video VAE 32x spatial, 8x temporal, 1x1 patch. */
  ltx: { spatial: 32, temporal: 8, heads: 32, width: 4096 },
  /** HunyuanVideo 1.5: 16x spatial, 4x temporal, 2048-wide DiT. */
  hunyuan: { spatial: 16, temporal: 4, heads: 16, width: 2048 },
} as const satisfies Record<string, VideoFamily>;

/**
 * Text encoders that catalog-only models need but whose artifacts the catalog
 * does not list yet (sizes of the published GGUF/safetensors). Assumptions.
 */
const CATALOG_ONLY_TEXT_ENCODER_BYTES = {
  /** umt5-xxl-encoder-Q8_0.gguf, shared with the qualified Wan profiles. */
  wan: 6_044_158_944,
  /** Gemma 3 12B Q4_K_M (LTX-2 text encoder). */
  ltx: Math.round(6.8 * gibibyte),
  /** Qwen2.5-VL-7B Q4_K_M (HunyuanVideo 1.5 text encoder). */
  hunyuan: Math.round(4.4 * gibibyte),
} as const;

function videoFamily(spec: ModelSpec): VideoFamily {
  const id = spec.modelId.toLowerCase();
  if (id.startsWith("ltx")) return VIDEO_FAMILIES.ltx;
  if (id.startsWith("hunyuan")) return VIDEO_FAMILIES.hunyuan;
  if (/5b/.test(id)) return VIDEO_FAMILIES.wan5b;
  if (/14b/.test(id)) return VIDEO_FAMILIES.wan14b;
  return VIDEO_FAMILIES.wan13b;
}

export function videoLatentTokens(
  family: VideoFamily,
  shape: { width: number; height: number; frames: number },
): number {
  return (
    Math.ceil(shape.width / family.spatial) *
    Math.ceil(shape.height / family.spatial) *
    (1 + Math.ceil((shape.frames - 1) / family.temporal))
  );
}

function roundUp(bytes: number, granularity: number): number {
  return Math.ceil(bytes / granularity) * granularity;
}

/**
 * sd-server video demand at the profile's qualification bounds.
 *
 * - activation = T x D x 48 with `--diffusion-fa`, plus T^2 x heads x 4 without
 * - VAE decode = W x H x 4 frames x channels x 4 B x 15 live maps
 * - with `--offload-to-cpu` each stage (text encoder, DiT, decoder, audio
 *   encoder) is resident on the GPU only while it runs, so the accelerator
 *   needs the largest stage (weights + its compute); otherwise every
 *   component plus the largest compute term stays resident
 * - host    = all weights + host overhead
 * - unified = all weights + largest compute + host overhead
 *
 * The accelerator demand rounds up to 256 MiB so `--max-vram` prints cleanly.
 */
export function estimateVideoMemory(spec: ModelSpec): MemoryEstimate {
  const family = videoFamily(spec);
  const sizes = artifactBytesByName(spec);
  const runtime: VideoRuntimeProfile | undefined = spec.videoRuntime;
  const bounds = runtime
    ? {
        width: runtime.qualification.maxWidth,
        height: runtime.qualification.maxHeight,
        frames: runtime.qualification.maxFrames,
      }
    : CATALOG_ONLY_VIDEO;
  const flashAttention =
    runtime?.qualification.launchOptions.diffusionFlashAttention ?? true;
  const cpuOffload = runtime?.qualification.launchOptions.cpuOffload ?? false;
  const tokens = videoLatentTokens(family, bounds);
  const activation = attentionActivationBytes({
    tokens,
    heads: family.heads,
    width: family.width,
    flashAttention,
  });
  const decoderChannels =
    runtime?.artifacts.decoder.kind === "tae" ? TAE_CHANNELS : WAN_VAE_CHANNELS;
  const vaeDecode =
    bounds.width *
    bounds.height *
    VIDEO_VAE_CHUNK_FRAMES *
    decoderChannels *
    4 *
    VIDEO_VAE_LIVE_FEATURE_MAPS;

  let diffusionWeights: number;
  let textEncoderWeights: number;
  let decoderWeights = 0;
  let audioEncoderWeights = 0;
  if (runtime) {
    diffusionWeights = sizes.get(runtime.artifacts.diffusionModel) ?? 0;
    textEncoderWeights = sizes.get(runtime.artifacts.textEncoder) ?? 0;
    decoderWeights = sizes.get(runtime.artifacts.decoder.artifactFilename) ?? 0;
    audioEncoderWeights =
      runtime.mode === "s2v"
        ? (sizes.get(runtime.artifacts.audioEncoder) ?? 0)
        : 0;
  } else {
    // Catalog-only: every listed artifact is DiT weights (A14B ships two
    // experts and sd.cpp loads both); the text encoder is an allowance.
    diffusionWeights = totalArtifactBytes(spec);
    const id = spec.modelId.toLowerCase();
    textEncoderWeights = id.startsWith("ltx")
      ? CATALOG_ONLY_TEXT_ENCODER_BYTES.ltx
      : id.startsWith("hunyuan")
        ? CATALOG_ONLY_TEXT_ENCODER_BYTES.hunyuan
        : CATALOG_ONLY_TEXT_ENCODER_BYTES.wan;
  }
  const weights =
    diffusionWeights +
    textEncoderWeights +
    decoderWeights +
    audioEncoderWeights;
  const stages = {
    textEncoder: textEncoderWeights + TEXT_ENCODER_COMPUTE_BYTES,
    diffusion: diffusionWeights + activation,
    decoder: decoderWeights + vaeDecode,
    audioEncoder: audioEncoderWeights,
  };
  const compute = Math.max(activation, vaeDecode, TEXT_ENCODER_COMPUTE_BYTES);
  const resident = cpuOffload
    ? Math.max(...Object.values(stages))
    : weights + compute;
  return freezeEstimate(
    {
      acceleratorBytes: roundUp(
        resident + ACCELERATOR_OVERHEAD_BYTES,
        VIDEO_ACCELERATOR_ROUNDING_BYTES,
      ),
      hostBytes: weights + RUNTIME_HOST_OVERHEAD_BYTES,
      unifiedBytes: weights + compute + RUNTIME_HOST_OVERHEAD_BYTES,
    },
    runtime ? "derived" : "estimated",
    {
      diffusionWeights,
      textEncoderWeights,
      decoderWeights,
      audioEncoderWeights,
      activation,
      vaeDecode,
      textEncoderCompute: TEXT_ENCODER_COMPUTE_BYTES,
      acceleratorOverhead: ACCELERATOR_OVERHEAD_BYTES,
      hostOverhead: RUNTIME_HOST_OVERHEAD_BYTES,
    },
  );
}

// ---------------------------------------------------------------------------
// Measured demand lookups
// ---------------------------------------------------------------------------

/** The measured LLM demand when this exact launch shape was measured. */
export function measuredLlmDemand(input: {
  modelId?: string;
  artifactBytes: number;
  ctxTokens: number;
  slots: number;
  cacheTypeK: string;
  cacheTypeV: string;
  promptCacheBytes: number;
}): MeasuredMemoryDemand | undefined {
  const record = findLlmMeasurement(input.modelId, input);
  return record
    ? measuredDemand(record, {
        artifactBytes: input.artifactBytes,
        promptCacheBytes: input.promptCacheBytes,
      })
    : undefined;
}

/**
 * The measured demand of an STT, TTS, image or video catalog model at the
 * launch shape the spec carries (video: its qualification bounds/options).
 */
export function measuredModelDemand(
  spec: ModelSpec,
  artifactBytes: number = totalArtifactBytes(spec),
): MeasuredMemoryDemand | undefined {
  switch (spec.kind) {
    case "stt": {
      const record = findModelMeasurement("stt", spec.modelId);
      return record
        ? measuredDemand(record, {
            artifactBytes,
            acceleratorOverheadBytes: WHISPER_ACCELERATOR_OVERHEAD_BYTES,
          })
        : undefined;
    }
    case "tts":
    case "image": {
      const record = findModelMeasurement(spec.kind, spec.modelId);
      return record ? measuredDemand(record, { artifactBytes }) : undefined;
    }
    case "video": {
      const qualification = spec.videoRuntime?.qualification;
      if (!qualification) return undefined;
      const record = findVideoMeasurement(spec.modelId, {
        width: qualification.maxWidth,
        height: qualification.maxHeight,
        frames: qualification.maxFrames,
        cpuOffload: qualification.launchOptions.cpuOffload,
        diffusionFlashAttention:
          qualification.launchOptions.diffusionFlashAttention,
      });
      return record
        ? measuredDemand(record, {
            artifactBytes,
            acceleratorRoundingBytes: VIDEO_ACCELERATOR_ROUNDING_BYTES,
          })
        : undefined;
    }
    case "llm":
      return undefined;
  }
}

// ---------------------------------------------------------------------------
// Catalog entry point
// ---------------------------------------------------------------------------

/** LLM reference launch: 8K context, one slot, launcher KV type, no cache. */
function catalogLlmReference(spec: ModelSpec) {
  const weightsBytes = totalArtifactBytes(spec);
  const shape = resolveLlmShape({
    modelId: spec.modelId,
    size: spec.size,
    quant: spec.quant,
    weightsBytes,
  });
  const kvType = llmKvCacheType(Boolean(spec.llmRuntime), shape.shape.geometry);
  return {
    weightsBytes,
    shape,
    ctxTokens: Math.min(
      MIN_VRAM_LLM_CONTEXT_TOKENS,
      spec.contextWindowTokens ?? MIN_VRAM_LLM_CONTEXT_TOKENS,
    ),
    slots: 1,
    cacheTypeK: kvType,
    cacheTypeV: kvType,
    promptCacheBytes: 0,
  };
}

/**
 * Estimate for a catalog model at its reference shape: LLMs at 8K context,
 * one slot, launcher KV type and no prompt cache; diffusion at the family
 * default resolution; video at the qualification bounds.
 */
export function estimateCatalogModelMemory(spec: ModelSpec): MemoryEstimate {
  switch (spec.kind) {
    case "llm":
      return estimateLlmMemory(catalogLlmReference(spec));
    case "stt":
      return estimateSttMemory(spec);
    case "tts":
      return estimateTtsMemory(spec);
    case "image":
      return estimateImageMemory(spec);
    case "video":
      return estimateVideoMemory(spec);
  }
}

/** The measured demand at the catalog reference shape, when one exists. */
export function measuredCatalogModelMemory(
  spec: ModelSpec,
): MeasuredMemoryDemand | undefined {
  if (spec.kind !== "llm") return measuredModelDemand(spec);
  const reference = catalogLlmReference(spec);
  return measuredLlmDemand({
    ...reference,
    modelId: spec.modelId,
    artifactBytes: reference.weightsBytes,
  });
}

/** Catalog reference demand: the measurement when present, else the estimate. */
export function catalogModelDemand(spec: ModelSpec): RuntimeMemoryDemand {
  return preferMeasured(
    estimateCatalogModelMemory(spec),
    measuredCatalogModelMemory(spec),
  );
}

/** The minVramGb a catalog entry must declare: its accelerator demand's tier. */
export function derivedMinVramGb(spec: ModelSpec): number {
  return minVramTierGb(catalogModelDemand(spec).acceleratorBytes);
}
