/**
 * Peak memory measured from real runs of catalog models at their reference
 * launch shape. A matching record replaces the formula estimate through
 * `preferMeasured` (memory-estimate.ts), giving the demand confidence
 * "measured".
 *
 * Method (docs/memory-calibration.md):
 * - the backend runs exactly as the gateway launches it, on one machine;
 * - `backendAcceleratorBytes` is the sum of the backend's own GPU buffer
 *   report (llama.cpp `MTL0` model + context + compute, whisper.cpp Metal
 *   buffers, sd.cpp params + the largest per-stage compute buffer). On Apple
 *   unified memory these Metal allocations are not part of the process RSS,
 *   so RSS alone under-reports the accelerator demand;
 * - `hostBufferBytes` is the backend's CPU buffer report (0 when it prints
 *   none);
 * - `peakRssBytes` is the largest RSS of the backend process, sampled every
 *   250 ms through the whole workload;
 * - `systemDeltaBytes`, when recorded, is the peak drop of free + inactive
 *   system memory during the run.
 *
 * A record only applies to the launch shape it was measured at; any other
 * context size, slot count, KV type, resolution or launch option falls back
 * to the estimate.
 */

const mebibyte = 1024 * 1024;
const mib = (value: number): number => Math.round(value * mebibyte);
/** whisper.cpp prints buffer sizes in MB of 10^6 bytes. */
const mb = (value: number): number => Math.round(value * 1e6);

export const MEASUREMENT_DATE = "2026-10-13";
/** Second pass: the remaining runnable image and video profiles. */
export const IMAGE_VIDEO_MEASUREMENT_DATE = "2026-10-14";
export const MEASUREMENT_HARDWARE = "Apple M-series 64 GiB, macOS 26";

export type LlmMeasurementShape = Readonly<{
  ctxTokens: number;
  slots: number;
  cacheTypeK: string;
  cacheTypeV: string;
}>;

export type ImageMeasurementShape = Readonly<{
  width: number;
  height: number;
  steps: number;
  diffusionFlashAttention: boolean;
}>;

export type VideoMeasurementShape = Readonly<{
  width: number;
  height: number;
  frames: number;
  cpuOffload: boolean;
  diffusionFlashAttention: boolean;
}>;

type MeasurementBase = Readonly<{
  modelId: string;
  /** Quantization or artifact variant that was loaded. */
  variant: string;
  backendAcceleratorBytes: number;
  hostBufferBytes: number;
  peakRssBytes: number;
  systemDeltaBytes?: number;
  date: string;
  hardware: string;
  method: string;
}>;

export type MemoryMeasurement =
  | (MeasurementBase & { kind: "llm"; shape: LlmMeasurementShape })
  | (MeasurementBase & { kind: "stt"; shape: Readonly<Record<string, never>> })
  | (MeasurementBase & {
      kind: "tts";
      shape: Readonly<{ ctxTokens: number }>;
    })
  | (MeasurementBase & { kind: "image"; shape: ImageMeasurementShape })
  | (MeasurementBase & { kind: "video"; shape: VideoMeasurementShape });

const LLM_SHAPE: LlmMeasurementShape = {
  ctxTokens: 8192,
  slots: 1,
  cacheTypeK: "q8_0",
  cacheTypeV: "q8_0",
};
const LLM_METHOD =
  "llama-server with gateway flags (ctx 8192, 1 slot, q8_0 KV, flash " +
  "attention, cache-ram 2048); two chats of ~1750 prompt + <=256 output " +
  "tokens; MTL0 model + context + compute buffers; RSS sampled every 250 ms";
const IMAGE_METHOD =
  "sd-server via the gateway, catalog defaults with --diffusion-fa; params " +
  "+ largest stage compute buffer (VAE decode); RSS sampled every 250 ms";

function llm(
  modelId: string,
  variant: string,
  metalMib: number,
  hostMib: number,
  peakRssBytes: number,
): MemoryMeasurement {
  return {
    kind: "llm",
    modelId,
    variant,
    shape: LLM_SHAPE,
    backendAcceleratorBytes: mib(metalMib),
    hostBufferBytes: mib(hostMib),
    peakRssBytes,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: LLM_METHOD,
  };
}

export const MEMORY_MEASUREMENTS: readonly MemoryMeasurement[] = Object.freeze([
  llm("qwen3.5-0.8b-q4_k_m", "Q4_K_M", 521 + 70 + 497, 211, 827_424_768),
  llm("qwen3.5-2b-q4_k_m", "Q4_K_M", 1259 + 70 + 493, 414, 1_593_032_704),
  llm("qwen3.5-4b-q4_k_m", "Q4_K_M", 2740 + 186 + 495, 515, 3_320_250_368),
  llm("qwen3.5-9b-q4_k_m", "Q4_K_M", 5626 + 186 + 501, 569, 6_390_480_896),
  llm("qwen3.5-27b-q4_k_m", "Q4_K_M", 16710 + 421 + 513, 710, 18_096_013_312),
  llm(
    "ministral-3-14b-instruct-q4_k_m",
    "Q4_K_M",
    7849 + 680 + 266,
    388,
    7_823_081_472,
  ),
  llm("mistral-nemo-12b-instruct-q4_k_m", "Q4_K_M", 8107, 388, 8_069_824_512),
  llm("gpt-oss-20b-q4_k_m", "Q4_K_M", 11120 + 111 + 404, 399, 10_298_408_960),
  {
    kind: "stt",
    modelId: "whisper-tiny-en-q8_0",
    variant: "Q8_0",
    shape: {},
    backendAcceleratorBytes: mb(208.9),
    hostBufferBytes: 0,
    peakRssBytes: 169_902_080,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "direct whisper-server with gateway flags; two transcriptions of a " +
      "~13 s clip; Metal model + KV + compute buffers; RSS sampled every " +
      "250 ms",
  },
  {
    kind: "stt",
    modelId: "whisper-large-v3-turbo",
    variant: "F16",
    shape: {},
    backendAcceleratorBytes: mb(1941.8),
    hostBufferBytes: 0,
    peakRssBytes: 1_900_625_920,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "direct whisper-server with gateway flags; two transcriptions of a " +
      "~13 s clip; Metal model + KV + compute buffers; RSS sampled every " +
      "250 ms",
  },
  {
    kind: "tts",
    modelId: "qwen3-tts-1.7b-base-q4_k_m",
    variant: "Q4_K_M",
    // llama-tts -c 2048: 224 MiB f16 KV (was 3584 MiB at the implicit 32K).
    shape: { ctxTokens: 2048 },
    backendAcceleratorBytes: mib(982 + 224 + 632 + 2 * 426 + 141 + 868),
    hostBufferBytes: 0,
    peakRssBytes: 4_160_536_576,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "llama-tts with the gateway argv (-c 2048, harbor voice); worst-case " +
      "256 four-byte code points, 256 frames; Metal talker + KV + compute + " +
      "2x projector + codec buffers; RSS sampled every 250 ms",
  },
  {
    kind: "tts",
    modelId: "kokoro-82m-v1.0-q8",
    variant: "model_quantized (q8)",
    // One request is at most 256 characters, well under the 510-token window.
    shape: { ctxTokens: 512 },
    // ONNX Runtime CPU session inside the packaged Bun runtime: no GPU buffers.
    backendAcceleratorBytes: 0,
    hostBufferBytes: 0,
    peakRssBytes: 1_027_719_168,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "kokoro-tts v0.0.1 CI package through the gateway (cold per request); " +
      "af_heart and af_bella, 56 and 239 characters (17.2 s of audio); " +
      "RSS of the runtime process sampled every 250 ms",
  },
  {
    kind: "image",
    modelId: "dreamshaper-v8",
    variant: "safetensors F16",
    shape: {
      width: 512,
      height: 512,
      steps: 20,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(2035 + 1664.06),
    hostBufferBytes: 0,
    peakRssBytes: 2_988_212_224,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "flux2-klein-4b-q4_0",
    variant: "Q4_0",
    shape: {
      width: 1024,
      height: 1024,
      steps: 4,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(4882.75 + 6658),
    hostBufferBytes: 0,
    peakRssBytes: 5_983_240_192,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "dreamshaper-8-lcm",
    variant: "safetensors F16 (LCM)",
    shape: { width: 512, height: 512, steps: 4, diffusionFlashAttention: true },
    backendAcceleratorBytes: mib(2035 + 1664.06),
    hostBufferBytes: 0,
    peakRssBytes: 3_006_021_632,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "stable-diffusion-v1-5",
    variant: "safetensors F16",
    shape: {
      width: 512,
      height: 512,
      steps: 20,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(2784.45 + 1664.06),
    hostBufferBytes: 0,
    peakRssBytes: 4_996_431_872,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "z-image-turbo-q4_0",
    variant: "Q4_0 + Qwen3-4B-Instruct Q4_K_M",
    shape: {
      width: 1024,
      height: 1024,
      steps: 8,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(6048.62 + 6657),
    hostBufferBytes: 0,
    peakRssBytes: 7_087_439_872,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "flux2-klein-4b-q8_0",
    variant: "Q8_0 + Qwen3-4B Q4_K_M",
    shape: {
      width: 1024,
      height: 1024,
      steps: 4,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(6637.75 + 6658),
    hostBufferBytes: 0,
    peakRssBytes: 8_412_053_504,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "realvis-xl-v5.0",
    variant: "safetensors F16",
    shape: {
      width: 1024,
      height: 1024,
      steps: 20,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(6624.11 + 7680.25),
    hostBufferBytes: 0,
    peakRssBytes: 7_805_108_224,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "sdxl-turbo",
    variant: "safetensors F16",
    shape: {
      width: 1024,
      height: 1024,
      steps: 20,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(6624.11 + 7680.25),
    hostBufferBytes: 0,
    peakRssBytes: 8_064_352_256,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "image",
    modelId: "juggernaut-xl-v9",
    variant: "safetensors F16",
    shape: {
      width: 1024,
      height: 1024,
      steps: 20,
      diffusionFlashAttention: true,
    },
    backendAcceleratorBytes: mib(6624.11 + 7680.25),
    hostBufferBytes: 0,
    peakRssBytes: 7_788_806_144,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method: IMAGE_METHOD,
  },
  {
    kind: "video",
    modelId: "fastwan2.2-ti2v-5b-q6_k",
    variant: "Q6_K + umt5-xxl Q8_0 + TAEHV",
    shape: {
      width: 480,
      height: 832,
      frames: 81,
      cpuOffload: true,
      diffusionFlashAttention: true,
    },
    // Offload streams one stage at a time; the T5 stage is the largest.
    backendAcceleratorBytes: mib(5757.05 + 297),
    hostBufferBytes: mib(9795.96),
    peakRssBytes: 13_746_454_528,
    systemDeltaBytes: 15_058_550_784,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "sd-server with the qualification launch options (offload, " +
      "diffusion-fa, vae-conv-direct, TAE), 832x480, 81 frames, 3 steps; " +
      "largest stage params + compute; RSS and free memory sampled every " +
      "250 ms",
  },
  {
    kind: "video",
    modelId: "wan2.1-t2v-1.3b-q8_0",
    variant: "Q8_0 + umt5-xxl Q8_0 + Wan2.1 VAE",
    shape: {
      width: 320,
      height: 320,
      frames: 33,
      cpuOffload: true,
      diffusionFlashAttention: true,
    },
    // Offload streams one stage at a time; the T5 stage is the largest. The
    // Wan VAE stage is 242.10 MiB of weights plus 3435.35 MiB of decode.
    backendAcceleratorBytes: mib(5757.05 + 297),
    hostBufferBytes: mib(7463.53),
    peakRssBytes: 12_147_949_568,
    systemDeltaBytes: 8_220_786_688,
    date: IMAGE_VIDEO_MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "sd-server with the qualification launch options (offload, " +
      "diffusion-fa), 320x320, 33 frames, 20 steps; largest stage params + " +
      "compute; RSS and free memory sampled every 250 ms",
  },
]);

function byModel(kind: MemoryMeasurement["kind"], modelId: string) {
  return MEMORY_MEASUREMENTS.find(
    (record) => record.kind === kind && record.modelId === modelId,
  );
}

/** The LLM record for this model at exactly this launch shape. */
export function findLlmMeasurement(
  modelId: string | undefined,
  shape: LlmMeasurementShape,
): MemoryMeasurement | undefined {
  if (!modelId) return undefined;
  const record = byModel("llm", modelId);
  if (record?.kind !== "llm") return undefined;
  const measured = record.shape;
  return measured.ctxTokens === shape.ctxTokens &&
    measured.slots === shape.slots &&
    measured.cacheTypeK === shape.cacheTypeK &&
    measured.cacheTypeV === shape.cacheTypeV
    ? record
    : undefined;
}

/** STT and TTS runtimes launch one fixed shape per model. */
export function findModelMeasurement(
  kind: "stt" | "tts",
  modelId: string | undefined,
): MemoryMeasurement | undefined {
  return modelId ? byModel(kind, modelId) : undefined;
}

/**
 * The image record for this model, only when it was measured with the same
 * `--diffusion-fa` setting the catalog launches (it changes the diffusion
 * compute buffer and timings).
 */
export function findImageMeasurement(
  modelId: string | undefined,
  diffusionFlashAttention: boolean,
): MemoryMeasurement | undefined {
  if (!modelId) return undefined;
  const record = byModel("image", modelId);
  return record?.kind === "image" &&
    record.shape.diffusionFlashAttention === diffusionFlashAttention
    ? record
    : undefined;
}

/** The video record for this model at exactly these bounds and options. */
export function findVideoMeasurement(
  modelId: string | undefined,
  shape: VideoMeasurementShape,
): MemoryMeasurement | undefined {
  if (!modelId) return undefined;
  const record = byModel("video", modelId);
  if (record?.kind !== "video") return undefined;
  const measured = record.shape;
  return measured.width === shape.width &&
    measured.height === shape.height &&
    measured.frames === shape.frames &&
    measured.cpuOffload === shape.cpuOffload &&
    measured.diffusionFlashAttention === shape.diffusionFlashAttention
    ? record
    : undefined;
}
