import type { ModelSpec, VideoRuntimeTarget } from "../../catalog";

export const experimentalModelOptInRequired =
  "Experimental models require models.allowExperimental = true.";
export const unsupportedModelTargetReason =
  "Requires Linux x64 with a single NVIDIA GPU.";

export class ModelHardwareIneligibleError extends Error {
  constructor(modelId: string, reason: string) {
    super(`${modelId}: ${reason}`);
    this.name = "ModelHardwareIneligibleError";
  }
}

export function videoTargetFromTopology(
  topology:
    | { kind: "unified" }
    | { kind: "discrete"; accelerators: readonly { id: string }[] },
  host: Readonly<{
    platform: NodeJS.Platform;
    arch: NodeJS.Architecture;
  }> = process,
): VideoRuntimeTarget | null {
  if (
    host.platform === "linux" &&
    host.arch === "x64" &&
    topology.kind === "discrete" &&
    topology.accelerators.length === 1 &&
    topology.accelerators[0]?.id.startsWith("nvidia:")
  )
    return { platform: "linux", architecture: "x64", accelerator: "nvidia" };
  if (
    host.platform === "darwin" &&
    host.arch === "arm64" &&
    topology.kind === "unified"
  )
    return {
      platform: "darwin",
      architecture: "arm64",
      accelerator: "apple-unified",
    };
  return null;
}

export function modelEligibilityReason(
  model: ModelSpec,
  options: {
    allowExperimental?: boolean;
    target?: VideoRuntimeTarget | null;
    platform?: NodeJS.Platform;
    architecture?: NodeJS.Architecture;
  } = {},
): string | null {
  if (
    model.qualificationState === "experimental" &&
    options.allowExperimental !== true
  )
    return experimentalModelOptInRequired;

  const profile = model.videoRuntime;
  if (!profile) return null;
  if (Object.hasOwn(options, "target")) {
    if (options.target === null) return unsupportedModelTargetReason;
    const target = options.target;
    if (!target) return unsupportedModelTargetReason;
    const { platform, architecture, accelerator } = target;
    return profile.supportedTargets.some(
      (candidate) =>
        candidate.platform === platform &&
        candidate.architecture === architecture &&
        candidate.accelerator === accelerator,
    )
      ? null
      : unsupportedModelTargetReason;
  }
  if (options.platform || options.architecture) {
    const platform = options.platform ?? process.platform;
    const architecture = options.architecture ?? process.arch;
    return profile.supportedTargets.some(
      (candidate) =>
        candidate.platform === platform &&
        candidate.architecture === architecture,
    )
      ? null
      : unsupportedModelTargetReason;
  }
  return null;
}

export function assertModelEligible(
  model: ModelSpec,
  options?: Parameters<typeof modelEligibilityReason>[1],
): void {
  const reason = modelEligibilityReason(model, options);
  if (reason === unsupportedModelTargetReason)
    throw new ModelHardwareIneligibleError(model.modelId, reason);
  if (reason) throw new Error(`${model.modelId}: ${reason}`);
}
