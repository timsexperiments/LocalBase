import { z } from "zod";

/** Maximum concurrent llama.cpp request slots supported by the current policy. */
export const MAX_PARALLEL_SLOTS = 4;

/** llama.cpp divides its total context budget across parallel slots. */
export const MIN_CONTEXT_PER_PARALLEL_SLOT = 2048;

/** Memory reserved for the OS, runtime, and non-model GPU work. */
export const RESERVED_RUNTIME_MEMORY_GB = 2;

/** Conservative estimate for the shared KV cache at the configured context size. */
export const CONTEXT_MEMORY_GB_PER_8K_TOKENS = 0.5;

/** Per-slot scheduler and activation headroom after shared memory is reserved. */
export const PARALLEL_SLOT_OVERHEAD_GB = 0.5;

export const parallelSlotsSchema = z.union([
  z.literal("auto"),
  z.literal(1),
  z.literal(2),
  z.literal(3),
  z.literal(4),
]);

export type ParallelSlots = z.infer<typeof parallelSlotsSchema>;

export type ParallelAllocationInput = {
  parallel: ParallelSlots;
  memoryGb: number;
  modelRequirementGb?: number;
  ctxSize: number;
  /** Model-specific KV cache size in GB for a slot count; replaces the flat estimate. */
  kvCacheGb?: (slots: number) => number;
};

export type ParallelAllocation = {
  slots: number;
  isAuto: boolean;
  contextPerSlot: number;
};

/**
 * Matches llama.cpp b10419 context normalization for the value exposed by
 * /v1/models. LocalBase passes --no-kv-unified, so slots receive separate
 * contexts padded to 256 tokens after the total context is padded.
 */
export function llamaContextPerSequence(
  ctxSize: number,
  slots: number,
  kvUnified = false,
): number {
  const paddedContext = Math.ceil(ctxSize / 256) * 256;
  if (kvUnified) return paddedContext;
  const perSequence = Math.floor(paddedContext / slots);
  return Math.ceil(perSequence / 256) * 256;
}

function invalidParallelSlots(value: unknown): never {
  throw new Error(
    `Invalid parallel slots ${JSON.stringify(value)}. Use "auto" or an integer from 1 to ${MAX_PARALLEL_SLOTS}.`,
  );
}

/** Parses CLI, TOML, and interactive parallel-slot input without broad coercion. */
export function parseParallelSlots(value: unknown): ParallelSlots {
  let normalized: unknown = value;

  if (typeof value === "string") {
    const trimmed = value.trim();
    if (trimmed.toLowerCase() === "auto") {
      normalized = "auto";
    } else if (/^[1-4]$/.test(trimmed)) {
      normalized = Number(trimmed);
    }
  }

  const result = parallelSlotsSchema.safeParse(normalized);
  if (result.success) return result.data;

  return invalidParallelSlots(value);
}

/** Parses an optional config value while preserving an omitted value for callers. */
export function parseOptionalParallelSlots(
  value: unknown,
): ParallelSlots | undefined {
  return value === undefined ? undefined : parseParallelSlots(value);
}

/**
 * Selects a safe slot count from explicit hardware and model inputs.
 * ctxSize is llama.cpp's total server budget, not a per-slot budget.
 */
export function allocateParallelSlots(
  input: ParallelAllocationInput,
): ParallelAllocation {
  const parallel = parseParallelSlots(input.parallel);
  if (
    !Number.isFinite(input.ctxSize) ||
    input.ctxSize < MIN_CONTEXT_PER_PARALLEL_SLOT
  ) {
    throw new Error(
      `Context size must be at least ${MIN_CONTEXT_PER_PARALLEL_SLOT} tokens to allocate parallel slots.`,
    );
  }
  if (parallel !== "auto") {
    const minimumContext = parallel * MIN_CONTEXT_PER_PARALLEL_SLOT;
    if (input.ctxSize < minimumContext) {
      throw new Error(
        `Parallel slots ${parallel} require at least ${minimumContext} total context tokens.`,
      );
    }
    return {
      slots: parallel,
      isAuto: false,
      contextPerSlot: Math.floor(input.ctxSize / parallel),
    };
  }

  const contextLimitedSlots = Math.max(
    1,
    Math.min(
      MAX_PARALLEL_SLOTS,
      Math.floor(input.ctxSize / MIN_CONTEXT_PER_PARALLEL_SLOT),
    ),
  );
  const hasUsableMemory =
    Number.isFinite(input.memoryGb) &&
    input.memoryGb > 0 &&
    Number.isFinite(input.modelRequirementGb) &&
    (input.modelRequirementGb ?? 0) >= 0;

  if (!hasUsableMemory) {
    return { slots: 1, isAuto: true, contextPerSlot: input.ctxSize };
  }

  const availableGb =
    input.memoryGb -
    (input.modelRequirementGb ?? 0) -
    RESERVED_RUNTIME_MEMORY_GB;
  let memoryLimitedSlots = 1;
  for (let slots = MAX_PARALLEL_SLOTS; slots > 1; slots -= 1) {
    const contextMemoryGb = input.kvCacheGb
      ? input.kvCacheGb(slots)
      : (Math.max(0, input.ctxSize) / 8192) * CONTEXT_MEMORY_GB_PER_8K_TOKENS;
    if (availableGb - contextMemoryGb >= slots * PARALLEL_SLOT_OVERHEAD_GB) {
      memoryLimitedSlots = slots;
      break;
    }
  }
  const slots = Math.min(contextLimitedSlots, memoryLimitedSlots);

  return {
    slots,
    isAuto: true,
    contextPerSlot: Math.floor(input.ctxSize / slots),
  };
}
