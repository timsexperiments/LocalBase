import { z } from "zod";

/**
 * What a catalog model can do. Kept free of runtime imports so the browser UI
 * can share the enum with the server.
 */
export const capabilitySchema = z.enum([
  "text-generation",
  "tool-calling",
  "reasoning",
  "embeddings",
  "code-generation",
  "code-editing",
  "code-retrieval",
  "agentic-coding",
  "long-context",
  "multilingual",
  "translation",
  "speech-to-text",
  "text-to-speech",
  "text-to-image",
  "image-to-image",
  "text-to-video",
  "image-to-video",
  "speech-to-video",
  "mixture-of-experts",
]);
export type Capability = z.infer<typeof capabilitySchema>;

/**
 * Where a capability claim came from, lowest to highest precedence:
 * `declared` by the catalog data file, `detected` from the installed model
 * files and the engine that runs them, and `runtime` as reported by the
 * running backend. Runtime evidence wins over every other source.
 */
export const capabilitySourceSchema = z.enum([
  "declared",
  "detected",
  "runtime",
]);
export type CapabilitySource = z.infer<typeof capabilitySourceSchema>;

export const modelCapabilitySchema = z
  .object({
    id: capabilitySchema,
    source: capabilitySourceSchema,
  })
  .strict();
export type ModelCapability = z.infer<typeof modelCapabilitySchema>;

/**
 * Data files list bare capability ids; those are catalog declarations. Only
 * the declared source may appear in data files: detection and runtime evidence
 * are computed, never written into the catalog.
 */
export const modelCapabilityInputSchema = z.union([
  capabilitySchema.transform((id): ModelCapability => ({
    id,
    source: "declared",
  })),
  z.object({ id: capabilitySchema, source: z.literal("declared") }).strict(),
]);

export function hasCapability(
  model: Readonly<{ capabilities: readonly ModelCapability[] }>,
  capability: Capability,
): boolean {
  return model.capabilities.some(({ id }) => id === capability);
}
