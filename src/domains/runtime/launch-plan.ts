import { join } from "node:path";
import {
  allocateParallelSlots,
  CONTEXT_MEMORY_GB_PER_8K_TOKENS,
  PARALLEL_SLOT_OVERHEAD_GB,
  type ParallelSlots,
} from "../config/parallel";
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
  readonly parallel: ParallelSlots;
  readonly modelRequirementGb: number | undefined;
  readonly hardware: Readonly<RuntimeHardware>;
};

export type SttLaunchPlan = LaunchPlanBase<"stt", "whisper-server">;

export type ImageLaunchPlan = LaunchPlanBase<"image", "sd-server"> & {
  readonly workingDirectory: string;
};

export type RuntimeLaunchPlan = LlmLaunchPlan | SttLaunchPlan | ImageLaunchPlan;

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
    confidence: "authoritative",
  });
}

function llmMemoryDemand(input: {
  artifactBytes: number;
  modelRequirementGb: number | undefined;
  ctxSize: number;
  parallel: ParallelSlots;
  hardware: RuntimeHardware;
}): RuntimeMemoryDemand {
  const parallel = allocateParallelSlots({
    parallel: input.parallel,
    memoryGb: input.hardware.memoryGb,
    modelRequirementGb: input.modelRequirementGb,
    ctxSize: input.ctxSize,
  });
  const contextBytes = Math.ceil(
    (input.ctxSize / 8192) * CONTEXT_MEMORY_GB_PER_8K_TOKENS * gibibyte,
  );
  const slotBytes = Math.ceil(
    parallel.slots * PARALLEL_SLOT_OVERHEAD_GB * gibibyte,
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
    confidence: "authoritative",
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
    parallel: input.parallel,
    modelRequirementGb: input.modelRequirementGb,
    hardware: Object.freeze({ ...input.hardware }),
    memoryDemand: llmMemoryDemand(input),
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
    workingDirectory: join(input.root, "bin"),
    memoryDemand: runtimeMemoryDemand(input),
  });
}
