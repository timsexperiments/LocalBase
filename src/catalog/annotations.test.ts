import { afterEach, describe, expect, test } from "bun:test";
import { BUILT_IN_CATALOG, type ModelSpec } from "../catalog";
import { resolveModelCapabilities } from "../domains/models/capability-detection";
import type { MemoryMeasurement } from "../domains/runtime/memory-measurements";
import {
  annotationFor,
  annotationSummaryFor,
  appliedFeedMarks,
  buildFeedAnnotations,
  effectiveUnsupportedCapabilities,
  headlinePeak,
  huggingFaceRepoOf,
  memoryPeakOf,
  parseFeedAnnotations,
  setFeedAnnotations,
  summarizeAnnotation,
  type FeedAnnotation,
} from "./annotations";
import { applyCatalogLayers } from "./sources";

afterEach(() => {
  applyCatalogLayers([]);
  setFeedAnnotations([]);
});

const llm = BUILT_IN_CATALOG.find(
  (model) =>
    model.kind === "llm" &&
    model.artifacts.length === 1 &&
    model.source.startsWith("https://huggingface.co/") &&
    !(model.unsupportedCapabilities ?? []).some(
      ({ id }) => id === "tool-calling",
    ) &&
    !model.capabilities.some(({ id }) => id === "tool-calling"),
)!;
const repo = new URL(llm.source).pathname.slice(1);
const otherRevision = "0".repeat(40);

const mark = {
  id: "tool-calling",
  supported: false,
  source: "declared",
  verified: true,
  reason: "The chat template has no tool-call format.",
} as const;

function annotation(overrides: Partial<FeedAnnotation> = {}): FeedAnnotation {
  return {
    repo,
    revision: llm.repositoryRevision,
    file: llm.artifacts[0]!.sourcePath,
    modelId: llm.modelId,
    status: "verified",
    unsupportedCapabilities: [mark],
    ...overrides,
  };
}

const measurement: MemoryMeasurement = {
  kind: "llm",
  modelId: llm.modelId,
  variant: "Q4_K_M",
  shape: { ctxTokens: 8192, slots: 1, cacheTypeK: "q8_0", cacheTypeV: "q8_0" },
  backendAcceleratorBytes: 1_000,
  hostBufferBytes: 500,
  peakRssBytes: 1_200,
  systemDeltaBytes: 1_800,
  date: "2026-10-13",
  hardware: "Test machine",
  method: "test",
};

describe("parseFeedAnnotations", () => {
  test("drops a bad field or item, keeps the annotation", () => {
    const { annotations, skipped } = parseFeedAnnotations([
      {
        ...annotation(),
        status: "excellent",
        unsupportedCapabilities: [mark, { id: "not-a-capability" }],
        memoryPeaks: "nope",
        futureField: { anything: true },
      },
    ]);
    expect(skipped).toBe(0);
    expect(annotations).toHaveLength(1);
    const [parsed] = annotations;
    expect(parsed!.status).toBeUndefined();
    expect(parsed!.unsupportedCapabilities).toEqual([mark]);
    expect("memoryPeaks" in parsed!).toBe(false);
    expect("futureField" in parsed!).toBe(false);
  });

  test("skips annotations without a valid repo and revision", () => {
    const { annotations, skipped } = parseFeedAnnotations([
      { revision: llm.repositoryRevision },
      { repo: "not a repo", revision: llm.repositoryRevision },
      { repo, revision: "main" },
      null,
      annotation(),
    ]);
    expect(annotations).toHaveLength(1);
    expect(skipped).toBe(4);
  });
});

describe("matching", () => {
  test("huggingFaceRepoOf reads org/name from Hugging Face URLs only", () => {
    expect(huggingFaceRepoOf("https://huggingface.co/org/name/tree/main")).toBe(
      "org/name",
    );
    expect(huggingFaceRepoOf("https://example.com/org/name")).toBeNull();
    expect(huggingFaceRepoOf("https://huggingface.co/org")).toBeNull();
  });

  test("an exact repo and revision match applies its marks", () => {
    setFeedAnnotations([
      annotation({
        repo: repo.toUpperCase(),
        revision: llm.repositoryRevision.toUpperCase(),
      }),
    ]);
    expect(annotationFor(llm)?.exact).toBe(true);
    expect(appliedFeedMarks(llm).map(({ id }) => id)).toEqual(["tool-calling"]);
    expect(
      effectiveUnsupportedCapabilities(llm).some(
        ({ id }) => id === "tool-calling",
      ),
    ).toBe(true);
    const summary = annotationSummaryFor(llm)!;
    expect(summary.revisionMatch).toBe(true);
    expect(summary.unsupportedCapabilities[0]).toMatchObject({
      id: "tool-calling",
      applied: true,
      reason: mark.reason,
    });
  });

  test("another revision of the same model is shown, never applied", () => {
    setFeedAnnotations([annotation({ revision: otherRevision })]);
    expect(annotationFor(llm)).toMatchObject({ exact: false });
    expect(appliedFeedMarks(llm)).toEqual([]);
    const summary = annotationSummaryFor(llm)!;
    expect(summary.revisionMatch).toBe(false);
    expect(summary.unsupportedCapabilities[0]!.applied).toBe(false);
  });

  test("another revision for a different model id does not match", () => {
    setFeedAnnotations([
      annotation({ revision: otherRevision, modelId: "someone-else" }),
    ]);
    expect(annotationFor(llm)).toBeNull();
    expect(annotationSummaryFor(llm)).toBeNull();
  });

  test("a different file at the same revision is not an exact match", () => {
    setFeedAnnotations([annotation({ file: "other.gguf" })]);
    expect(annotationFor(llm)).toMatchObject({ exact: false });
    expect(appliedFeedMarks(llm)).toEqual([]);
  });

  test("an annotation without a file covers the whole revision", () => {
    const { file: _file, ...rest } = annotation();
    setFeedAnnotations([rest]);
    expect(annotationFor(llm)?.exact).toBe(true);
  });

  test("a mark the spec already carries is not doubled", () => {
    const marked: ModelSpec = { ...llm, unsupportedCapabilities: [mark] };
    setFeedAnnotations([annotation()]);
    expect(appliedFeedMarks(marked)).toEqual([]);
    expect(effectiveUnsupportedCapabilities(marked)).toEqual([mark]);
  });
});

describe("precedence", () => {
  test("a user entry that declares the capability wins over the feed", () => {
    applyCatalogLayers([
      {
        tier: "user",
        entries: [
          {
            path: "/scratch/catalog.d/patch.json",
            data: {
              modelId: llm.modelId,
              capabilities: [
                ...llm.capabilities,
                { id: "tool-calling", source: "declared" },
              ],
            },
          },
        ],
      },
    ]);
    setFeedAnnotations([annotation()]);
    const patched = {
      ...llm,
      capabilities: [
        ...llm.capabilities,
        { id: "tool-calling", source: "declared" },
      ],
    } as ModelSpec;
    expect(appliedFeedMarks(patched)).toEqual([]);
    expect(
      annotationSummaryFor(patched)!.unsupportedCapabilities[0]!.applied,
    ).toBe(false);
    // A user patch that leaves the capability alone still gets the mark.
    expect(appliedFeedMarks(llm).map(({ id }) => id)).toEqual(["tool-calling"]);
  });

  test("a verified runtime round-trip overrides a feed mark", () => {
    setFeedAnnotations([annotation()]);
    const blocked = resolveModelCapabilities(llm, {
      detected: { capabilities: { "tool-calling": true } },
    } as Parameters<typeof resolveModelCapabilities>[1]);
    expect(blocked.capabilities.some(({ id }) => id === "tool-calling")).toBe(
      false,
    );
    const verified = resolveModelCapabilities(llm, {
      runtime: {
        capabilities: { "tool-calling": true },
        verified: { "tool-calling": true },
      },
    } as Parameters<typeof resolveModelCapabilities>[1]);
    expect(verified.capabilities).toContainEqual({
      id: "tool-calling",
      source: "runtime",
    });
  });
});

describe("summaries", () => {
  test("summarizeAnnotation labels marks and condenses the recipe", () => {
    const summary = summarizeAnnotation(
      annotation({
        memoryPeaks: [memoryPeakOf(measurement)],
        recipe: {
          modelId: llm.modelId,
          totalBytes: 30,
          files: [
            {
              repositoryUrl: llm.source,
              revision: llm.repositoryRevision,
              path: "a.gguf",
              sizeBytes: 10,
              sha256: "a".repeat(64),
              role: "primary",
            },
            {
              repositoryUrl: llm.source,
              revision: llm.repositoryRevision,
              path: "b.gguf",
              sizeBytes: 20,
              sha256: "b".repeat(64),
              role: "supplementary",
            },
          ],
        },
      }),
      { revisionMatch: true, applied: new Set(["tool-calling"]) },
    );
    expect(summary.unsupportedCapabilities[0]).toEqual({
      id: "tool-calling",
      label: expect.any(String),
      reason: mark.reason,
      applied: true,
    });
    expect(summary.recipe).toEqual({
      modelId: llm.modelId,
      fileCount: 2,
      totalBytes: 30,
      files: [
        { path: "a.gguf", sizeBytes: 10 },
        { path: "b.gguf", sizeBytes: 20 },
      ],
    });
    expect(summary.memoryPeaks).toHaveLength(1);
  });

  test("memoryPeakOf takes the largest figure and describes the shape", () => {
    const peak = memoryPeakOf(measurement);
    expect(peak.peakBytes).toBe(1_800);
    expect(peak.launchShape).toBe("ctx=8192;slots=1;kv=q8_0/q8_0");
    expect(peak.measuredAt).toBe("2026-10-13");
    const smaller = memoryPeakOf({
      ...measurement,
      systemDeltaBytes: undefined,
    });
    expect(smaller.peakBytes).toBe(1_500);
    expect("systemDeltaBytes" in smaller).toBe(false);
    expect(headlinePeak([smaller, peak])).toBe(peak);
    expect(headlinePeak([])).toBeUndefined();
  });
});

describe("buildFeedAnnotations", () => {
  test("publishes status, marks and peaks for the entry's exact revision", () => {
    const [built] = buildFeedAnnotations(
      [{ ...llm, unsupportedCapabilities: [mark] }],
      [measurement],
    );
    expect(built).toMatchObject({
      repo,
      revision: llm.repositoryRevision.toLowerCase(),
      file: llm.artifacts[0]!.sourcePath,
      modelId: llm.modelId,
      kind: "llm",
      status: llm.qualificationState === "qualified" ? "qualified" : "verified",
      unsupportedCapabilities: [mark],
      memoryPeaks: [memoryPeakOf(measurement)],
    });
    expect(built!.recipe).toBeUndefined();
  });

  test("multi-file entries pinned by sha256 get a recipe", () => {
    const multi = BUILT_IN_CATALOG.find(
      (model) =>
        model.artifacts.length > 1 &&
        model.artifacts.every(
          (artifact) => artifact.sha256 && artifact.expectedSizeBytes,
        ),
    )!;
    expect(multi).toBeDefined();
    const [built] = buildFeedAnnotations([multi], []);
    expect(built!.recipe!.modelId).toBe(multi.modelId);
    expect(built!.recipe!.files).toHaveLength(multi.artifacts.length);
    expect(built!.recipe!.totalBytes).toBe(
      multi.artifacts.reduce(
        (sum, artifact) => sum + artifact.expectedSizeBytes!,
        0,
      ),
    );
    for (const file of built!.recipe!.files)
      expect(file.sha256).toMatch(/^[a-f0-9]{64}$/);
  });

  test("an entry with an unpinned file gets no recipe", () => {
    const multi = BUILT_IN_CATALOG.find((model) => model.artifacts.length > 1)!;
    const unpinned: ModelSpec = {
      ...multi,
      artifacts: multi.artifacts.map((artifact, index) =>
        index === 0 ? { ...artifact, sha256: undefined } : artifact,
      ),
    };
    const [built] = buildFeedAnnotations([unpinned], []);
    expect(built!.recipe).toBeUndefined();
  });

  test("entries off Hugging Face are skipped", () => {
    const local: ModelSpec = {
      ...llm,
      source: "https://example.com/model",
      artifacts: llm.artifacts.map((artifact) => ({
        ...artifact,
        source: undefined,
      })),
    };
    expect(buildFeedAnnotations([local], [])).toEqual([]);
  });
});
