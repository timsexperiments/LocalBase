import { describe, expect, test } from "bun:test";
import { byId, CATALOG, type ModelSpec } from "../../catalog";
import { gibibyte } from "./memory-safety";
import {
  LLM_DEFAULT_STARTUP_TIMEOUT_MS,
  LLM_LARGE_STARTUP_TIMEOUT_MS,
  LLM_LARGE_STARTUP_WEIGHT_BYTES,
  llmStartupTimeoutMs,
  llmStartupTimeoutMsForWeights,
} from "./startup-timeout";

// Catalog LLMs that have had the long llama-server startup budget since it was
// introduced (hand-written minVramGb >= 16 before #278; unchanged through #280).
const LONG_STARTUP_LLMS = new Set([
  "qwen2.5-coder-32b-instruct-q4_k_m",
  "qwen3-coder-next-q4_k_m",
  "llama-3.3-70b-instruct-q4_k_m",
  "deepseek-coder-33b-instruct-q4_k_m",
  "gemma-3-27b-it-q4_k_m",
  "phi-3.5-moe-instruct-q4_k_m",
  "gpt-oss-20b-q4_k_m",
  "deepseek-r1-distill-qwen-32b-q4_k_m",
  "deepseek-r1-distill-llama-70b-q4_k_m",
  "qwen2.5-72b-instruct-q4_k_m",
  "codestral-22b-instruct-q4_k_m",
  "qwen3.8-27b-q4_k_m",
  "devstral-small-2-24b-instruct-q4_k_m",
  "qwen3.5-27b-q4_k_m",
  "mistral-small-3.2-24b-instruct-q4_k_m",
]);

describe("llmStartupTimeoutMsForWeights", () => {
  test("switches to the long budget at the weight threshold", () => {
    expect(LLM_LARGE_STARTUP_WEIGHT_BYTES).toBe(10 * gibibyte);
    expect(llmStartupTimeoutMsForWeights(0)).toBe(60_000);
    expect(
      llmStartupTimeoutMsForWeights(LLM_LARGE_STARTUP_WEIGHT_BYTES - 1),
    ).toBe(LLM_DEFAULT_STARTUP_TIMEOUT_MS);
    expect(llmStartupTimeoutMsForWeights(LLM_LARGE_STARTUP_WEIGHT_BYTES)).toBe(
      LLM_LARGE_STARTUP_TIMEOUT_MS,
    );
    expect(LLM_LARGE_STARTUP_TIMEOUT_MS).toBe(180_000);
  });
});

describe("llmStartupTimeoutMs", () => {
  test("keeps the default budget for models outside the catalog", () => {
    expect(llmStartupTimeoutMs(undefined)).toBe(LLM_DEFAULT_STARTUP_TIMEOUT_MS);
  });

  test("keys off weight bytes, not the published minVramGb tier", () => {
    const small = byId("qwen3.5-0.8b-q4_k_m")!;
    expect(llmStartupTimeoutMs({ ...small, minVramGb: 64 })).toBe(
      LLM_DEFAULT_STARTUP_TIMEOUT_MS,
    );
    const large = byId("gpt-oss-20b-q4_k_m")!;
    expect(llmStartupTimeoutMs({ ...large, minVramGb: 4 })).toBe(
      LLM_LARGE_STARTUP_TIMEOUT_MS,
    );
  });

  test("counts every shard and projector", () => {
    const base = byId("qwen3.5-0.8b-q4_k_m")!;
    const half = LLM_LARGE_STARTUP_WEIGHT_BYTES / 2;
    const sharded: ModelSpec = {
      ...base,
      artifacts: [
        { ...base.artifacts[0]!, filename: "a.gguf", expectedSizeBytes: half },
        { ...base.artifacts[0]!, filename: "b.gguf", expectedSizeBytes: half },
      ],
    };
    expect(llmStartupTimeoutMs(sharded)).toBe(LLM_LARGE_STARTUP_TIMEOUT_MS);
  });

  test("preserves each catalog LLM's existing startup budget", () => {
    const llms = CATALOG.filter((spec) => spec.kind === "llm");
    for (const id of LONG_STARTUP_LLMS) {
      expect(llms.some((spec) => spec.modelId === id)).toBe(true);
    }
    for (const spec of llms) {
      expect({ id: spec.modelId, ms: llmStartupTimeoutMs(spec) }).toEqual({
        id: spec.modelId,
        ms: LONG_STARTUP_LLMS.has(spec.modelId)
          ? LLM_LARGE_STARTUP_TIMEOUT_MS
          : LLM_DEFAULT_STARTUP_TIMEOUT_MS,
      });
    }
  });
});
