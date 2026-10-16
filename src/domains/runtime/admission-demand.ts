import { primaryArtifact, type ModelSpec } from "../../catalog";
import { plannableFeedPeaks, type MemoryPeak } from "../../catalog/annotations";
import { feedPeakMatchesLaunch } from "../models/memory-fit";
import type { RuntimeLaunchPlan } from "./launch-plan";
import {
  MINIMUM_MEMORY_MARGIN_BYTES,
  MINIMUM_MEMORY_MARGIN_RATIO,
  measuredDemand,
  measurementDemandOptions,
  totalArtifactBytes,
} from "./memory-estimate";
import type { RuntimeMemoryDemand } from "./memory-safety";
import {
  DEFAULT_LAUNCH_SHAPE,
  planLaunchShape,
  type ObservedMemoryRecord,
  type ObservedMemoryStore,
} from "./observed-memory";

/** Where admission looks up the peaks this machine recorded. */
export type ObservedPeakSource = Pick<ObservedMemoryStore, "list">;

/** What a launch loads, as far as memory evidence is concerned. */
export type AdmissionLaunch = Readonly<{
  spec: ModelSpec | undefined;
  /** The demand the launch plan computed: checked-in measurement or estimate. */
  base: RuntimeMemoryDemand;
  /** Launch-shape key, as in `planLaunchShape`. */
  launchShape: string;
  /** False when the launch loads some file other than the catalog primary. */
  loadsPrimaryArtifact: boolean;
  /** Bytes on disk for the launch; defaults to the catalog artifact sizes. */
  artifactBytes?: number;
  /** Host prompt-cache allowance of the launch (LLM only). */
  promptCacheBytes?: number;
  observed?: ObservedPeakSource;
}>;

/** Margin over an observed peak: max(512 MiB, 10%), as in `minimumMemoryGb`. */
export function observedPeakMargin(peakBytes: number): number {
  return Math.max(
    MINIMUM_MEMORY_MARGIN_BYTES,
    peakBytes * MINIMUM_MEMORY_MARGIN_RATIO,
  );
}

/**
 * The observed peak for exactly this artifact and launch shape. Only macOS
 * `footprint` peaks count: they cover the whole unified demand, while Linux
 * VmHWM omits discrete GPU memory.
 */
export function observedAdmissionPeak(
  spec: ModelSpec | undefined,
  launchShape: string,
  observed: ObservedPeakSource | undefined,
): ObservedMemoryRecord | undefined {
  if (!spec || !observed) return undefined;
  const sha = primaryArtifact(spec).sha256;
  if (!sha) return undefined;
  return observed
    .list(spec.modelId, sha)
    .find(
      (record) =>
        record.method === "footprint" &&
        record.peakBytes > 0 &&
        record.launchShape === launchShape,
    );
}

/**
 * The largest feed peak published for the exact file and revision at this
 * launch shape. LLM peaks must match the planned context, slots and KV type;
 * other kinds use the shape the catalog entry pins.
 */
export function feedAdmissionPeak(
  spec: ModelSpec | undefined,
  launchShape: string,
): MemoryPeak | undefined {
  if (!spec) return undefined;
  return plannableFeedPeaks(spec)
    .filter((peak) =>
      spec.kind === "llm"
        ? peak.launchShape === launchShape
        : feedPeakMatchesLaunch(spec, peak.launchShape),
    )
    .reduce<MemoryPeak | undefined>(
      (best, peak) => (!best || peak.peakBytes > best.peakBytes ? peak : best),
      undefined,
    );
}

/**
 * Memory demand admission plans a launch with, most trusted first:
 * 1. observed: the footprint peak this machine recorded for the same artifact
 *    and launch shape, plus max(512 MiB, 10%). Runtime always wins, so it
 *    replaces every other figure, larger or smaller.
 * 2. measured: a checked-in calibration run (already in `base`).
 * 3. feed: a peak the catalog feed publishes for exactly this file, revision
 *    and launch shape.
 * 4. estimate: the first-principles figure (already in `base`).
 * Evidence only applies when the launch loads the catalog primary artifact.
 * The memory-pressure monitor that runs after load is unaffected.
 */
export function admissionDemand(launch: AdmissionLaunch): RuntimeMemoryDemand {
  const { spec, base } = launch;
  if (!spec || !launch.loadsPrimaryArtifact) return base;

  const peak = observedAdmissionPeak(spec, launch.launchShape, launch.observed);
  if (peak) {
    const bytes = Math.ceil(
      peak.peakBytes + observedPeakMargin(peak.peakBytes),
    );
    return Object.freeze({
      unifiedBytes: bytes,
      hostBytes: bytes,
      acceleratorBytes: bytes,
      confidence: "observed",
    });
  }

  if (base.confidence === "measured") return base;

  const feedPeak = feedAdmissionPeak(spec, launch.launchShape);
  if (!feedPeak) return base;
  const catalogBytes = totalArtifactBytes(spec);
  const artifactBytes = Math.max(
    launch.artifactBytes ?? catalogBytes,
    catalogBytes,
  );
  const feed = measuredDemand(feedPeak, {
    ...measurementDemandOptions(spec, catalogBytes),
    promptCacheBytes: launch.promptCacheBytes ?? 0,
  });
  // The feed measured the catalog file; a larger local file adds its extra.
  const extra = artifactBytes - catalogBytes;
  return Object.freeze({
    unifiedBytes: feed.unifiedBytes + extra,
    hostBytes: feed.hostBytes + extra,
    acceleratorBytes: feed.acceleratorBytes + extra,
    confidence: "feed",
  });
}

/** Whether a plan loads the catalog primary artifact of `spec`. */
function planLoadsPrimary(spec: ModelSpec, plan: RuntimeLaunchPlan): boolean {
  if (spec.kind !== plan.modality) return false;
  return (
    !("modelFile" in plan) || plan.modelFile === primaryArtifact(spec).filename
  );
}

/** A launch plan whose demand reflects the best memory evidence available. */
export function withAdmissionDemand<Plan extends RuntimeLaunchPlan>(
  plan: Plan,
  spec: ModelSpec | undefined,
  evidence: Readonly<{
    observed?: ObservedPeakSource;
    artifactBytes?: number;
  }> = {},
): Plan {
  if (!spec) return plan;
  const memoryDemand = admissionDemand({
    spec,
    base: plan.memoryDemand,
    launchShape: planLaunchShape(plan),
    loadsPrimaryArtifact: planLoadsPrimary(spec, plan),
    artifactBytes: evidence.artifactBytes,
    promptCacheBytes:
      plan.modality === "llm" ? plan.promptCacheRamMib * 1024 * 1024 : 0,
    observed: evidence.observed,
  });
  return memoryDemand === plan.memoryDemand
    ? plan
    : (Object.freeze({ ...plan, memoryDemand }) as Plan);
}

/** Demand for a TTS model before its warm server plan exists. */
export function ttsAdmissionDemand(
  spec: ModelSpec,
  base: RuntimeMemoryDemand,
  evidence: Readonly<{
    observed?: ObservedPeakSource;
    loadsPrimaryArtifact?: boolean;
  }> = {},
): RuntimeMemoryDemand {
  return admissionDemand({
    spec,
    base,
    launchShape: DEFAULT_LAUNCH_SHAPE,
    loadsPrimaryArtifact: evidence.loadsPrimaryArtifact ?? true,
    observed: evidence.observed,
  });
}
