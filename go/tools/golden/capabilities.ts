// Golden capability detection and resolution from the TypeScript code.
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/capabilities.ts <models-root> \
//     > go/internal/runtime/testdata/capabilities.json
//
// <models-root> holds llm/, stt/, tts/, image/, video/ (read only). The
// detected opinions are recorded with their key order so the Go resolution
// test is hermetic; the Go detection test re-reads the same files.
import { join } from "node:path";
import { CATALOG } from "../../../src/catalog";
import {
  detectModelCapabilities,
  resolveModelCapabilities,
} from "../../../src/domains/models/capability-detection";

const modelsRoot = process.argv[2] ?? "/nonexistent";
const runtimes = [
  null,
  { capabilities: { "tool-calling": false, reasoning: true }, inputModalities: ["text", "image"] },
  { capabilities: { "tool-calling": true, decision: false }, inputModalities: ["text"] },
];
const cases = [];
for (const model of CATALOG) {
  const detected = await detectModelCapabilities(model, join(modelsRoot, model.kind));
  for (const runtime of runtimes) {
    const resolved = resolveModelCapabilities(model, { detected, runtime });
    cases.push({
      modelId: model.modelId,
      detected: detected && {
        capabilities: Object.entries(detected.capabilities),
        inputModalities: detected.inputModalities ?? null,
        evidence: detected.evidence,
      },
      runtime,
      capabilities: resolved.capabilities,
      inputModalities: resolved.inputModalities,
      mismatches: resolved.mismatches,
    });
  }
}
console.log(JSON.stringify({ cases }, null, 1));
