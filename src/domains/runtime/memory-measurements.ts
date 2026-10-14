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
    // llama-tts runs without -c, so the talker allocates its full 32K f16 KV.
    shape: { ctxTokens: 32768 },
    backendAcceleratorBytes: mib(982 + 3584 + 658 + 2 * 426 + 141 + 868),
    hostBufferBytes: 0,
    peakRssBytes: 7_040_352_256,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "speech runtime via the gateway; one synthesis; Metal talker + KV + " +
      "compute + 2x projector + codec buffers; RSS sampled every 250 ms",
  },
  {
    kind: "image",
    modelId: "dreamshaper-v8",
    variant: "safetensors F16",
    shape: { width: 512, height: 512, steps: 20 },
    backendAcceleratorBytes: mib(2035 + 1664.06),
    hostBufferBytes: 0,
    peakRssBytes: 2_924_511_232,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "sd-server via the gateway, catalog defaults; params + largest " +
      "stage compute buffer (VAE decode); RSS sampled every 250 ms",
  },
  {
    kind: "image",
    modelId: "flux2-klein-4b-q4_0",
    variant: "Q4_0",
    shape: { width: 1024, height: 1024, steps: 4 },
    backendAcceleratorBytes: mib(4882.75 + 6658),
    hostBufferBytes: 0,
    peakRssBytes: 5_941_821_440,
    date: MEASUREMENT_DATE,
    hardware: MEASUREMENT_HARDWARE,
    method:
      "sd-server via the gateway, catalog defaults; params + largest " +
      "stage compute buffer (VAE decode); RSS sampled every 250 ms",
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

/** STT, TTS and image runtimes launch one fixed shape per model. */
export function findModelMeasurement(
  kind: "stt" | "tts" | "image",
  modelId: string | undefined,
): MemoryMeasurement | undefined {
  return modelId ? byModel(kind, modelId) : undefined;
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
