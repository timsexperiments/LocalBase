import { usesReferenceMeasurements } from "../../catalog/sources";
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
 * (GGUF header of the installed file) and "estimated" when it falls back
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
  findImageMeasurement,
  findModelMeasurement,
  findVideoMeasurement,
  type MemoryMeasurement,
} from "./memory-measurements";
import { SPEECH_CONTEXT_TOKENS } from "./speech-supervisor";

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
// Minimum memory
// ---------------------------------------------------------------------------

/** Safety margin over a demand figure: max(512 MiB, 10%). */
export const MINIMUM_MEMORY_MARGIN_BYTES = 512 * mebibyte;
export const MINIMUM_MEMORY_MARGIN_RATIO = 0.1;

/**
 * Whole GiB that hold `bytes` plus the safety margin. This is the computed
 * `minVramGb` the catalog no longer declares by hand.
 */
export function minimumMemoryGb(bytes: number): number {
  const required =
    bytes +
    Math.max(MINIMUM_MEMORY_MARGIN_BYTES, bytes * MINIMUM_MEMORY_MARGIN_RATIO);
  return Math.max(1, Math.ceil(required / gibibyte));
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
/** Reference launch for catalog fit: one slot, 8K tokens, no prompt cache. */
export const LLM_REFERENCE_CONTEXT_TOKENS = 8192;

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
  source: "gguf" | "fallback";
}>;

/**
 * Shape for a model: a GGUF header's KV geometry wins (it describes the exact
 * file); without one the size-based fallback stands in. The compute and slot
 * terms always use the fallback's hidden size and vocabulary, which are
 * allowances (152064 tokens is a large-vocab upper bound for most families).
 */
export function resolveLlmShape(input: {
  size?: string;
  quant?: string;
  weightsBytes: number;
  geometry?: LlmKvGeometry | null;
}): ResolvedLlmShape {
  const base = fallbackLlmShape(
    parameterBillions(input.size, input.weightsBytes, input.quant),
  );
  if (input.geometry) {
    return Object.freeze({
      shape: Object.freeze({ ...base, geometry: input.geometry }),
      source: "gguf",
    });
  }
  return Object.freeze({ shape: base, source: "fallback" });
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

/** tts-server and llama-tts launch the talker with this explicit `-c`. */
const TTS_CONTEXT_TOKENS = SPEECH_CONTEXT_TOKENS;
/** Audio codec/vocoder decode working set. Allowance. */
const TTS_CODEC_BYTES = 1 * gibibyte;
/** The Qwen3-TTS runtime loads the projector twice (measured 2 x 426 MiB Metal buffers). */
const TTS_PROJECTOR_COPIES = 2;

/**
 * ONNX Runtime CPU session working set for Kokoro: the dequantized q8 graph,
 * initializer copies and the arena for one 510-token synthesis. Allowance.
 */
const KOKORO_SESSION_BYTES = 512 * mebibyte;

/**
 * Kokoro runs on the CPU inside the packaged Bun runtime, so it holds no
 * accelerator memory: the weights, the ONNX session and the runtime overhead
 * are all host-resident (and so also count against unified memory).
 */
function estimateKokoroMemory(spec: ModelSpec): MemoryEstimate {
  const terms = {
    weights: totalArtifactBytes(spec),
    session: KOKORO_SESSION_BYTES,
    overhead: RUNTIME_HOST_OVERHEAD_BYTES,
  };
  const total = Object.values(terms).reduce((sum, value) => sum + value, 0);
  return freezeEstimate(
    { acceleratorBytes: 0, hostBytes: total, unifiedBytes: total },
    "estimated",
    terms,
  );
}

/**
 * Qwen3-TTS runs a llama.cpp talker LLM plus the multimodal projector and the
 * audio codec. The talker uses the GGUF geometry when the file is local, else
 * the size-based fallback shape, with the f16 launch context; the projector is
 * loaded twice. Every term is held in one process, so all pools carry the
 * full sum.
 */
export function estimateTtsMemory(
  spec: ModelSpec,
  talkerGeometry?: LlmKvGeometry | null,
): MemoryEstimate {
  if (spec.ttsRuntime?.kind === "kokoro-onnx") {
    return estimateKokoroMemory(spec);
  }
  const sizes = artifactBytesByName(spec);
  const projector = spec.ttsRuntime
    ? (sizes.get(spec.ttsRuntime.projectorArtifactFilename) ?? 0)
    : 0;
  const weights = totalArtifactBytes(spec) - projector;
  const talker = estimateLlmMemory({
    weightsBytes: weights,
    shape: resolveLlmShape({
      size: spec.size,
      quant: spec.quant,
      weightsBytes: weights,
      geometry: talkerGeometry,
    }),
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
 * 2026-10-13 and 2026-10-14: the sd.cpp VAE compute buffer is 13.0 maps of
 * 128 channels for SD1.5 at 512^2 (1664 MiB) and for Flux.2 and Z-Image at
 * 1024^2 (6657-6658 MiB). The SDXL decoder (sd.cpp applies its Conv2D scale
 * to SDXL checkpoints without a separate VAE) holds 15.0 maps: 7680 MiB at
 * 1024^2 for both RealVisXL and SDXL Turbo.
 */
const VAE_LIVE_FEATURE_MAPS = 13;
const SDXL_VAE_LIVE_FEATURE_MAPS = 15;
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
  /** Live VAE decoder maps; defaults to `VAE_LIVE_FEATURE_MAPS`. */
  vaeMaps?: number;
};

const IMAGE_FAMILIES = {
  sd15: { pixels: 512 * 512, tokens: 4096, heads: 8, width: 320 },
  sdxl: {
    pixels: 1024 * 1024,
    tokens: 4096,
    heads: 10,
    width: 640,
    vaeMaps: SDXL_VAE_LIVE_FEATURE_MAPS,
  },
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
 * sd-server image demand (untiled VAE):
 * - activation = T x D x 48, plus T^2 x heads x 4 scores without
 *   `--diffusion-fa` (catalog `imageLaunchOptions`)
 * - VAE decode = pixels x 128 ch x 4 B x live maps (13; SDXL 15)
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
    flashAttention: spec.imageLaunchOptions?.diffusionFlashAttention ?? false,
  });
  const vaeDecode =
    family.pixels *
    IMAGE_VAE_CHANNELS *
    4 *
    (family.vaeMaps ?? VAE_LIVE_FEATURE_MAPS);
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
 * the TAEHV decode of 81 frames at 832x480 took 14.45 maps of 64 channels.
 * Measured 2026-10-14: the Wan2.1 VAE decode of 33 frames at 320x320 took
 * 3435 MiB, 22.9 maps of 96 channels. Catalog-only profiles use the Wan VAE
 * value.
 */
const TAE_LIVE_FEATURE_MAPS = 15;
const WAN_VAE_LIVE_FEATURE_MAPS = 23;
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
  const tae = runtime?.artifacts.decoder.kind === "tae";
  const decoderChannels = tae ? TAE_CHANNELS : WAN_VAE_CHANNELS;
  const vaeDecode =
    bounds.width *
    bounds.height *
    VIDEO_VAE_CHUNK_FRAMES *
    decoderChannels *
    4 *
    (tae ? TAE_LIVE_FEATURE_MAPS : WAN_VAE_LIVE_FEATURE_MAPS);

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
  if (!usesReferenceMeasurements(spec)) return undefined;
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
    case "image": {
      const record = findImageMeasurement(
        spec.modelId,
        spec.imageLaunchOptions?.diffusionFlashAttention ?? false,
      );
      return record ? measuredDemand(record, { artifactBytes }) : undefined;
    }
    case "tts": {
      const record = findModelMeasurement("tts", spec.modelId);
      // Kokoro runs on the CPU, so it carries no accelerator context.
      const acceleratorOverheadBytes =
        spec.ttsRuntime?.kind === "kokoro-onnx" ? 0 : undefined;
      return record
        ? measuredDemand(record, { artifactBytes, acceleratorOverheadBytes })
        : undefined;
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

/**
 * Local facts that sharpen a catalog estimate: the GGUF KV geometry of the
 * LLM (or TTS talker) file when it is installed.
 */
export type CatalogEstimateInputs = Readonly<{
  geometry?: LlmKvGeometry | null;
}>;

/** LLM reference launch: 8K context, one slot, launcher KV type, no cache. */
export function catalogLlmReference(
  spec: ModelSpec,
  inputs: CatalogEstimateInputs = {},
) {
  const weightsBytes = totalArtifactBytes(spec);
  const shape = resolveLlmShape({
    size: spec.size,
    quant: spec.quant,
    weightsBytes,
    geometry: inputs.geometry,
  });
  const kvType = llmKvCacheType(Boolean(spec.llmRuntime), shape.shape.geometry);
  return {
    weightsBytes,
    shape,
    ctxTokens: Math.min(
      LLM_REFERENCE_CONTEXT_TOKENS,
      spec.contextWindowTokens ?? LLM_REFERENCE_CONTEXT_TOKENS,
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
 * default resolution; video at the qualification bounds. Artifact sizes come
 * from the catalog; the GGUF geometry, when given, replaces the size-based
 * layout.
 */
export function estimateCatalogModelMemory(
  spec: ModelSpec,
  inputs: CatalogEstimateInputs = {},
): MemoryEstimate {
  switch (spec.kind) {
    case "llm":
      return estimateLlmMemory(catalogLlmReference(spec, inputs));
    case "stt":
      return estimateSttMemory(spec);
    case "tts":
      return estimateTtsMemory(spec, inputs.geometry);
    case "image":
      return estimateImageMemory(spec);
    case "video":
      return estimateVideoMemory(spec);
  }
}

/** The measured demand at the catalog reference shape, when one exists. */
export function measuredCatalogModelMemory(
  spec: ModelSpec,
  inputs: CatalogEstimateInputs = {},
): MeasuredMemoryDemand | undefined {
  if (!usesReferenceMeasurements(spec)) return undefined;
  if (spec.kind !== "llm") return measuredModelDemand(spec);
  const reference = catalogLlmReference(spec, inputs);
  return measuredLlmDemand({
    ...reference,
    modelId: spec.modelId,
    artifactBytes: reference.weightsBytes,
  });
}

/** Catalog reference demand: the measurement when present, else the estimate. */
export function catalogModelDemand(
  spec: ModelSpec,
  inputs: CatalogEstimateInputs = {},
): RuntimeMemoryDemand {
  return preferMeasured(
    estimateCatalogModelMemory(spec, inputs),
    measuredCatalogModelMemory(spec, inputs),
  );
}
