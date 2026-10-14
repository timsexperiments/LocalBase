import type { ModelSpec } from "../../catalog";
import { totalArtifactBytes } from "./memory-estimate";
import { gibibyte } from "./memory-safety";

/** llama-server startup budget for models with modest weights. */
export const LLM_DEFAULT_STARTUP_TIMEOUT_MS = 60_000;
/** llama-server startup budget once reading and mapping weights dominates. */
export const LLM_LARGE_STARTUP_TIMEOUT_MS = 180_000;
/**
 * Weight bytes at which llama-server gets the long startup budget. Load time
 * is driven by how many bytes must be read from disk and mapped onto the
 * accelerator, not by the memory tier a model is published under. 10 GiB
 * separates the largest model that has always started inside 60 s
 * (deepseek-coder-v2-lite, ~9.6 GiB) from the smallest that has always had
 * 180 s (gpt-oss-20b, ~10.9 GiB).
 */
export const LLM_LARGE_STARTUP_WEIGHT_BYTES = 10 * gibibyte;

/** Startup timeout for llama-server loading `weightBytes` of weights. */
export function llmStartupTimeoutMsForWeights(weightBytes: number): number {
  return weightBytes >= LLM_LARGE_STARTUP_WEIGHT_BYTES
    ? LLM_LARGE_STARTUP_TIMEOUT_MS
    : LLM_DEFAULT_STARTUP_TIMEOUT_MS;
}

/**
 * Startup timeout for a llama-server model, from the catalog's weight bytes
 * (every shard and projector). Models without a catalog entry keep the
 * default budget.
 */
export function llmStartupTimeoutMs(spec: ModelSpec | undefined): number {
  return llmStartupTimeoutMsForWeights(spec ? totalArtifactBytes(spec) : 0);
}
