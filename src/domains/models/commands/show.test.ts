import { afterAll, expect, test } from "bun:test";
import { byId } from "../../../catalog";
import { defaultConfig } from "../../../manager";
import { gibibyte } from "../../runtime/memory-safety";
import { resolveModelCapabilities } from "../capability-detection";
import { projectModelMetadata } from "../model-metadata";
import { feedAnnotationLines, modelDetailLines } from "./show";
import {
  keepFormerCatalogEntries,
  restoreBuiltInCatalog,
} from "../../../test/former-catalog";

// Some fixtures are former built-in entries an upgraded install kept.
keepFormerCatalogEntries();
afterAll(restoreBuiltInCatalog);

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";

test("models show prints provenance, capability sources, and memory per launch shape", () => {
  const model = byId(modelId)!;
  const metadata = projectModelMetadata(model, {
    catalog: [model],
    config: defaultConfig("/tmp/localbase-show-test"),
    installations: new Map([[modelId, true]]),
    capabilities: new Map([
      [
        modelId,
        resolveModelCapabilities(model, {
          detected: { capabilities: { "code-editing": false } },
        }),
      ],
    ]),
    memory: new Map([
      [
        modelId,
        {
          observed: [
            {
              modelId,
              artifactSha256: model.artifacts[0]!.sha256!,
              launchShape: "ctx=8192;slots=1;kv=q8_0/q8_0",
              peakBytes: 3 * gibibyte,
              method: "footprint" as const,
              observedAt: "2026-10-15T00:00:00.000Z",
            },
          ],
        },
      ],
    ]),
    systemMemoryGb: 64,
  });
  const text = modelDetailLines(metadata, {
    installed: true,
    enabled: true,
    default: false,
  }).join("\n");

  expect(text).toContain(`ID:             ${modelId}`);
  expect(text).toContain("Status:         Installed · Enabled");
  expect(text).toContain(`(revision ${model.repositoryRevision})`);
  expect(text).toContain("Code generation · Listed in the catalog");
  expect(text).toContain(
    "Note: Code editing: the catalog lists it, but the installed files don't support it. Treated as unavailable, going by the installed files.",
  );
  expect(text).toContain(
    "Seen on this machine: 3.0 GB with 8,192-token context, 1 request at a time, q8_0 cache, 2026-10-15",
  );
  expect(text).toContain("This machine:   Fits, 60 GB to spare of 64 GB");
});

test("feedAnnotationLines prints status, peaks, recipe, and marks", () => {
  const lines = feedAnnotationLines({
    repo: "Org/Model-GGUF",
    revision: "a".repeat(40),
    file: "model.gguf",
    modelId: "model",
    status: "verified",
    revisionMatch: true,
    unsupportedCapabilities: [
      {
        id: "tool-calling",
        label: "Tool calling",
        reason: "The chat template has no tool-call format.",
        applied: false,
      },
    ],
    memoryPeaks: [
      {
        variant: "Q4_K_M",
        launchShape: "ctx=8192;slots=1;kv=q8_0/q8_0",
        peakBytes: 2 * gibibyte,
        backendAcceleratorBytes: gibibyte,
        hostBufferBytes: gibibyte,
        peakRssBytes: gibibyte,
        measuredAt: "2026-10-13",
        hardware: "Test machine",
      },
    ],
    recipe: {
      modelId: "model",
      fileCount: 2,
      totalBytes: 3 * gibibyte,
      files: [],
    },
  });
  const text = lines.join("\n");
  expect(lines[0]).toBe("From the catalog feed");
  expect(text).toContain("Verified by LocalBase");
  expect(text).toContain(`Org/Model-GGUF · model.gguf at ${"a".repeat(40)}`);
  expect(text).toMatch(
    /Measured peak: .*\(Q4_K_M on Test machine, 2026-10-13\)/,
  );
  expect(text).toMatch(/Recipe: +2 files, .*pinned by sha256/);
  expect(text).toContain(
    "Verified unsupported: The chat template has no tool-call format. (not applied: your entry declares it)",
  );
  expect(text).not.toContain("Note:");
  const other = feedAnnotationLines({
    repo: "Org/Model-GGUF",
    revision: "b".repeat(40),
    file: null,
    modelId: null,
    status: null,
    revisionMatch: false,
    unsupportedCapabilities: [],
    memoryPeaks: [],
    recipe: null,
  }).join("\n");
  expect(other).toContain("Not qualified or verified");
  expect(other).toContain("Note:");
});
