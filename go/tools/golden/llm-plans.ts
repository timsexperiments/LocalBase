// Golden launch plans from the TypeScript implementation. The Go plan and
// estimate code must reproduce these exactly.
//
//   bun run go/tools/golden/llm-plans.ts <models-dir> > go/internal/runtime/testdata/llm-plans.json
//
// <models-dir> is read only, for GGUF headers; geometry is recorded in the
// output so the Go test is hermetic.
import { join } from "node:path";
import { existsSync } from "node:fs";
import { byId, primaryArtifact } from "../../../src/catalog";
import { readLlmKvGeometry } from "../../../src/domains/runtime/gguf-metadata";
import { resolveConfiguredLlmLaunchPlan, describeLlmContextBudget } from "../../../src/domains/runtime/launch-plan";
import { buildLlamaServerArgs } from "../../../src/domains/runtime/launcher";
import { keepFormerCatalogEntries } from "../../../src/test/former-catalog";

// Installs keep former built-in entries (#368); plan them as well.
keepFormerCatalogEntries();

const modelsDir = process.argv[2] ?? "/nonexistent";
const models = [
  "qwen2.5-coder-1.5b-instruct-q4_k_m",
  "qwen3.5-9b-q4_k_m",
  "qwen3.5-27b-q4_k_m",
  "qwen3.8-27b-q4_k_m",
  "clef-flash-q4_k_m",
  "qwen3-embedding-0.6b-q8_0",
  "qwen3-embedding-4b-q4_k_m",
  "gpt-oss-20b-q4_k_m",
  "gemma-3-12b-it-q4_k_m",
];
const configs = [
  { ctxSize: 8192, ctxMode: "auto", parallel: "auto" },
  { ctxSize: 65536, ctxMode: "auto", parallel: 1 },
  { ctxSize: 65536, ctxMode: "explicit", parallel: 1 },
  { ctxSize: 131072, ctxMode: "auto", parallel: "auto" },
  { ctxSize: 32768, ctxMode: "explicit", parallel: 4 },
] as const;
const memories = [64, 16];

const cases = [];
for (const modelId of models) {
  const spec = byId(modelId);
  if (!spec) continue;
  const file = primaryArtifact(spec).filename;
  const path = join(modelsDir, file);
  const geometry = existsSync(path) ? await readLlmKvGeometry(path) : null;
  for (const config of configs) {
    for (const memoryGb of memories) {
      const artifactBytes = spec.artifacts.reduce((t, a) => t + (a.expectedSizeBytes ?? 0), 0);
      let plan;
      try {
        plan = resolveConfiguredLlmLaunchPlan({
          runtimeId: "golden", root: "/r", modelsDirectory: "/m", modelId, modelFile: file,
          host: "127.0.0.1", port: 18000, model: spec,
          configCtxSize: config.ctxSize, ctxMode: config.ctxMode, parallel: config.parallel,
          artifactBytes, memoryGb, kvGeometry: geometry,
          trainingContextLength: geometry?.contextLength ?? null, projectorPath: null,
        });
      } catch (error) {
        cases.push({ modelId, config, memoryGb, geometry, error: String(error) });
        continue;
      }
      const budget = describeLlmContextBudget({
        model: spec, configCtxSize: config.ctxSize, ctxMode: config.ctxMode, memoryGb,
        plan: { ctxSize: plan.ctxSize, parallel: plan.parallel, trainingContextLength: geometry?.contextLength ?? null },
      });
      cases.push({
        modelId, config, memoryGb, geometry,
        ctxSize: plan.ctxSize, slots: plan.parallel.slots, kvType: plan.kvCache.typeK,
        demand: plan.memoryDemand, args: buildLlamaServerArgs(plan).args, budget,
      });
    }
  }
}
console.log(JSON.stringify({ platform: `${process.platform}-${process.arch}`, cases }, null, 1));
