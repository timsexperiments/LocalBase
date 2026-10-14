import { isIP } from "node:net";
import { join } from "node:path";
import {
  allocateParallelSlots,
  type ParallelAllocation,
  type ParallelSlots,
} from "../config/parallel";
import type {
  EmbeddingLlmRuntimeProfile,
  ImageRuntimeProfile,
  ModelSpec,
  VideoRuntimeProfile,
  VideoRuntimeTarget,
} from "../../catalog";
import { calculateMaxSafeContextSize } from "../../catalog";
import {
  kvCacheBytes,
  type KvCacheType,
  type LlmKvGeometry,
} from "./gguf-metadata";
import type { RuntimeComponent, RuntimeModality } from "./modality";
import { gibibyte, type RuntimeMemoryDemand } from "./memory-safety";
import {
  catalogModelDemand,
  estimateLlmMemory,
  genericArtifactDemand,
  llmKvCacheType,
  measuredLlmDemand,
  preferMeasured,
  resolveLlmShape,
  totalArtifactBytes,
} from "./memory-estimate";

const LOOPBACK_HOST = "127.0.0.1";

/**
 * Supervised backends (llama/whisper/sd servers) have no auth of their own, so
 * they must never listen on every interface. Wildcard hosts, including the old
 * persisted `0.0.0.0` default, are normalized to loopback. Wildcards are also
 * invalid connect targets, so the same value is used for gateway-to-backend
 * requests. Only IPv4 loopback, IPv6 loopback, and localhost pass through;
 * every other address and hostname falls back to loopback.
 */
export function backendBindHost(host: string): string {
  const trimmed = host.trim();
  if (/^localhost\.?$/i.test(trimmed)) return trimmed;

  const address =
    trimmed.startsWith("[") && trimmed.endsWith("]")
      ? trimmed.slice(1, -1)
      : trimmed;
  const unscopedAddress = address.split("%", 1)[0] ?? "";
  if (unscopedAddress.toLowerCase() === "::1") return "::1";
  if (isIP(unscopedAddress) === 4 && unscopedAddress.startsWith("127.")) {
    return unscopedAddress;
  }
  return LOOPBACK_HOST;
}

function urlHost(host: string): string {
  const bound = backendBindHost(host);
  return bound.includes(":") && !bound.startsWith("[") ? `[${bound}]` : bound;
}

/** KV cache type for chat models; the estimate and llama-server argv share it. */
export const DEFAULT_LLM_KV_CACHE_TYPE: KvCacheType = "q8_0";

/** Host RAM (MiB) llama-server may spend on prompt-cache states for chat models. */
export const LLAMA_PROMPT_CACHE_RAM_MIB = 2048;

export type RuntimeHardware = { memoryGb: number };

/** Context budget used by both runtime startup and the served-model listing. */
export function configuredLlmContextSize(
  model: ModelSpec | undefined,
  configCtxSize: number,
  memoryGb: number,
  override?: number,
): number {
  if (override !== undefined) return override;
  const recommended = model
    ? calculateMaxSafeContextSize(model, memoryGb)
    : memoryGb >= 32
      ? 32768
      : 8192;
  return Math.min(recommended, configCtxSize);
}

/** Build a model launch plan from the same config and model inputs in every caller. */
export function resolveConfiguredLlmLaunchPlan(input: {
  runtimeId: string;
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
  model: ModelSpec | undefined;
  configCtxSize: number;
  ctxSizeOverride?: number;
  parallel: ParallelSlots;
  artifactBytes: number;
  memoryGb: number;
  kvGeometry?: LlmKvGeometry | null;
  trainingContextLength?: number | null;
}): LlmLaunchPlan {
  return resolveLlmLaunchPlan({
    runtimeId: input.runtimeId,
    root: input.root,
    modelsDirectory: input.modelsDirectory,
    modelId: input.modelId,
    modelFile: input.modelFile,
    host: input.host,
    port: input.port,
    model: input.model,
    ctxSize: configuredLlmContextSize(
      input.model,
      input.configCtxSize,
      input.memoryGb,
      input.ctxSizeOverride,
    ),
    contextWindowTokens: input.model?.contextWindowTokens,
    parallel: input.parallel,
    artifactBytes: input.artifactBytes,
    hardware: { memoryGb: input.memoryGb },
    embedding: input.model?.llmRuntime,
    kvGeometry: input.kvGeometry,
    trainingContextLength: input.trainingContextLength,
  });
}

type LaunchPlanBase<
  Modality extends RuntimeModality,
  Component extends RuntimeComponent,
> = {
  readonly runtimeId: string;
  readonly modality: Modality;
  readonly component: Component;
  readonly root: string;
  readonly modelId: string;
  readonly modelFile: string;
  readonly modelPath: string;
  readonly host: string;
  readonly port: number;
  readonly healthUrl: string;
  readonly memoryDemand: RuntimeMemoryDemand;
};

export type LlmLaunchPlan = LaunchPlanBase<"llm", "llama-server"> & {
  readonly ctxSize: number;
  readonly trainingContextLength?: number | null;
  readonly parallel: ParallelAllocation;
  readonly hardware: Readonly<RuntimeHardware>;
  readonly embedding: EmbeddingLlmRuntimeProfile | null;
  readonly kvCache: Readonly<{ typeK: KvCacheType; typeV: KvCacheType }>;
  readonly kvGeometry: LlmKvGeometry | null;
  readonly promptCacheRamMib: number;
};

export type SttLaunchPlan = LaunchPlanBase<"stt", "whisper-server">;

type ImageRuntimeLaunchProfile =
  | Readonly<{
      kind: "diffusion-qwen3";
      diffusionModelPath: string;
      vaePath: string;
      textEncoderPath: string;
      generation: Readonly<
        Extract<ImageRuntimeProfile, { kind: "diffusion-qwen3" }>["generation"]
      >;
    }>
  | Readonly<{
      kind: "diffusion-flux1";
      diffusionModelPath: string;
      vaePath: string;
      clipLPath: string;
      t5xxlPath: string;
      generation: Readonly<
        Extract<ImageRuntimeProfile, { kind: "diffusion-flux1" }>["generation"]
      >;
    }>
  | Readonly<{
      kind: "checkpoint";
      diffusionModelPath: string;
      generation: Readonly<
        Extract<ImageRuntimeProfile, { kind: "checkpoint" }>["generation"]
      >;
    }>;

export type ImageLaunchPlan = LaunchPlanBase<"image", "sd-server"> & {
  readonly imageRuntime?: ImageRuntimeLaunchProfile;
};

type VideoLaunchPlanBase = Omit<
  LaunchPlanBase<"video", "sd-server">,
  "modelFile" | "modelPath"
> & {
  readonly diffusionModelPath: string;
  readonly textEncoderPath: string;
  readonly inputBounds: Readonly<{
    maxWidth: number;
    maxHeight: number;
    maxFrames: number;
    fps: number;
  }>;
  readonly generation: Readonly<
    VideoRuntimeProfile["qualification"]["generation"]
  >;
  readonly launchOptions: Readonly<
    VideoRuntimeProfile["qualification"]["launchOptions"]
  >;
};

export type VideoLaunchPlan = VideoLaunchPlanBase &
  (
    | Readonly<{
        mode: "t2v";
        decoder:
          | Readonly<{ kind: "vae"; path: string }>
          | Readonly<{ kind: "tae"; path: string }>;
      }>
    | Readonly<{
        mode: "s2v";
        decoder: Readonly<{ kind: "vae"; path: string }>;
        audioEncoderPath: string;
      }>
  );

export type RuntimeLaunchPlan =
  LlmLaunchPlan | SttLaunchPlan | ImageLaunchPlan | VideoLaunchPlan;

/**
 * Demand for a catalog-backed runtime, never below the bytes actually on disk
 * (a local file may be larger than the catalog's expected size).
 */
function catalogDemand(
  model: ModelSpec,
  estimate: RuntimeMemoryDemand,
  artifactBytes: number,
): RuntimeMemoryDemand {
  const extra = Math.max(0, artifactBytes - totalArtifactBytes(model));
  if (extra === 0) return estimate;
  return Object.freeze({
    unifiedBytes: estimate.unifiedBytes + extra,
    hostBytes: estimate.hostBytes + extra,
    acceleratorBytes: estimate.acceleratorBytes + extra,
    confidence: estimate.confidence,
  });
}

function sttMemoryDemand(input: {
  model?: ModelSpec;
  artifactBytes: number;
}): RuntimeMemoryDemand {
  return input.model?.kind === "stt"
    ? catalogDemand(
        input.model,
        catalogModelDemand(input.model),
        input.artifactBytes,
      )
    : genericArtifactDemand(input.artifactBytes).demand;
}

function imageMemoryDemand(input: {
  model?: ModelSpec;
  artifactBytes: number;
}): RuntimeMemoryDemand {
  return input.model?.kind === "image"
    ? catalogDemand(
        input.model,
        catalogModelDemand(input.model),
        input.artifactBytes,
      )
    : genericArtifactDemand(input.artifactBytes).demand;
}

function llmMemoryDemand(input: {
  model?: ModelSpec;
  artifactBytes: number;
  ctxSize: number;
  parallel: ParallelAllocation;
  kvCache: LlmLaunchPlan["kvCache"];
  kvGeometry: LlmKvGeometry | null;
  promptCacheRamMib: number;
}): RuntimeMemoryDemand {
  const shape = resolveLlmShape({
    modelId: input.model?.modelId,
    size: input.model?.size,
    quant: input.model?.quant,
    weightsBytes: input.artifactBytes,
    geometry: input.kvGeometry,
  });
  const launch = {
    ctxTokens: input.ctxSize,
    slots: input.parallel.slots,
    cacheTypeK: input.kvCache.typeK,
    cacheTypeV: input.kvCache.typeV,
    promptCacheBytes: input.promptCacheRamMib * 1024 * 1024,
  };
  const estimate = estimateLlmMemory({
    ...launch,
    weightsBytes: input.artifactBytes,
    shape,
  });
  if (input.model?.kind !== "llm") return estimate.demand;
  // A measurement was taken with the catalog weights; a larger local file
  // still adds its extra bytes.
  const measured = measuredLlmDemand({
    ...launch,
    modelId: input.model.modelId,
    artifactBytes: input.artifactBytes,
  });
  return measured
    ? catalogDemand(
        input.model,
        preferMeasured(estimate, measured),
        input.artifactBytes,
      )
    : estimate.demand;
}

export function resolveLlmLaunchPlan(input: {
  runtimeId: string;
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
  ctxSize: number;
  contextWindowTokens?: number | null;
  parallel: ParallelSlots;
  /** Catalog entry, when known; supplies the published model shape. */
  model?: ModelSpec;
  artifactBytes: number;
  hardware: RuntimeHardware;
  embedding?: EmbeddingLlmRuntimeProfile | null;
  kvGeometry?: LlmKvGeometry | null;
  trainingContextLength?: number | null;
}): LlmLaunchPlan {
  const ctxSize = Math.min(
    input.ctxSize,
    input.contextWindowTokens ?? input.ctxSize,
  );
  const kvGeometry = input.kvGeometry ?? null;
  // q8_0 needs every head dim to be a multiple of 32; unknown geometry or a
  // misfit falls back to f16 so llama.cpp does not refuse to start.
  const kvType =
    llmKvCacheType(Boolean(input.embedding), kvGeometry) === "f16"
      ? "f16"
      : DEFAULT_LLM_KV_CACHE_TYPE;
  const kvCache = Object.freeze({ typeK: kvType, typeV: kvType });
  const promptCacheRamMib = input.embedding ? 0 : LLAMA_PROMPT_CACHE_RAM_MIB;
  const parallel = allocateParallelSlots({
    parallel: input.parallel,
    memoryGb: input.hardware.memoryGb,
    modelRequirementGb: input.artifactBytes / gibibyte,
    ctxSize,
    ...(kvGeometry
      ? {
          kvCacheGb: (slots: number) =>
            kvCacheBytes(kvGeometry, {
              ctxTokens: ctxSize,
              slots,
              cacheTypeK: kvCache.typeK,
              cacheTypeV: kvCache.typeV,
            }) / gibibyte,
        }
      : {}),
  });
  return Object.freeze({
    runtimeId: input.runtimeId,
    modality: "llm",
    component: "llama-server",
    root: input.root,
    modelId: input.modelId,
    modelFile: input.modelFile,
    modelPath: join(input.modelsDirectory, input.modelFile),
    host: backendBindHost(input.host),
    port: input.port,
    healthUrl: `http://${urlHost(input.host)}:${input.port}/health`,
    ctxSize,
    parallel: Object.freeze({ ...parallel }),
    hardware: Object.freeze({ ...input.hardware }),
    embedding: input.embedding ? Object.freeze({ ...input.embedding }) : null,
    kvCache,
    kvGeometry,
    ...(input.trainingContextLength !== undefined
      ? { trainingContextLength: input.trainingContextLength }
      : {}),
    promptCacheRamMib,
    memoryDemand: llmMemoryDemand({
      model: input.model,
      artifactBytes: input.artifactBytes,
      ctxSize,
      parallel,
      kvCache,
      kvGeometry,
      promptCacheRamMib,
    }),
  });
}

export function resolveSttLaunchPlan(input: {
  runtimeId: string;
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
  model?: ModelSpec;
  artifactBytes: number;
}): SttLaunchPlan {
  return Object.freeze({
    runtimeId: input.runtimeId,
    modality: "stt",
    component: "whisper-server",
    root: input.root,
    modelId: input.modelId,
    modelFile: input.modelFile,
    modelPath: join(input.modelsDirectory, input.modelFile),
    host: backendBindHost(input.host),
    port: input.port,
    healthUrl: `http://${urlHost(input.host)}:${input.port}/health`,
    memoryDemand: sttMemoryDemand(input),
  });
}

export function resolveImageLaunchPlan(input: {
  runtimeId: string;
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
  model?: ModelSpec;
  artifactBytes: number;
  imageRuntime?: ImageRuntimeProfile;
}): ImageLaunchPlan {
  const imageRuntime = resolveImageRuntimeProfile(
    input.modelsDirectory,
    input.imageRuntime,
  );
  return Object.freeze({
    runtimeId: input.runtimeId,
    modality: "image",
    component: "sd-server",
    root: input.root,
    modelId: input.modelId,
    modelFile: input.modelFile,
    modelPath: join(input.modelsDirectory, input.modelFile),
    host: backendBindHost(input.host),
    port: input.port,
    healthUrl: `http://${urlHost(input.host)}:${input.port}/`,
    memoryDemand: imageMemoryDemand(input),
    ...(imageRuntime ? { imageRuntime } : {}),
  });
}

function resolveImageRuntimeProfile(
  modelsDirectory: string,
  profile: ImageRuntimeProfile | undefined,
): ImageRuntimeLaunchProfile | undefined {
  if (!profile) return undefined;
  switch (profile.kind) {
    case "diffusion-qwen3":
      return Object.freeze({
        kind: profile.kind,
        diffusionModelPath: join(
          modelsDirectory,
          profile.artifacts.diffusionModel,
        ),
        vaePath: join(modelsDirectory, profile.artifacts.vae),
        textEncoderPath: join(modelsDirectory, profile.artifacts.textEncoder),
        generation: Object.freeze({ ...profile.generation }),
      });
    case "diffusion-flux1":
      return Object.freeze({
        kind: profile.kind,
        diffusionModelPath: join(
          modelsDirectory,
          profile.artifacts.diffusionModel,
        ),
        vaePath: join(modelsDirectory, profile.artifacts.vae),
        clipLPath: join(modelsDirectory, profile.artifacts.clipL),
        t5xxlPath: join(modelsDirectory, profile.artifacts.t5xxl),
        generation: Object.freeze({ ...profile.generation }),
      });
    case "checkpoint":
      return Object.freeze({
        kind: profile.kind,
        diffusionModelPath: join(
          modelsDirectory,
          profile.artifacts.diffusionModel,
        ),
        generation: Object.freeze({ ...profile.generation }),
      });
  }
}

export function resolveVideoLaunchPlan(input: {
  runtimeId: string;
  root: string;
  modelsDirectory: string;
  modelId: string;
  diffusionModelFile: string;
  textEncoderFile: string;
  host: string;
  port: number;
  videoRuntime: VideoRuntimeProfile;
  target: VideoRuntimeTarget;
  /** Catalog entry; the derived demand reads its artifact sizes. */
  model: ModelSpec;
}): VideoLaunchPlan {
  if (
    !input.videoRuntime.supportedTargets.some(
      (candidate) =>
        candidate.platform === input.target.platform &&
        candidate.architecture === input.target.architecture &&
        candidate.accelerator === input.target.accelerator,
    )
  ) {
    throw new Error("Video model does not support this runtime target.");
  }
  const { qualification } = input.videoRuntime;
  const memoryDemand = catalogModelDemand({
    ...input.model,
    videoRuntime: input.videoRuntime,
  });
  const common = {
    runtimeId: input.runtimeId,
    modality: "video",
    component: "sd-server",
    root: input.root,
    modelId: input.modelId,
    diffusionModelPath: join(input.modelsDirectory, input.diffusionModelFile),
    textEncoderPath: join(input.modelsDirectory, input.textEncoderFile),
    inputBounds: Object.freeze({
      maxWidth: qualification.maxWidth,
      maxHeight: qualification.maxHeight,
      maxFrames: qualification.maxFrames,
      fps: qualification.fps,
    }),
    generation: Object.freeze({ ...qualification.generation }),
    launchOptions: Object.freeze({ ...qualification.launchOptions }),
    host: backendBindHost(input.host),
    port: input.port,
    healthUrl: `http://${urlHost(input.host)}:${input.port}/`,
    memoryDemand,
  } satisfies VideoLaunchPlanBase;
  return input.videoRuntime.mode === "s2v"
    ? Object.freeze({
        ...common,
        mode: "s2v",
        decoder: Object.freeze({
          kind: "vae",
          path: join(
            input.modelsDirectory,
            input.videoRuntime.artifacts.decoder.artifactFilename,
          ),
        }),
        audioEncoderPath: join(
          input.modelsDirectory,
          input.videoRuntime.artifacts.audioEncoder,
        ),
      })
    : Object.freeze({
        ...common,
        mode: "t2v",
        decoder: Object.freeze({
          kind: input.videoRuntime.artifacts.decoder.kind,
          path: join(
            input.modelsDirectory,
            input.videoRuntime.artifacts.decoder.artifactFilename,
          ),
        }),
      });
}
