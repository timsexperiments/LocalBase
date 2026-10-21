import { afterAll, afterEach, describe, expect, test } from "bun:test";
import { byId, primaryArtifact, type ModelSpec } from "../../catalog";
import {
  huggingFaceRepoOf,
  setFeedAnnotations,
  type FeedAnnotation,
  type MemoryPeak,
} from "../../catalog/annotations";
import {
  admissionDemand,
  observedPeakMargin,
  ttsAdmissionDemand,
  withAdmissionDemand,
  type ObservedPeakSource,
} from "./admission-demand";
import { resolveLlmLaunchPlan, resolveSttLaunchPlan } from "./launch-plan";
import {
  catalogModelDemand,
  measuredDemand,
  measurementDemandOptions,
  MINIMUM_MEMORY_MARGIN_BYTES,
  totalArtifactBytes,
} from "./memory-estimate";
import { planLaunchShape, type ObservedMemoryRecord } from "./observed-memory";
import {
  keepFormerCatalogEntries,
  restoreBuiltInCatalog,
} from "../../test/former-catalog";

// Some fixtures are former built-in entries an upgraded install kept.
keepFormerCatalogEntries();
afterAll(restoreBuiltInCatalog);

const gib = 1024 ** 3;
const mib = 1024 ** 2;
const llm = byId("qwen2.5-coder-1.5b-instruct-q4_k_m")!;
const stt = byId("whisper-base-en-q8_0")!;
const measuredStt = byId("whisper-tiny-en-q8_0")!;
const measuredTts = byId("kokoro-82m-v1.0-q8")!;

function llmPlan(ctxSize = 8192, slots: 1 | 2 = 1) {
  return resolveLlmLaunchPlan({
    runtimeId: "llm:test:1",
    root: "/tmp/lb-admission-test",
    modelsDirectory: "/tmp/lb-admission-test/models",
    modelId: llm.modelId,
    modelFile: primaryArtifact(llm).filename,
    host: "127.0.0.1",
    port: 24901,
    ctxSize,
    parallel: slots,
    model: llm,
    artifactBytes: totalArtifactBytes(llm),
    hardware: { memoryGb: 64 },
  });
}

function sttPlan(
  model: ModelSpec,
  modelFile = primaryArtifact(model).filename,
) {
  return resolveSttLaunchPlan({
    runtimeId: "stt:test:1",
    root: "/tmp/lb-admission-test",
    modelsDirectory: "/tmp/lb-admission-test/models",
    modelId: model.modelId,
    modelFile,
    host: "127.0.0.1",
    port: 24902,
    model,
    artifactBytes: totalArtifactBytes(model),
  });
}

function feedPeak(
  launchShape: string,
  overrides: Partial<MemoryPeak> = {},
): MemoryPeak {
  return {
    variant: "Q4_K_M",
    launchShape,
    peakBytes: 3 * gib,
    backendAcceleratorBytes: 2 * gib,
    hostBufferBytes: 256 * mib,
    peakRssBytes: 3 * gib,
    measuredAt: "2026-10-14",
    hardware: "Feed machine",
    ...overrides,
  };
}

function publish(
  model: ModelSpec,
  peaks: MemoryPeak[],
  overrides: Partial<FeedAnnotation> = {},
): void {
  setFeedAnnotations([
    {
      repo: huggingFaceRepoOf(model.source)!,
      revision: model.repositoryRevision,
      file: primaryArtifact(model).sourcePath,
      modelId: model.modelId,
      memoryPeaks: peaks,
      ...overrides,
    },
  ]);
}

function observed(
  model: ModelSpec,
  records: Partial<ObservedMemoryRecord>[],
): ObservedPeakSource & { calls: string[] } {
  const calls: string[] = [];
  const sha = primaryArtifact(model).sha256!;
  return {
    calls,
    list: (modelId, artifactSha256) => {
      calls.push(`${modelId}@${artifactSha256}`);
      if (modelId !== model.modelId || artifactSha256 !== sha) return [];
      return records.map((record) => ({
        modelId,
        artifactSha256: sha,
        launchShape: "default",
        peakBytes: 4 * gib,
        method: "footprint",
        observedAt: "2026-10-15T00:00:00.000Z",
        ...record,
      }));
    },
  };
}

afterEach(() => setFeedAnnotations([]));

describe("admission demand precedence", () => {
  test("with no evidence the plan keeps its estimate", () => {
    const plan = llmPlan();
    const admitted = withAdmissionDemand(plan, llm, {
      observed: observed(llm, []),
    });
    expect(admitted).toBe(plan);
    expect(admitted.memoryDemand.confidence).not.toBe("feed");
  });

  test("an exact feed peak replaces the estimate", () => {
    const plan = llmPlan();
    const shape = planLaunchShape(plan);
    publish(llm, [feedPeak(shape)]);
    const admitted = withAdmissionDemand(plan, llm);
    const expected = measuredDemand(feedPeak(shape), {
      ...measurementDemandOptions(llm),
      promptCacheBytes: plan.promptCacheRamMib * mib,
    });
    expect(admitted.memoryDemand).toEqual({
      ...expected,
      confidence: "feed",
    });
    // The rest of the plan is untouched.
    expect({ ...admitted, memoryDemand: plan.memoryDemand }).toEqual(plan);
  });

  test("the largest matching feed peak wins", () => {
    const plan = llmPlan();
    const shape = planLaunchShape(plan);
    publish(llm, [
      feedPeak(shape),
      feedPeak(shape, { backendAcceleratorBytes: 5 * gib, peakBytes: 6 * gib }),
    ]);
    const demand = withAdmissionDemand(plan, llm).memoryDemand;
    expect(demand.confidence).toBe("feed");
    expect(demand.acceleratorBytes).toBeGreaterThanOrEqual(5 * gib);
  });

  test("a feed peak at another launch shape is ignored", () => {
    const plan = llmPlan(8192, 1);
    publish(llm, [feedPeak(planLaunchShape(llmPlan(16384, 2)))]);
    expect(withAdmissionDemand(plan, llm)).toBe(plan);
  });

  test("a feed annotation for another revision is ignored", () => {
    const plan = llmPlan();
    publish(llm, [feedPeak(planLaunchShape(plan))], {
      revision: "0".repeat(40),
    });
    expect(withAdmissionDemand(plan, llm)).toBe(plan);
  });

  test("a feed annotation whose recipe sha differs is ignored", () => {
    const plan = llmPlan();
    publish(llm, [feedPeak(planLaunchShape(plan))], {
      recipe: {
        modelId: llm.modelId,
        totalBytes: 1,
        files: [
          {
            repositoryUrl: llm.source,
            revision: llm.repositoryRevision,
            path: primaryArtifact(llm).sourcePath,
            sizeBytes: 1,
            sha256: "f".repeat(64),
            role: "primary",
          },
        ],
      },
    });
    expect(withAdmissionDemand(plan, llm)).toBe(plan);
  });

  test("a checked-in measurement beats the feed", () => {
    const plan = sttPlan(measuredStt);
    expect(plan.memoryDemand.confidence).toBe("measured");
    publish(measuredStt, [feedPeak("default", { peakBytes: 20 * gib })]);
    expect(withAdmissionDemand(plan, measuredStt)).toBe(plan);
  });

  test("a non-LLM feed peak applies at the pinned shape", () => {
    const plan = sttPlan(stt);
    publish(stt, [feedPeak("default")]);
    const demand = withAdmissionDemand(plan, stt).memoryDemand;
    expect(demand.confidence).toBe("feed");
    expect(demand).toEqual({
      ...measuredDemand(feedPeak("default"), measurementDemandOptions(stt)),
      confidence: "feed",
    });
  });

  test("an observed peak overrides a larger feed peak", () => {
    const plan = llmPlan();
    const shape = planLaunchShape(plan);
    publish(llm, [
      feedPeak(shape, {
        backendAcceleratorBytes: 9 * gib,
        peakBytes: 10 * gib,
      }),
    ]);
    const demand = withAdmissionDemand(plan, llm, {
      observed: observed(llm, [{ launchShape: shape, peakBytes: 2 * gib }]),
    }).memoryDemand;
    const bytes = Math.ceil(2 * gib + MINIMUM_MEMORY_MARGIN_BYTES);
    expect(demand).toEqual({
      unifiedBytes: bytes,
      hostBytes: bytes,
      acceleratorBytes: bytes,
      confidence: "observed",
    });
  });

  test("an observed peak overrides a checked-in measurement, up or down", () => {
    const plan = sttPlan(measuredStt);
    for (const peakBytes of [64 * mib, 30 * gib]) {
      const demand = withAdmissionDemand(plan, measuredStt, {
        observed: observed(measuredStt, [{ peakBytes }]),
      }).memoryDemand;
      expect(demand.confidence).toBe("observed");
      expect(demand.unifiedBytes).toBe(
        Math.ceil(peakBytes + observedPeakMargin(peakBytes)),
      );
    }
  });

  test("observed peaks at another shape or from VmHWM are ignored", () => {
    const plan = llmPlan();
    const source = observed(llm, [
      { launchShape: planLaunchShape(llmPlan(16384, 2)), peakBytes: 9 * gib },
      { launchShape: planLaunchShape(plan), method: "vmhwm" },
      { launchShape: planLaunchShape(plan), peakBytes: 0 },
    ]);
    expect(withAdmissionDemand(plan, llm, { observed: source })).toBe(plan);
    expect(source.calls).toEqual([
      `${llm.modelId}@${primaryArtifact(llm).sha256}`,
    ]);
  });

  test("a launch of another file ignores observed and feed evidence", () => {
    const plan = sttPlan(stt, "custom-whisper.bin");
    publish(stt, [feedPeak("default")]);
    const source = observed(stt, [{ peakBytes: 30 * gib }]);
    expect(withAdmissionDemand(plan, stt, { observed: source })).toBe(plan);
    expect(source.calls).toEqual([]);
  });

  test("an unknown model keeps the plan", () => {
    const plan = llmPlan();
    expect(withAdmissionDemand(plan, undefined)).toBe(plan);
  });
});

describe("admission demand sizing", () => {
  test("the observed margin is max(512 MiB, 10%)", () => {
    expect(observedPeakMargin(1 * gib)).toBe(MINIMUM_MEMORY_MARGIN_BYTES);
    expect(observedPeakMargin(20 * gib)).toBe(2 * gib);
  });

  test("a larger local file adds its extra bytes to the feed figure", () => {
    publish(stt, [feedPeak("default")]);
    const base = catalogModelDemand(stt);
    const catalogBytes = totalArtifactBytes(stt);
    const plain = admissionDemand({
      spec: stt,
      base,
      launchShape: "default",
      loadsPrimaryArtifact: true,
    });
    const larger = admissionDemand({
      spec: stt,
      base,
      launchShape: "default",
      loadsPrimaryArtifact: true,
      artifactBytes: catalogBytes + gib,
    });
    const smaller = admissionDemand({
      spec: stt,
      base,
      launchShape: "default",
      loadsPrimaryArtifact: true,
      artifactBytes: 1,
    });
    expect(larger.unifiedBytes).toBe(plain.unifiedBytes + gib);
    expect(larger.hostBytes).toBe(plain.hostBytes + gib);
    expect(smaller).toEqual(plain);
  });

  test("the feed figure never drops below the bytes on disk", () => {
    publish(stt, [
      feedPeak("default", {
        peakBytes: 1,
        backendAcceleratorBytes: 1,
        hostBufferBytes: 0,
        peakRssBytes: 1,
      }),
    ]);
    const demand = withAdmissionDemand(sttPlan(stt), stt).memoryDemand;
    expect(demand.confidence).toBe("feed");
    expect(demand.hostBytes).toBeGreaterThanOrEqual(totalArtifactBytes(stt));
  });
});

describe("TTS admission demand", () => {
  test("an observed peak replaces the checked-in TTS figure", () => {
    const base = catalogModelDemand(measuredTts);
    expect(base.confidence).toBe("measured");
    const demand = ttsAdmissionDemand(measuredTts, base, {
      observed: observed(measuredTts, [{ peakBytes: 3 * gib }]),
    });
    expect(demand.confidence).toBe("observed");
    expect(demand.unifiedBytes).toBe(3 * gib + MINIMUM_MEMORY_MARGIN_BYTES);
  });

  test("a model-file override keeps the catalog figure", () => {
    const base = catalogModelDemand(measuredTts);
    expect(
      ttsAdmissionDemand(measuredTts, base, {
        observed: observed(measuredTts, [{ peakBytes: 3 * gib }]),
        loadsPrimaryArtifact: false,
      }),
    ).toBe(base);
  });
});
