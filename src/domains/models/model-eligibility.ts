import type { ModelSpec, VideoRuntimeTarget } from "../../catalog";

export const experimentalModelOptInRequired =
  "Experimental models require models.allowExperimental = true.";
export const unsupportedModelTargetReason =
  "Requires Linux x64 with a single NVIDIA GPU.";

export function modelEligibilityReason(
  model: ModelSpec,
  options: {
    allowExperimental?: boolean;
    target?: VideoRuntimeTarget;
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
  if (options.target) {
    const { platform, architecture, accelerator } = options.target;
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
  if (reason) throw new Error(`${model.modelId}: ${reason}`);
}
