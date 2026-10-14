import { z } from "zod";

export const hostSchema = z
  .string()
  .min(1)
  .max(253)
  .refine(
    (value) => value === value.trim() && !/\s/.test(value),
    "must not contain whitespace",
  );

export const portSchema = z.number().int().min(1).max(65_535);

/**
 * How the configured `ctxSize` ceiling becomes the llama-server launch context.
 * `auto` clamps it to the memory-safe recommendation; `explicit` launches at the
 * configured value as-is (still capped by the model's advertised window).
 */
export const llmContextModes = ["auto", "explicit"] as const;
export type LlmContextMode = (typeof llmContextModes)[number];
export const llmContextModeSchema = z.enum(llmContextModes);
