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

/** sd-server image startup budget for models with modest weights. */
export const IMAGE_DEFAULT_STARTUP_TIMEOUT_MS = 30_000;
/** sd-server image startup budget once reading the weights dominates. */
export const IMAGE_LARGE_STARTUP_TIMEOUT_MS = 180_000;
/**
 * Weight bytes, summed over every component (diffusion model, VAE, text
 * encoders), at which sd-server image runtimes get the long startup budget.
 * Every catalog image model up to flux2-klein-4b-q8_0 (~6.6 GiB) keeps the
 * 30 s budget it has always had; flux2-klein-9b-q4_0 (~10.2 GiB) and
 * flux1-schnell-q4_0 (~16.1 GiB, mostly the fp16 T5-XXL encoder) do not fit
 * in it. The threshold matches the llama-server one so both runtimes treat
 * the same amount of weight the same way.
 */
export const IMAGE_LARGE_STARTUP_WEIGHT_BYTES = 10 * gibibyte;

/** Startup timeout for sd-server loading `weightBytes` of image weights. */
export function imageStartupTimeoutMsForWeights(weightBytes: number): number {
  return weightBytes >= IMAGE_LARGE_STARTUP_WEIGHT_BYTES
    ? IMAGE_LARGE_STARTUP_TIMEOUT_MS
    : IMAGE_DEFAULT_STARTUP_TIMEOUT_MS;
}

/**
 * Startup timeout for an sd-server image model, from the catalog's weight
 * bytes across all of its component files. Models without a catalog entry
 * keep the default budget.
 */
export function imageStartupTimeoutMs(spec: ModelSpec | undefined): number {
  return imageStartupTimeoutMsForWeights(spec ? totalArtifactBytes(spec) : 0);
}

/**
 * sd-server video startup budget. It is already the long budget, and every
 * catalog video model's weights (at most ~21.7 GiB, wan2.2-s2v-14b-fp8) are
 * well below the largest llama-server models (up to ~45 GiB) that start inside the
 * same 180 s, so weight bytes do not justify a different value.
 */
export const VIDEO_STARTUP_TIMEOUT_MS = 180_000;
