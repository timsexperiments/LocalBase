import { join } from "node:path";
import {
  allocateParallelSlots,
  CONTEXT_MEMORY_GB_PER_8K_TOKENS,
  PARALLEL_SLOT_OVERHEAD_GB,
  type ParallelAllocation,
  type ParallelSlots,
} from "../config/parallel";
import type { VideoRuntimeProfile, VideoRuntimeTarget } from "../../catalog";
import type { RuntimeComponent, RuntimeModality } from "./modality";
import { gibibyte, type RuntimeMemoryDemand } from "./memory-safety";

const RUNTIME_HOST_OVERHEAD_BYTES = 512 * 1024 * 1024;

export type RuntimeHardware = { memoryGb: number };

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
  readonly parallel: ParallelAllocation;
  readonly modelRequirementGb: number | undefined;
  readonly hardware: Readonly<RuntimeHardware>;
};

export type SttLaunchPlan = LaunchPlanBase<"stt", "whisper-server">;

export type ImageLaunchPlan = LaunchPlanBase<"image", "sd-server">;

export type VideoLaunchPlan = Omit<
  LaunchPlanBase<"video", "sd-server">,
  "modelFile" | "modelPath"
> & {
  readonly diffusionModelPath: string;
  readonly textEncoderPath: string;
  readonly vaePath: string;
  readonly inputBounds: Readonly<{
    maxWidth: number;
    maxHeight: number;
    maxFrames: number;
    fps: number;
  }>;
  readonly generation: Readonly<{
    sampler: "euler";
    steps: number;
    cfgScale: number;
    flowShift: number;
    seed: number;
  }>;
  readonly launchOptions: Readonly<{
    cpuOffload: boolean;
    diffusionFlashAttention: boolean;
  }>;
};

export type RuntimeLaunchPlan =
  LlmLaunchPlan | SttLaunchPlan | ImageLaunchPlan | VideoLaunchPlan;

function modelBytes(
  artifactBytes: number,
  modelRequirementGb: number | undefined,
): number {
  return Math.max(
    artifactBytes,
    Math.ceil((modelRequirementGb ?? 0) * gibibyte),
  );
}

function runtimeMemoryDemand(input: {
  artifactBytes: number;
  modelRequirementGb: number | undefined;
}): RuntimeMemoryDemand {
  const requirementBytes = modelBytes(
    input.artifactBytes,
    input.modelRequirementGb,
  );
  return Object.freeze({
    unifiedBytes: requirementBytes + RUNTIME_HOST_OVERHEAD_BYTES,
    hostBytes: input.artifactBytes + RUNTIME_HOST_OVERHEAD_BYTES,
    acceleratorBytes: requirementBytes,
    confidence: "estimated",
  });
}

function videoMemoryDemand(input: {
  estimatedDemand: VideoRuntimeProfile["estimatedMemoryDemand"];
}): RuntimeMemoryDemand {
  return Object.freeze({
    ...input.estimatedDemand,
    confidence: "estimated",
  });
}

function llmMemoryDemand(input: {
  artifactBytes: number;
  modelRequirementGb: number | undefined;
  ctxSize: number;
  parallel: ParallelAllocation;
  hardware: RuntimeHardware;
}): RuntimeMemoryDemand {
  const contextBytes = Math.ceil(
    (input.ctxSize / 8192) * CONTEXT_MEMORY_GB_PER_8K_TOKENS * gibibyte,
  );
  const slotBytes = Math.ceil(
    input.parallel.slots * PARALLEL_SLOT_OVERHEAD_GB * gibibyte,
  );
  const requirementBytes = modelBytes(
    input.artifactBytes,
    input.modelRequirementGb,
  );
  return Object.freeze({
    unifiedBytes:
      requirementBytes + contextBytes + slotBytes + RUNTIME_HOST_OVERHEAD_BYTES,
    hostBytes: input.artifactBytes + RUNTIME_HOST_OVERHEAD_BYTES + contextBytes,
    acceleratorBytes: requirementBytes + contextBytes + slotBytes,
    confidence: "estimated",
  });
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
  parallel: ParallelSlots;
  modelRequirementGb: number | undefined;
  artifactBytes: number;
  hardware: RuntimeHardware;
}): LlmLaunchPlan {
  const parallel = allocateParallelSlots({
    parallel: input.parallel,
    memoryGb: input.hardware.memoryGb,
    modelRequirementGb: input.modelRequirementGb,
    ctxSize: input.ctxSize,
  });
  return Object.freeze({
    runtimeId: input.runtimeId,
    modality: "llm",
    component: "llama-server",
    root: input.root,
    modelId: input.modelId,
    modelFile: input.modelFile,
    modelPath: join(input.modelsDirectory, input.modelFile),
    host: input.host,
    port: input.port,
    healthUrl: `http://${input.host}:${input.port}/health`,
    ctxSize: input.ctxSize,
    parallel: Object.freeze({ ...parallel }),
    modelRequirementGb: input.modelRequirementGb,
    hardware: Object.freeze({ ...input.hardware }),
    memoryDemand: llmMemoryDemand({ ...input, parallel }),
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
  modelRequirementGb: number | undefined;
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
    host: input.host,
    port: input.port,
    healthUrl: `http://${input.host}:${input.port}/health`,
    memoryDemand: runtimeMemoryDemand(input),
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
  modelRequirementGb: number | undefined;
  artifactBytes: number;
}): ImageLaunchPlan {
  return Object.freeze({
    runtimeId: input.runtimeId,
    modality: "image",
    component: "sd-server",
    root: input.root,
    modelId: input.modelId,
    modelFile: input.modelFile,
    modelPath: join(input.modelsDirectory, input.modelFile),
    host: input.host,
    port: input.port,
    healthUrl: `http://${input.host}:${input.port}/`,
    memoryDemand: runtimeMemoryDemand(input),
  });
}

export function resolveVideoLaunchPlan(input: {
  runtimeId: string;
  root: string;
  modelsDirectory: string;
  modelId: string;
  diffusionModelFile: string;
  textEncoderFile: string;
  vaeFile: string;
  host: string;
  port: number;
  videoRuntime: VideoRuntimeProfile;
  target: VideoRuntimeTarget;
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
  const memoryDemand = videoMemoryDemand({
    estimatedDemand: input.videoRuntime.estimatedMemoryDemand,
  });
  return Object.freeze({
    runtimeId: input.runtimeId,
    modality: "video",
    component: "sd-server",
    root: input.root,
    modelId: input.modelId,
    diffusionModelPath: join(input.modelsDirectory, input.diffusionModelFile),
    textEncoderPath: join(input.modelsDirectory, input.textEncoderFile),
    vaePath: join(input.modelsDirectory, input.vaeFile),
    inputBounds: Object.freeze({
      maxWidth: qualification.maxWidth,
      maxHeight: qualification.maxHeight,
      maxFrames: qualification.maxFrames,
      fps: qualification.fps,
    }),
    generation: Object.freeze({ ...qualification.generation }),
    launchOptions: Object.freeze({ ...qualification.launchOptions }),
    host: input.host,
    port: input.port,
    healthUrl: `http://${input.host}:${input.port}/`,
    memoryDemand,
  });
}
