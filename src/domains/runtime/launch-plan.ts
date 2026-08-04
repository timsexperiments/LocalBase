import { join } from "node:path";
import type { ParallelSlots } from "../config/parallel";
import type { RuntimeComponent, RuntimeModality } from "./modality";

export type RuntimeHardware = { memoryGb: number };

type LaunchPlanBase<
  Modality extends RuntimeModality,
  Component extends RuntimeComponent,
> = {
  readonly modality: Modality;
  readonly component: Component;
  readonly root: string;
  readonly modelId: string;
  readonly modelFile: string;
  readonly modelPath: string;
  readonly host: string;
  readonly port: number;
  readonly healthUrl: string;
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

export function resolveLlmLaunchPlan(input: {
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
  ctxSize: number;
  parallel: ParallelSlots;
  modelRequirementGb: number | undefined;
  hardware: RuntimeHardware;
}): LlmLaunchPlan {
  return Object.freeze({
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
  });
}

export function resolveSttLaunchPlan(input: {
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
}): SttLaunchPlan {
  return Object.freeze({
    modality: "stt",
    component: "whisper-server",
    root: input.root,
    modelId: input.modelId,
    modelFile: input.modelFile,
    modelPath: join(input.modelsDirectory, input.modelFile),
    host: input.host,
    port: input.port,
    healthUrl: `http://${input.host}:${input.port}/health`,
  });
}

export function resolveImageLaunchPlan(input: {
  root: string;
  modelsDirectory: string;
  modelId: string;
  modelFile: string;
  host: string;
  port: number;
}): ImageLaunchPlan {
  return Object.freeze({
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
  });
}
