import { expect, test } from "bun:test";
import { byId } from "../../../catalog";
import { defaultConfig } from "../../../manager";
import { gibibyte } from "../../runtime/memory-safety";
import { resolveModelCapabilities } from "../capability-detection";
import { projectModelMetadata } from "../model-metadata";
import { modelDetailLines } from "./show";

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
