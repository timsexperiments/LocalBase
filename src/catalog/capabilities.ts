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
  "decision",
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

/**
 * A capability the catalog has checked and found missing, even though the
 * chat template or the engine's template flags may claim it. Only a verified
 * runtime round-trip (a real request that exercised it) overrides this.
 */
export const unsupportedCapabilitySchema = z
  .object({
    id: capabilitySchema,
    supported: z.literal(false),
    source: z.literal("declared"),
    verified: z.literal(true),
    /** Why the capability is unsupported, in plain words. */
    reason: z.string().min(1),
  })
  .strict();
export type UnsupportedCapability = z.infer<typeof unsupportedCapabilitySchema>;

export function hasCapability(
  model: Readonly<{ capabilities: readonly ModelCapability[] }>,
  capability: Capability,
): boolean {
  return model.capabilities.some(({ id }) => id === capability);
}

/** Plain-language capability names shared by the UI and the CLI. */
export const capabilityLabels: Readonly<Record<Capability, string>> = {
  "text-generation": "Text generation",
  "tool-calling": "Tool calling",
  reasoning: "Reasoning",
  embeddings: "Embeddings",
  decision: "Decisions",
  "code-generation": "Code generation",
  "code-editing": "Code editing",
  "code-retrieval": "Code retrieval",
  "agentic-coding": "Agentic coding",
  "long-context": "Long context",
  multilingual: "Multilingual",
  translation: "Translation",
  "speech-to-text": "Speech to text",
  "text-to-speech": "Text to speech",
  "text-to-image": "Text to image",
  "image-to-image": "Image editing",
  "text-to-video": "Text to video",
  "image-to-video": "Image to video",
  "speech-to-video": "Speech to video",
  "mixture-of-experts": "Mixture of experts",
};

export function capabilityLabel(id: string): string {
  return capabilityLabels[id as Capability] ?? id;
}

/** Plain-language wording for where a capability claim came from. */
export const capabilitySourceLabels: Readonly<
  Record<CapabilitySource, string>
> = {
  declared: "Listed in the catalog",
  detected: "Found in the installed files",
  runtime: "Reported by the running engine",
};

/** Wire shape of one capability mismatch, shared by metadata and diagnostics. */
export const capabilityMismatchSchema = z
  .object({
    id: capabilitySchema,
    declared: z.boolean(),
    detected: z.boolean().optional(),
    runtime: z.boolean().optional(),
    verified: z.boolean().optional(),
    effective: z.boolean(),
    unsupportedReason: z.string().min(1).optional(),
  })
  .strict();

export type CapabilityMismatch = Readonly<{
  id: Capability;
  declared: boolean;
  detected?: boolean;
  runtime?: boolean;
  /** What a verified runtime round-trip observed, when one was recorded. */
  verified?: boolean;
  effective: boolean;
  /** Set when the catalog marks the capability verified unsupported. */
  unsupportedReason?: string;
}>;

/**
 * Explains, in plain words, how the evidence about one capability disagrees
 * and which way LocalBase decided.
 */
export function describeCapabilityMismatch(
  mismatch: Omit<CapabilityMismatch, "id"> & Readonly<{ id: string }>,
): string {
  const facts = [
    mismatch.declared ? "the catalog lists it" : "the catalog doesn't list it",
    ...(mismatch.detected === undefined
      ? []
      : [
          mismatch.detected
            ? "the installed files support it"
            : "the installed files don't support it",
        ]),
    ...(mismatch.runtime === undefined
      ? []
      : [
          mismatch.runtime
            ? "the running engine reports it"
            : "the running engine doesn't report it",
        ]),
  ];
  const joined =
    facts.length === 2
      ? `${facts[0]}, but ${facts[1]}`
      : `${facts.slice(0, -1).join(", ")}, and ${facts.at(-1)}`;
  const label = capabilityLabel(mismatch.id);
  if (mismatch.unsupportedReason !== undefined) {
    if (mismatch.effective) {
      return `${label}: the catalog marks it verified unsupported (${mismatch.unsupportedReason}), but a runtime round-trip succeeded. Treated as available, going by the round-trip.`;
    }
    const claims = [
      ...(mismatch.detected ? ["the installed files claim it"] : []),
      ...(mismatch.runtime ? ["the engine's template flags claim it"] : []),
    ];
    const despite =
      claims.length > 0
        ? ` This holds even though ${claims.join(" and ")}.`
        : "";
    return `${label}: verified unsupported. ${mismatch.unsupportedReason}${/[.!?]$/.test(mismatch.unsupportedReason) ? "" : "."}${despite} Treated as unavailable until a runtime round-trip proves otherwise.`;
  }
  const decider =
    mismatch.verified !== undefined
      ? "a runtime round-trip"
      : mismatch.runtime !== undefined
        ? "the running engine"
        : mismatch.detected !== undefined
          ? "the installed files"
          : "the catalog";
  return `${label}: ${joined}. Treated as ${mismatch.effective ? "available" : "unavailable"}, going by ${decider}.`;
}
