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
 * Where a capability claim came from. Only catalog declarations exist today;
 * detected and runtime-reported sources will join later, with runtime
 * evidence taking precedence over every other source.
 */
export const capabilitySourceSchema = z.enum(["declared"]);
export type CapabilitySource = z.infer<typeof capabilitySourceSchema>;

export const modelCapabilitySchema = z
  .object({
    id: capabilitySchema,
    source: capabilitySourceSchema,
  })
  .strict();
export type ModelCapability = z.infer<typeof modelCapabilitySchema>;

/** Data files list bare capability ids; those are catalog declarations. */
export const modelCapabilityInputSchema = z.union([
  capabilitySchema.transform((id): ModelCapability => ({
    id,
    source: "declared",
  })),
  modelCapabilitySchema,
]);

export function hasCapability(
  model: Readonly<{ capabilities: readonly ModelCapability[] }>,
  capability: Capability,
): boolean {
  return model.capabilities.some(({ id }) => id === capability);
}
