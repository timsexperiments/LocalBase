import { join } from "node:path";
import { z } from "zod";
import {
  modelCapabilityInputSchema,
  unsupportedCapabilitySchema,
} from "./catalog/capabilities";
import { CATALOG_DATA } from "./catalog/models";
import { safeFilenameSchema } from "./utils/checksum";

export {
  capabilitySchema,
  hasCapability,
  type Capability,
  type ModelCapability,
  type UnsupportedCapability,
} from "./catalog/capabilities";

export const modelKindSchema = z.enum(["llm", "stt", "tts", "image", "video"]);
export type ModelKind = z.infer<typeof modelKindSchema>;

export const modelModalitySchema = z.enum(["text", "audio", "image", "video"]);
export type ModelModality = z.infer<typeof modelModalitySchema>;

export const speechVoiceSchema = z.enum([
  "default",
  "harbor",
  "willow",
  "af_heart",
  "af_bella",
]);
export type SpeechVoice = z.infer<typeof speechVoiceSchema>;

export const referenceSpeechVoiceSchema = z.enum(["harbor", "willow"]);
export type ReferenceSpeechVoice = z.infer<typeof referenceSpeechVoiceSchema>;

export const kokoroSpeechVoiceSchema = z.enum(["af_heart", "af_bella"]);
export type KokoroSpeechVoice = z.infer<typeof kokoroSpeechVoiceSchema>;

export const commercialStatusSchema = z.enum([
  "open",
  "conditional",
  "prohibited",
]);

const repositoryUrlSchema = z
  .string()
  .url()
  .refine((value) => {
    const url = URL.parse(value);
    if (url?.hostname !== "github.com") return true;
    return (
      /^https:\/\/github\.com\/[A-Za-z0-9-]+\/[A-Za-z0-9_.-]+\/?$/.test(
        value,
      ) &&
      ![".", ".."].includes(value.split("/")[4] ?? "") &&
      !url.pathname.replace(/\/$/, "").endsWith(".git")
    );
  }, "GitHub sources must be HTTPS owner/repository URLs");

const artifactSourceSchema = z.object({
  repositoryUrl: repositoryUrlSchema,
  revision: z.string().regex(/^[a-fA-F0-9]{40}$/),
});

export const modelArtifactSchema = z.object({
  sourcePath: z.string().min(1),
  filename: safeFilenameSchema,
  expectedSizeBytes: z.number().int().positive(),
  sha256: z.string().regex(/^[a-fA-F0-9]{64}$/),
  role: z.enum(["primary", "supplementary"]),
  source: artifactSourceSchema.optional(),
});

const ttsReferenceVoiceSchema = z
  .object({
    name: referenceSpeechVoiceSchema,
    artifactFilename: safeFilenameSchema,
    license: z.literal("CC0-1.0"),
    provenanceUrl: z.string().url(),
  })
  .strict();

const llamaTtsRuntimeProfileSchema = z
  .object({
    kind: z.literal("llama-tts"),
    projectorArtifactFilename: safeFilenameSchema,
    referenceVoices: z.array(ttsReferenceVoiceSchema),
  })
  .strict();

const kokoroTtsRuntimeProfileSchema = z
  .object({
    kind: z.literal("kokoro-onnx"),
    artifacts: z
      .object({
        model: safeFilenameSchema,
        config: safeFilenameSchema,
        tokenizer: safeFilenameSchema,
        tokenizerConfig: safeFilenameSchema,
      })
      .strict(),
    // Voice embeddings ship inside the qualified runtime package.
    voices: z
      .array(z.object({ name: kokoroSpeechVoiceSchema }).strict())
      .min(1),
    defaultVoice: kokoroSpeechVoiceSchema,
  })
  .strict();

const ttsRuntimeProfileSchema = z.discriminatedUnion("kind", [
  llamaTtsRuntimeProfileSchema,
  kokoroTtsRuntimeProfileSchema,
]);
export type TtsRuntimeProfile = z.infer<typeof ttsRuntimeProfileSchema>;

const embeddingLlmRuntimeProfileSchema = z
  .object({
    capability: z.literal("embedding-only"),
    pooling: z.literal("last"),
    dimensions: z
      .object({
        minimum: z.number().int().positive(),
        maximum: z.number().int().positive(),
      })
      .strict()
      .refine(
        ({ minimum, maximum }) => minimum <= maximum,
        "embedding dimensions must have an ordered range",
      ),
  })
  .strict();

export type EmbeddingLlmRuntimeProfile = z.infer<
  typeof embeddingLlmRuntimeProfileSchema
>;

/**
 * Decision models (System One style, for example Cloudflare Clef) answer typed
 * questions about a state in one forward pass. llama.cpp serves them from
 * `/v1/systemone` only: no chat, no KV cache, and the whole prompt must fit in
 * one micro-batch.
 */
const decisionLlmRuntimeProfileSchema = z
  .object({
    capability: z.literal("decision-only"),
  })
  .strict();

export type DecisionLlmRuntimeProfile = z.infer<
  typeof decisionLlmRuntimeProfileSchema
>;

const llmRuntimeProfileSchema = z.discriminatedUnion("capability", [
  embeddingLlmRuntimeProfileSchema,
  decisionLlmRuntimeProfileSchema,
]);
export type LlmRuntimeProfile = z.infer<typeof llmRuntimeProfileSchema>;

/** The embedding profile when the model is embedding-only, else null. */
export function embeddingRuntimeProfile(
  model: Readonly<{ llmRuntime?: LlmRuntimeProfile }> | null | undefined,
): EmbeddingLlmRuntimeProfile | null {
  return model?.llmRuntime?.capability === "embedding-only"
    ? model.llmRuntime
    : null;
}

/** True when the model is a decision-only model served from /v1/systemone. */
export function isDecisionModel(
  model: Readonly<{ llmRuntime?: LlmRuntimeProfile }> | null | undefined,
): boolean {
  return model?.llmRuntime?.capability === "decision-only";
}

const vaeDecoderSchema = z
  .object({
    kind: z.literal("vae"),
    artifactFilename: safeFilenameSchema,
  })
  .strict();

const eulerImageGenerationProfileSchema = z
  .object({
    sampler: z.literal("euler"),
    steps: z.number().int().positive(),
    cfgScale: z.number().positive(),
  })
  .strict();

const lcmImageGenerationProfileSchema = z
  .object({
    sampler: z.literal("lcm"),
    scheduler: z.literal("lcm"),
    steps: z.number().int().positive(),
    cfgScale: z.number().positive(),
  })
  .strict();

const imageRuntimeProfileSchema = z.discriminatedUnion("kind", [
  z
    .object({
      kind: z.literal("diffusion-qwen3"),
      artifacts: z
        .object({
          diffusionModel: safeFilenameSchema,
          vae: safeFilenameSchema,
          textEncoder: safeFilenameSchema,
        })
        .strict(),
      generation: eulerImageGenerationProfileSchema,
    })
    .strict(),
  z
    .object({
      kind: z.literal("diffusion-flux1"),
      artifacts: z
        .object({
          diffusionModel: safeFilenameSchema,
          vae: safeFilenameSchema,
          clipL: safeFilenameSchema,
          t5xxl: safeFilenameSchema,
        })
        .strict(),
      generation: eulerImageGenerationProfileSchema,
    })
    .strict(),
  z
    .object({
      kind: z.literal("checkpoint"),
      artifacts: z.object({ diffusionModel: safeFilenameSchema }).strict(),
      generation: lcmImageGenerationProfileSchema,
    })
    .strict(),
]);

export type ImageRuntimeProfile = z.infer<typeof imageRuntimeProfileSchema>;

/**
 * sd-server launch options for image models. `diffusionFlashAttention`
 * passes `--diffusion-fa`; enable it only for families validated on the
 * pinned sd-server (docs/memory-calibration.md). `prediction` passes
 * `--prediction` for checkpoints whose parameterization sd-server cannot
 * infer, such as SD 2.x 768-v and SDXL v-prediction finetunes. `lora`
 * names a supplementary LoRA artifact sd-server applies per request at
 * the strength the request asks for, `defaultStrength` when it asks for
 * none; `trigger` is the phrase the LoRA was trained to respond to.
 */
export const IMAGE_LORA_MAX_STRENGTH = 2;

const imageLoraSchema = z
  .object({
    artifactFilename: safeFilenameSchema,
    defaultStrength: z.number().min(0).max(IMAGE_LORA_MAX_STRENGTH),
    trigger: z.string().trim().min(1).max(200).optional(),
  })
  .strict();

const imageLaunchOptionsSchema = z
  .object({
    diffusionFlashAttention: z.boolean(),
    prediction: z.enum(["eps", "v"]).optional(),
    lora: imageLoraSchema.optional(),
  })
  .strict();

export type ImageLora = z.infer<typeof imageLoraSchema>;

export type ImageLaunchOptions = z.infer<typeof imageLaunchOptionsSchema>;

const videoDecoderSchema = z.discriminatedUnion("kind", [
  vaeDecoderSchema,
  z
    .object({ kind: z.literal("tae"), artifactFilename: safeFilenameSchema })
    .strict(),
]);

const t2vArtifactMappingSchema = z
  .object({
    diffusionModel: safeFilenameSchema,
    textEncoder: safeFilenameSchema,
    decoder: videoDecoderSchema,
  })
  .strict();

const s2vArtifactMappingSchema = t2vArtifactMappingSchema
  .extend({ decoder: vaeDecoderSchema, audioEncoder: safeFilenameSchema })
  .strict();

const videoWorkloadBoundsSchema = z
  .object({
    maxWidth: z.number().int().positive(),
    maxHeight: z.number().int().positive(),
    maxFrames: z.number().int().positive(),
    fps: z.number().int().positive(),
  })
  .strict();

const videoGenerationProfileSchema = z
  .object({
    sampler: z.literal("euler"),
    scheduler: z.enum(["discrete", "lcm"]),
    steps: z.number().int().positive(),
    cfgScale: z.number().positive(),
    flowShift: z.number().positive(),
    seed: z.number().int().nonnegative(),
  })
  .strict();

const videoLaunchOptionsSchema = z
  .object({
    cpuOffload: z.boolean(),
    diffusionFlashAttention: z.boolean(),
    vaeConvDirect: z.boolean(),
  })
  .strict();

const videoQualificationProfileSchema = videoWorkloadBoundsSchema
  .extend({
    jobDeadlineMs: z
      .number()
      .int()
      .positive()
      .max(30 * 60 * 1_000),
    generation: videoGenerationProfileSchema,
    launchOptions: videoLaunchOptionsSchema,
  })
  .strict();

export const modelQualificationStateSchema = z.enum([
  "qualified",
  "experimental",
]);

const videoRuntimeTargetSchema = z
  .object({
    platform: z.enum(["darwin", "linux"]),
    architecture: z.enum(["arm64", "x64"]),
    accelerator: z.enum(["apple-unified", "nvidia"]),
  })
  .strict();

const videoRuntimeProfileBaseSchema = z.object({
  qualification: videoQualificationProfileSchema,
  supportedTargets: z.array(videoRuntimeTargetSchema).min(1),
});

const videoRuntimeProfileSchema = z.discriminatedUnion("mode", [
  videoRuntimeProfileBaseSchema
    .extend({
      mode: z.literal("t2v"),
      artifacts: t2vArtifactMappingSchema,
    })
    .strict(),
  videoRuntimeProfileBaseSchema
    .extend({
      mode: z.literal("ti2v"),
      artifacts: t2vArtifactMappingSchema,
    })
    .strict(),
  videoRuntimeProfileBaseSchema
    .extend({
      mode: z.literal("s2v"),
      artifacts: s2vArtifactMappingSchema,
    })
    .strict(),
]);

export type VideoRuntimeProfile = z.infer<typeof videoRuntimeProfileSchema>;
export type VideoRuntimeTarget = z.infer<typeof videoRuntimeTargetSchema>;

/** Local or test artifacts may omit release metadata outside the managed catalog. */
export type ModelArtifact = Omit<
  z.infer<typeof modelArtifactSchema>,
  "expectedSizeBytes" | "sha256"
> &
  Partial<
    Pick<z.infer<typeof modelArtifactSchema>, "expectedSizeBytes" | "sha256">
  >;

/** Model spec fields without cross-field validation. */
export const modelSpecObjectSchema = z
  .object({
    modelId: z.string().min(1),
    qualificationState: modelQualificationStateSchema.default("qualified"),
    kind: modelKindSchema,
    provider: z.string().min(1),
    /** Human-facing model name; `family` alone is ambiguous for fine-tunes. */
    displayName: z.string().min(1).optional(),
    family: z.string().min(1),
    version: z.string().min(1),
    size: z.string().min(1),
    quant: z.string().min(1),
    codingScore: z.number().optional(),
    storageGb: z.number().positive(),
    source: repositoryUrlSchema,
    repositoryRevision: z.string().regex(/^[a-fA-F0-9]{40}$/),
    artifacts: z.array(modelArtifactSchema).min(1),
    llmRuntime: llmRuntimeProfileSchema.optional(),
    ttsRuntime: ttsRuntimeProfileSchema.optional(),
    imageRuntime: imageRuntimeProfileSchema.optional(),
    imageLaunchOptions: imageLaunchOptionsSchema.optional(),
    videoRuntime: videoRuntimeProfileSchema.optional(),
    videoCatalogOnly: z.literal(true).optional(),
    inputModalities: z.array(modelModalitySchema).min(1),
    outputModalities: z.array(modelModalitySchema).min(1),
    contextWindowTokens: z.number().int().positive().nullable().default(null),
    /** Declared capabilities, in catalog order. */
    capabilities: z.array(modelCapabilityInputSchema),
    /**
     * Capabilities checked and found missing. They stay unavailable even
     * when template detection or engine flags claim them; only a verified
     * runtime round-trip overrides one.
     */
    unsupportedCapabilities: z.array(unsupportedCapabilitySchema).optional(),
    /** Container the model's runtime writes; WAV speech or AVI video. */
    outputFormat: z.enum(["wav", "avi"]).optional(),
    /** Runtime support for this model is still experimental. */
    experimentalRuntime: z.literal(true).optional(),
    /** Catalog-only entry that ships diffusion weights without a full pipeline. */
    diffusionWeightsOnly: z.literal(true).optional(),
    commercialStatus: commercialStatusSchema,
    catch: z.string(),
    notes: z.string(),
  })
  .strict();

/** Cross-field rules shared by the catalog and CLI output schemas. */
export function refineModelSpec(
  model: z.output<typeof modelSpecObjectSchema>,
  ctx: z.RefinementCtx,
): void {
  const filenames = new Set<string>();
  let primaryCount = 0;

  for (const [index, entry] of (
    model.unsupportedCapabilities ?? []
  ).entries()) {
    if (model.capabilities.some(({ id }) => id === entry.id)) {
      ctx.addIssue({
        code: "custom",
        message: `"${entry.id}" cannot be both declared and verified unsupported`,
        path: ["unsupportedCapabilities", index, "id"],
      });
    }
  }

  for (const [index, artifact] of model.artifacts.entries()) {
    const repositoryUrl = artifact.source?.repositoryUrl ?? model.source;
    if (
      URL.parse(repositoryUrl)?.hostname === "github.com" &&
      artifact.sourcePath
        .split("/")
        .some((segment) => segment === "." || segment === "..")
    ) {
      ctx.addIssue({
        code: "custom",
        message: "GitHub artifact paths must remain within the pinned revision",
        path: ["artifacts", index, "sourcePath"],
      });
    }
    if (filenames.has(artifact.filename)) {
      ctx.addIssue({
        code: "custom",
        message: "artifact filenames must be unique",
        path: ["artifacts", index, "filename"],
      });
    }
    filenames.add(artifact.filename);
    if (artifact.role === "primary") primaryCount += 1;
  }

  if (model.kind === "tts" && !model.ttsRuntime) {
    ctx.addIssue({
      code: "custom",
      message: "TTS models must declare a runtime profile",
      path: ["ttsRuntime"],
    });
  } else if (model.kind !== "tts" && model.ttsRuntime) {
    ctx.addIssue({
      code: "custom",
      message: "only TTS models may declare a runtime profile",
      path: ["ttsRuntime"],
    });
  }
  const ttsRuntime = model.ttsRuntime;
  if (ttsRuntime?.kind === "llama-tts") {
    const projector = model.artifacts.find(
      ({ filename }) => filename === ttsRuntime.projectorArtifactFilename,
    );
    if (!projector || projector.role !== "supplementary") {
      ctx.addIssue({
        code: "custom",
        message: "TTS projector must name a supplementary artifact",
        path: ["ttsRuntime", "projectorArtifactFilename"],
      });
    }
  }
  const referenceNames = new Set<string>();
  for (const [index, voice] of (ttsRuntime?.kind === "llama-tts"
    ? ttsRuntime.referenceVoices
    : []
  ).entries()) {
    if (referenceNames.has(voice.name)) {
      ctx.addIssue({
        code: "custom",
        message: "reference voice names must be unique",
        path: ["ttsRuntime", "referenceVoices", index, "name"],
      });
    }
    referenceNames.add(voice.name);
    const artifact = model.artifacts.find(
      ({ filename }) => filename === voice.artifactFilename,
    );
    if (!artifact || artifact.role !== "supplementary") {
      ctx.addIssue({
        code: "custom",
        message: "reference voices must name a supplementary artifact",
        path: ["ttsRuntime", "referenceVoices", index, "artifactFilename"],
      });
    }
  }
  if (
    ttsRuntime?.kind === "llama-tts" &&
    (!referenceNames.has("harbor") || !referenceNames.has("willow"))
  ) {
    ctx.addIssue({
      code: "custom",
      message: "TTS runtime must declare Harbor and Willow reference voices",
      path: ["ttsRuntime", "referenceVoices"],
    });
  }
  if (ttsRuntime?.kind === "kokoro-onnx") {
    const mappedArtifacts = Object.entries(ttsRuntime.artifacts);
    const mappedNames = new Set(
      mappedArtifacts.map(([, filename]) => filename),
    );
    if (mappedNames.size !== mappedArtifacts.length) {
      ctx.addIssue({
        code: "custom",
        message: "Kokoro runtime artifacts must be distinct",
        path: ["ttsRuntime", "artifacts"],
      });
    }
    for (const [name, filename] of mappedArtifacts) {
      if (!model.artifacts.some((artifact) => artifact.filename === filename)) {
        ctx.addIssue({
          code: "custom",
          message: "Kokoro runtime artifacts must name declared artifacts",
          path: ["ttsRuntime", "artifacts", name],
        });
      }
    }
    const voices = new Set<KokoroSpeechVoice>();
    for (const [index, voice] of ttsRuntime.voices.entries()) {
      if (voices.has(voice.name)) {
        ctx.addIssue({
          code: "custom",
          message: "Kokoro voice names must be unique",
          path: ["ttsRuntime", "voices", index, "name"],
        });
      }
      voices.add(voice.name);
    }
    if (!voices.has(ttsRuntime.defaultVoice)) {
      ctx.addIssue({
        code: "custom",
        message: "Kokoro default voice must be declared",
        path: ["ttsRuntime", "defaultVoice"],
      });
    }
  }

  if (model.kind !== "llm" && model.llmRuntime) {
    ctx.addIssue({
      code: "custom",
      message: "only LLM models may declare an LLM runtime profile",
      path: ["llmRuntime"],
    });
  }

  if (primaryCount !== 1) {
    ctx.addIssue({
      code: "custom",
      message: "models must have exactly one primary artifact",
      path: ["artifacts"],
    });
  }

  if ((model.kind === "image") !== (model.imageLaunchOptions !== undefined)) {
    ctx.addIssue({
      code: "custom",
      message:
        "image models must declare image launch options, and only image models may",
      path: ["imageLaunchOptions"],
    });
  }

  const lora = model.imageLaunchOptions?.lora;
  if (lora) {
    const artifact = model.artifacts.find(
      ({ filename }) => filename === lora.artifactFilename,
    );
    const mapped =
      model.imageRuntime !== undefined &&
      Object.values(model.imageRuntime.artifacts).includes(
        lora.artifactFilename,
      );
    if (!artifact || artifact.role !== "supplementary" || mapped) {
      ctx.addIssue({
        code: "custom",
        message:
          "an image LoRA must name a supplementary artifact the image runtime does not map",
        path: ["imageLaunchOptions", "lora", "artifactFilename"],
      });
    }
  }

  if (model.imageRuntime) {
    if (model.kind !== "image") {
      ctx.addIssue({
        code: "custom",
        message: "only image models may declare an image runtime profile",
        path: ["imageRuntime"],
      });
    }
    const mapped = Object.entries(model.imageRuntime.artifacts);
    if (
      new Set(mapped.map(([, filename]) => filename)).size !== mapped.length
    ) {
      ctx.addIssue({
        code: "custom",
        message: "image runtime artifacts must be distinct",
        path: ["imageRuntime", "artifacts"],
      });
    }
    for (const [name, filename] of mapped) {
      const artifact = model.artifacts.find(
        (artifact) => artifact.filename === filename,
      );
      if (
        !artifact ||
        artifact.role !==
          (name === "diffusionModel" ? "primary" : "supplementary")
      ) {
        ctx.addIssue({
          code: "custom",
          message:
            "image runtime must map diffusion to the primary artifact and encoders to supplementary artifacts",
          path: ["imageRuntime", "artifacts", name],
        });
      }
    }
  }

  if (
    model.videoCatalogOnly &&
    (model.kind !== "video" || model.videoRuntime)
  ) {
    ctx.addIssue({
      code: "custom",
      message:
        "catalog-only video entries cannot declare a runtime profile or another model kind",
      path: ["videoCatalogOnly"],
    });
  }
  if (model.kind === "video") {
    if (!model.videoRuntime && !model.videoCatalogOnly) {
      ctx.addIssue({
        code: "custom",
        message: "video models must declare a measured runtime profile",
        path: ["videoRuntime"],
      });
    } else if (model.videoRuntime) {
      const artifactNames = new Set(
        model.artifacts.map(({ filename }) => filename),
      );
      const { decoder, ...artifacts } = model.videoRuntime.artifacts;
      for (const [name, filename] of Object.entries({
        ...artifacts,
        decoder: decoder.artifactFilename,
      })) {
        if (!artifactNames.has(filename)) {
          ctx.addIssue({
            code: "custom",
            message: "video runtime artifact must name a declared artifact",
            path: ["videoRuntime", "artifacts", name],
          });
        }
      }
    }
  } else if (model.videoRuntime) {
    ctx.addIssue({
      code: "custom",
      message: "only video models may declare a video runtime profile",
      path: ["videoRuntime"],
    });
  }

  const capabilityIds = new Set<string>();
  for (const [index, { id }] of model.capabilities.entries()) {
    if (capabilityIds.has(id)) {
      ctx.addIssue({
        code: "custom",
        message: "capabilities must be unique",
        path: ["capabilities", index],
      });
    }
    capabilityIds.add(id);
  }
  if (
    (model.outputFormat === "wav" && model.kind !== "tts") ||
    (model.outputFormat === "avi" &&
      (model.kind !== "video" || !model.videoRuntime))
  ) {
    ctx.addIssue({
      code: "custom",
      message:
        "wav output is for speech models and avi output is for runnable video models",
      path: ["outputFormat"],
    });
  }
  if (model.experimentalRuntime && !model.videoRuntime) {
    ctx.addIssue({
      code: "custom",
      message: "only runnable video models may mark their runtime experimental",
      path: ["experimentalRuntime"],
    });
  }
  if (model.diffusionWeightsOnly && !model.videoCatalogOnly) {
    ctx.addIssue({
      code: "custom",
      message: "diffusion-weights-only entries must be catalog-only",
      path: ["diffusionWeightsOnly"],
    });
  }
}

export const modelSpecSchema =
  modelSpecObjectSchema.superRefine(refineModelSpec);
export type ModelSpec = Omit<
  z.output<typeof modelSpecSchema>,
  "artifacts" | "qualificationState"
> & {
  qualificationState?: z.infer<typeof modelQualificationStateSchema>;
  artifacts: ModelArtifact[];
};

export type CatalogInstallationState = {
  complete: boolean;
  primaryPath: string;
};

export const catalogSchema = z.array(modelSpecSchema);

export function validateCatalog(catalog: unknown): ModelSpec[] {
  return catalogSchema.parse(catalog);
}

/** The catalog shipped in this binary, before any user or remote layer. */
export const BUILT_IN_CATALOG: readonly ModelSpec[] = Object.freeze(
  validateCatalog(CATALOG_DATA),
);

/**
 * The effective catalog: built-in entries with the remote and user layers
 * applied (see `src/catalog/sources.ts`). It is replaced in place when the
 * layers load, so every lookup sees the same entries. Treat it as read-only.
 */
export const CATALOG: ModelSpec[] = [...BUILT_IN_CATALOG];

/**
 * The pre-capability `features` list: capability ids followed by the tags
 * that are now typed fields. Public outputs that emitted `features` keep
 * emitting this list so existing clients see the same strings.
 */
export function legacyFeatures(model: ModelSpec): string[] {
  const tags: string[] = model.capabilities.map(({ id }) => id);
  if (model.outputFormat) tags.push(`${model.outputFormat}-output`);
  if (model.kind === "tts") tags.push("cold-per-request");
  if (model.ttsRuntime?.kind === "llama-tts")
    tags.push("catalog-reference-voices");
  if (model.ttsRuntime?.kind === "kokoro-onnx")
    tags.push("bundled-native-voices");
  if (model.experimentalRuntime) tags.push("experimental");
  if (model.videoCatalogOnly) tags.push("catalog-only");
  if (model.diffusionWeightsOnly) tags.push("diffusion-weights-only");
  return tags;
}

export type ModelSpecWithFeatures = ModelSpec & { features: string[] };

/** Adds the legacy `features` list for outputs that have always carried it. */
export function withLegacyFeatures(model: ModelSpec): ModelSpecWithFeatures {
  return { ...model, features: legacyFeatures(model) };
}

export function byId(modelId: string): ModelSpec | undefined {
  return CATALOG.find((model) => model.modelId === modelId);
}

export function primaryArtifact(model: ModelSpec): ModelArtifact {
  const artifact = model.artifacts.find(({ role }) => role === "primary");
  if (!artifact) {
    throw new Error(`Model "${model.modelId}" has no primary artifact`);
  }
  return artifact;
}

/** The vision/audio projector (mmproj) shipped beside an LLM, if any. */
export function llmProjectorArtifact(
  model: ModelSpec,
): ModelArtifact | undefined {
  if (model.kind !== "llm") return undefined;
  return model.artifacts.find(
    ({ role, filename }) =>
      role === "supplementary" && /mmproj/i.test(filename),
  );
}

/** Resolves the closed, catalog-owned reference files for a TTS model. */
export function ttsReferenceArtifacts(
  model: ModelSpec,
): Readonly<Record<ReferenceSpeechVoice, ModelArtifact>> {
  if (model.kind !== "tts" || model.ttsRuntime?.kind !== "llama-tts") {
    throw new Error(`Model "${model.modelId}" is not a llama-tts model.`);
  }
  const references = model.ttsRuntime.referenceVoices;
  const harbor = references?.find(({ name }) => name === "harbor");
  const willow = references?.find(({ name }) => name === "willow");
  if (!harbor || !willow) {
    throw new Error(
      `TTS model "${model.modelId}" is missing its catalog reference voices.`,
    );
  }
  const artifactByFilename = new Map(
    model.artifacts.map((artifact) => [artifact.filename, artifact]),
  );
  const harborArtifact = artifactByFilename.get(harbor.artifactFilename);
  const willowArtifact = artifactByFilename.get(willow.artifactFilename);
  if (!harborArtifact || !willowArtifact) {
    throw new Error(
      `TTS model "${model.modelId}" has an invalid reference voice artifact.`,
    );
  }
  return Object.freeze({ harbor: harborArtifact, willow: willowArtifact });
}

export function ttsVoiceAllowed(model: ModelSpec, voice: SpeechVoice): boolean {
  if (model.kind !== "tts" || !model.ttsRuntime) return false;
  if (model.ttsRuntime.kind === "llama-tts") {
    return (
      voice === "default" ||
      model.ttsRuntime.referenceVoices.some(({ name }) => name === voice)
    );
  }
  return model.ttsRuntime.voices.some(({ name }) => name === voice);
}

/**
 * Checks whether every file declared by a catalog model is ready to use.
 *
 * Expected sizes catch interrupted or stale downloads without hashing large
 * artifacts during routine startup. Full checksums remain installer work.
 */
export async function resolveCatalogInstallation(
  model: ModelSpec,
  kindDirectory: string,
): Promise<CatalogInstallationState> {
  const primary = primaryArtifact(model);
  const artifactStates = await Promise.all(
    model.artifacts.map(async (artifact) => {
      const file = Bun.file(join(kindDirectory, artifact.filename));
      if (!(await file.exists())) return false;
      if (artifact.expectedSizeBytes === undefined) return true;

      try {
        return (await file.stat()).size === artifact.expectedSizeBytes;
      } catch {
        return false;
      }
    }),
  );

  return {
    complete: artifactStates.every(Boolean),
    primaryPath: join(kindDirectory, primary.filename),
  };
}

export function artifactDownloadUrl(
  model: ModelSpec,
  artifact: ModelArtifact,
): string {
  const { repositoryUrl, revision } = artifact.source ?? {
    repositoryUrl: model.source,
    revision: model.repositoryRevision,
  };
  const base = repositoryUrl.replace(/\/$/, "");
  const sourcePath = artifact.sourcePath.replace(/^\/+/, "");
  const repository = new URL(base);
  if (repository.hostname === "github.com") {
    const encodedPath = sourcePath.split("/").map(encodeURIComponent).join("/");
    return `https://raw.githubusercontent.com${repository.pathname}/${revision}/${encodedPath}`;
  }
  return `${base}/resolve/${revision}/${sourcePath}`;
}

export function listModels(
  kind?: ModelKind,
  allowExperimental = false,
): ModelSpec[] {
  return CATALOG.filter(
    (model) =>
      (!kind || model.kind === kind) &&
      (allowExperimental || model.qualificationState !== "experimental"),
  );
}

export function calculateMaxSafeContextSize(
  model: ModelSpec,
  systemVramGb: number,
): number {
  const p = parseFloat(model.size);
  if (isNaN(p)) {
    return 8192;
  }

  const w = model.storageGb;

  const availableGb = (systemVramGb - w) * 0.8 - 4.0;
  if (availableGb <= 0) {
    return 4096;
  }

  const calculatedC = availableGb / (p * 8e-6);

  const standardBlocks = [
    4096, 8192, 16384, 24576, 32768, 49152, 65536, 98304, 131072,
  ];

  let recommended = 4096;
  for (const block of standardBlocks) {
    if (block <= calculatedC) {
      recommended = block;
    }
  }

  const isOlderModel =
    model.family.toLowerCase().includes("phi-3") ||
    model.modelId.includes("llama-3.2-") ||
    model.modelId.includes("llama-3-8b");
  const maxModelCtx = isOlderModel ? 8192 : 131072;

  return Math.min(
    recommended,
    maxModelCtx,
    model.contextWindowTokens ?? maxModelCtx,
  );
}
