import { join } from "node:path";
import { CATALOG, primaryArtifact, type ModelSpec } from "../../catalog";
import {
  feedAnnotationsGeneration,
  plannableFeedPeaks,
  type MemoryPeak,
} from "../../catalog/annotations";
import type { LlmKvGeometry } from "../runtime/gguf-metadata";
import { gibibyte } from "../runtime/memory-safety";
import {
  catalogLlmReference,
  estimateCatalogModelMemory,
  measuredCatalogModelMemory,
  measuredDemand,
  measurementDemandOptions,
  minimumMemoryGb,
  type CatalogEstimateInputs,
} from "../runtime/memory-estimate";
import {
  DEFAULT_LAUNCH_SHAPE,
  llmLaunchShape,
  type ObservedMemoryRecord,
  type ObservedMemoryStore,
} from "../runtime/observed-memory";

/**
 * Where a memory figure came from, most trusted first:
 * - `observed`: the peak this machine recorded while serving the model
 *   (model_observed_memory table, runtime/observed-memory.ts)
 * - `measured`: a calibration run checked into memory-measurements.ts
 * - `feed`: a peak the catalog feed publishes for exactly this file at its
 *   pinned revision, measured at the launch shape LocalBase plans
 *   (catalog/annotations.ts)
 * - `estimate`: the first-principles estimate from artifact sizes, plus the
 *   GGUF geometry when the file is installed
 *
 * Runtime always wins: once this machine records a peak, it replaces the
 * measured, feed and estimated figures.
 */
export type MemorySource = "estimate" | "feed" | "measured" | "observed";

export type ModelMemoryInputs = CatalogEstimateInputs &
  Readonly<{
    /** Peaks recorded for the installed artifact, newest first. */
    observed?: readonly ObservedMemoryRecord[];
  }>;

export type ModelMemoryFigure = Readonly<{
  source: MemorySource;
  /** Peak demand in bytes the minimum is computed from. */
  demandBytes: number;
  /** Whole GiB including the safety margin; the legacy `minVramGb`. */
  minVramGb: number;
  /** Estimate or measurement at the reference shape, before any observation. */
  referenceBytes: number;
  referenceSource: Exclude<MemorySource, "observed">;
  /** The launch shape the figure describes. */
  launchShape: string;
  /** The observed peak used, when there is one. */
  observed?: ObservedMemoryRecord;
  /** The feed peak behind the reference figure, when it came from the feed. */
  feedPeak?: MemoryPeak;
}>;

/** Launch-shape key of the catalog reference launch for a model. */
export function referenceLaunchShape(
  spec: ModelSpec,
  inputs: CatalogEstimateInputs = {},
): string {
  if (spec.kind !== "llm") return DEFAULT_LAUNCH_SHAPE;
  const reference = catalogLlmReference(spec, inputs);
  return llmLaunchShape({
    ctxSize: reference.ctxTokens,
    slots: reference.slots,
    typeK: reference.cacheTypeK,
    typeV: reference.cacheTypeV,
  });
}

/**
 * Picks the observed peak that stands for the model: the one at the reference
 * shape, else the newest. Only `footprint` peaks count: they cover the whole
 * unified-memory demand, while Linux VmHWM omits discrete GPU memory.
 */
export function selectObservedPeak(
  observed: readonly ObservedMemoryRecord[] | undefined,
  referenceShape: string,
): ObservedMemoryRecord | undefined {
  const usable = (observed ?? []).filter(
    (record) => record.method === "footprint" && record.peakBytes > 0,
  );
  return (
    usable.find((record) => record.launchShape === referenceShape) ?? usable[0]
  );
}

const staticFigures = new Map<string, ModelMemoryFigure>();
let staticGeneration = -1;

const onOff = (value: boolean | undefined) => (value ? "on" : "off");

/**
 * Whether a feed peak describes the launch LocalBase plans for `spec`,
 * mirroring how the checked-in measurements are matched: LLMs at the exact
 * reference shape, image at the same `--diffusion-fa` setting, video at its
 * qualification bounds and options, speech at its one fixed shape.
 */
export function feedPeakMatchesLaunch(
  spec: ModelSpec,
  launchShape: string,
  inputs: CatalogEstimateInputs = {},
): boolean {
  switch (spec.kind) {
    case "llm":
      return launchShape === referenceLaunchShape(spec, inputs);
    case "stt":
      return launchShape === DEFAULT_LAUNCH_SHAPE;
    case "tts":
      return /^ctx=\d+$/.test(launchShape);
    case "image":
      return launchShape.endsWith(
        `;diffusion-fa=${onOff(spec.imageLaunchOptions?.diffusionFlashAttention)}`,
      );
    case "video": {
      const qualification = spec.videoRuntime?.qualification;
      if (!qualification) return false;
      const options = qualification.launchOptions;
      return (
        launchShape ===
        `${qualification.maxWidth}x${qualification.maxHeight};frames=${qualification.maxFrames};cpu-offload=${onOff(options.cpuOffload)};diffusion-fa=${onOff(options.diffusionFlashAttention)}`
      );
    }
  }
}

/**
 * The feed peak memory planning uses for `spec`: the largest one published
 * for its exact file and revision at the planned launch shape.
 */
export function selectFeedPeak(
  spec: ModelSpec,
  inputs: CatalogEstimateInputs = {},
): MemoryPeak | undefined {
  return plannableFeedPeaks(spec)
    .filter((peak) => feedPeakMatchesLaunch(spec, peak.launchShape, inputs))
    .reduce<MemoryPeak | undefined>(
      (best, peak) => (!best || peak.peakBytes > best.peakBytes ? peak : best),
      undefined,
    );
}

/**
 * The memory figure for a model on this machine. Precedence:
 * observed > measured > feed > estimate. The accelerator demand drives the
 * minimum, as the hand-entered `minVramGb` tiers did.
 */
export function modelMemoryFigure(
  spec: ModelSpec,
  inputs: ModelMemoryInputs = {},
): ModelMemoryFigure {
  const cacheable = !inputs.geometry && !inputs.observed?.length;
  if (staticGeneration !== feedAnnotationsGeneration()) {
    staticFigures.clear();
    staticGeneration = feedAnnotationsGeneration();
  }
  if (cacheable) {
    const cached = staticFigures.get(spec.modelId);
    if (cached) return cached;
  }
  const estimate = estimateCatalogModelMemory(spec, inputs);
  const measured = measuredCatalogModelMemory(spec, inputs);
  const feedPeak = measured ? undefined : selectFeedPeak(spec, inputs);
  const feed = feedPeak
    ? measuredDemand(feedPeak, measurementDemandOptions(spec))
    : undefined;
  const referenceBytes =
    measured?.acceleratorBytes ??
    feed?.acceleratorBytes ??
    estimate.demand.acceleratorBytes;
  const referenceSource = measured ? "measured" : feed ? "feed" : "estimate";
  const launchShape = referenceLaunchShape(spec, inputs);
  const observed = selectObservedPeak(inputs.observed, launchShape);
  const reference = feedPeak ? { feedPeak } : {};
  const figure: ModelMemoryFigure = Object.freeze(
    observed
      ? {
          source: "observed",
          demandBytes: observed.peakBytes,
          minVramGb: minimumMemoryGb(observed.peakBytes),
          referenceBytes,
          referenceSource,
          launchShape: observed.launchShape,
          observed,
          ...reference,
        }
      : {
          source: referenceSource,
          demandBytes: referenceBytes,
          minVramGb: minimumMemoryGb(referenceBytes),
          referenceBytes,
          referenceSource,
          launchShape: feedPeak?.launchShape ?? launchShape,
          ...reference,
        },
  );
  if (cacheable) staticFigures.set(spec.modelId, figure);
  return figure;
}

/** Computed minimum memory (GiB) from the catalog alone. */
export function computedMinVramGb(spec: ModelSpec): number {
  return modelMemoryFigure(spec).minVramGb;
}

export function formatMemoryGb(bytes: number): string {
  return `${(bytes / gibibyte).toFixed(1)} GB`;
}

/** One-line description of a figure, such as `3 GB (observed, ctx=8192;...)`. */
export function describeMemoryFigure(figure: ModelMemoryFigure): string {
  const detail =
    figure.source === "observed"
      ? `observed peak ${formatMemoryGb(figure.demandBytes)} at ${figure.launchShape}; ${figure.referenceSource} ${formatMemoryGb(figure.referenceBytes)}`
      : figure.source === "feed"
        ? `feed-measured peak ${formatMemoryGb(figure.feedPeak?.peakBytes ?? figure.demandBytes)} at ${figure.launchShape}`
        : `${figure.source} ${formatMemoryGb(figure.demandBytes)}`;
  return `${figure.minVramGb} GB (${detail})`;
}

export function recommendedForVram(vramGb: number): ModelSpec[] {
  return CATALOG.filter(
    (model) =>
      model.kind === "llm" &&
      !model.llmRuntime &&
      computedMinVramGb(model) <= vramGb,
  ).sort((a, b) => computedMinVramGb(b) - computedMinVramGb(a));
}

function recommendedKindForVram(
  kind: ModelSpec["kind"],
  vramGb: number,
): ModelSpec[] {
  return CATALOG.filter(
    (m) => m.kind === kind && computedMinVramGb(m) <= vramGb,
  ).sort((a, b) => a.storageGb - b.storageGb);
}

export function recommendedSttForVram(vramGb: number): ModelSpec[] {
  return recommendedKindForVram("stt", vramGb);
}

export function recommendedTtsForVram(vramGb: number): ModelSpec[] {
  return recommendedKindForVram("tts", vramGb);
}

export function recommendedImageForVram(vramGb: number): ModelSpec[] {
  return recommendedKindForVram("image", vramGb);
}

export type MemoryFitStatus = "perfect" | "tight" | "insufficient";
export type MemoryFitVerdict = "fits" | "tight" | "wont-fit";

const verdicts: Readonly<Record<MemoryFitStatus, MemoryFitVerdict>> = {
  perfect: "fits",
  tight: "tight",
  insufficient: "wont-fit",
};

export type MemoryFitEvaluation = {
  status: MemoryFitStatus;
  verdict: MemoryFitVerdict;
  minVramGb: number;
  systemVramGb: number;
  headroomGb: number;
  source: MemorySource;
  figure: ModelMemoryFigure;
  message: string;
};

/** Fit verdict from a minimum and this machine's memory. */
export function fitStatus(
  minVramGb: number,
  systemVramGb: number,
): MemoryFitStatus {
  if (systemVramGb < minVramGb) return "insufficient";
  const headroomGb = systemVramGb - minVramGb;
  if (headroomGb < 4 || (systemVramGb > 0 && minVramGb / systemVramGb > 0.75))
    return "tight";
  return "perfect";
}

export function evaluateModelFit(
  model: ModelSpec,
  systemVramGb: number,
  inputs: ModelMemoryInputs = {},
): MemoryFitEvaluation {
  const figure = modelMemoryFigure(model, inputs);
  const minVramGb = figure.minVramGb;
  const headroomGb = systemVramGb - minVramGb;
  const status = fitStatus(minVramGb, systemVramGb);
  const message =
    status === "insufficient"
      ? `Requires ${minVramGb}GB (${figure.source}), you have ${systemVramGb}GB`
      : `Leaves ${headroomGb.toFixed(1)}GB headroom (${figure.source})`;
  return {
    status,
    verdict: verdicts[status],
    minVramGb,
    systemVramGb,
    headroomGb,
    source: figure.source,
    figure,
    message,
  };
}

/** Whether a model's primary file is a GGUF whose KV geometry sharpens the estimate. */
function readsGeometry(spec: ModelSpec): boolean {
  return (
    (spec.kind === "llm" || spec.ttsRuntime?.kind === "llama-tts") &&
    primaryArtifact(spec).filename.endsWith(".gguf")
  );
}

/**
 * Local facts for every catalog model: the GGUF geometry of installed LLM and
 * talker files, and the peaks this machine recorded for the pinned artifact.
 * Models that are not installed get no inputs and fall back to the estimate.
 */
export async function collectModelMemoryInputs(
  input: Readonly<{
    catalog: readonly ModelSpec[];
    installations: ReadonlyMap<string, boolean>;
    directoryFor: (spec: ModelSpec) => string;
    observed?: ObservedMemoryStore;
    readGeometry?: (path: string) => Promise<LlmKvGeometry | null>;
  }>,
): Promise<ReadonlyMap<string, ModelMemoryInputs>> {
  const entries = await Promise.all(
    input.catalog.map(
      async (spec): Promise<readonly [string, ModelMemoryInputs]> => {
        if (!input.installations.get(spec.modelId)) return [spec.modelId, {}];
        const primary = primaryArtifact(spec);
        const geometry =
          input.readGeometry && readsGeometry(spec)
            ? await input
                .readGeometry(join(input.directoryFor(spec), primary.filename))
                .catch(() => null)
            : null;
        const observed =
          input.observed && primary.sha256
            ? input.observed.list(spec.modelId, primary.sha256)
            : [];
        return [spec.modelId, { geometry, observed }];
      },
    ),
  );
  return new Map(entries);
}

/**
 * Observed peaks for every catalog model's pinned artifact, without touching
 * the model files. The CLI uses this when the gateway database already exists.
 */
export function observedMemoryInputs(
  catalog: readonly ModelSpec[],
  store: ObservedMemoryStore,
): ReadonlyMap<string, ModelMemoryInputs> {
  return new Map(
    catalog.map((spec) => {
      const sha256 = primaryArtifact(spec).sha256;
      return [
        spec.modelId,
        sha256 ? { observed: store.list(spec.modelId, sha256) } : {},
      ] as const;
    }),
  );
}

/** Additive memory fields for CLI `--json` model listings. */
export type ModelMemoryJson = Readonly<{
  minVramGb: number;
  minimumVramEstimateGb: number;
  memorySource: MemorySource;
  memoryLaunchShape: string;
  observedPeakBytes: number | null;
  fitVerdict: MemoryFitVerdict;
  systemMemoryGb: number;
}>;

export function modelMemoryJson(fit: MemoryFitEvaluation): ModelMemoryJson {
  return {
    minVramGb: fit.minVramGb,
    minimumVramEstimateGb: fit.minVramGb,
    memorySource: fit.source,
    memoryLaunchShape: fit.figure.launchShape,
    observedPeakBytes: fit.figure.observed?.peakBytes ?? null,
    fitVerdict: fit.verdict,
    systemMemoryGb: fit.systemVramGb,
  };
}
