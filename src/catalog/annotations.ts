/**
 * Catalog feed annotations: what the project has verified about a Hugging
 * Face repository at a pinned revision, published in the v2 remote feed
 * (`./remote-feed.ts`) beside the catalog entries.
 *
 * An annotation is keyed by repository and revision (and, for repositories
 * with many files, the primary file). It can carry:
 *
 * - `status`: `qualified` (LocalBase ships and tests it) or `verified`
 *   (checked to load and run, not part of the qualified set);
 * - `unsupportedCapabilities`: capabilities verified missing, with reasons;
 * - `memoryPeaks`: measured peak memory at a named launch shape;
 * - `recipe`: the full pinned file set a multi-file model needs (image,
 *   video and speech models ship weights, encoders and VAEs separately).
 *
 * Precedence, highest first: a verified runtime observation, then the
 * user's own catalog entry, then the feed. A feed mark never removes a
 * capability a user (or modified) entry declares, and only a verified
 * runtime round-trip turns a marked capability back on. Measured peaks feed
 * memory planning only from an exact match at the planned launch shape, and
 * rank below this machine's own observations and the checked-in
 * measurements (see `plannableFeedPeaks` and models/memory-fit.ts).
 *
 * Readers are tolerant: an annotation that fails validation is skipped and
 * counted, and an unknown or invalid field inside an otherwise good
 * annotation is dropped, so newer feeds keep working with older binaries.
 */
import { z } from "zod";
import type { ModelSpec } from "../catalog";
import {
  capabilityLabels,
  capabilitySchema,
  unsupportedCapabilitySchema,
  type Capability,
  type UnsupportedCapability,
} from "./capabilities";
import { catalogProvenance } from "./sources";
import type { MemoryMeasurement } from "../domains/runtime/memory-measurements";

export const MAX_FEED_ANNOTATIONS = 5_000;

const HF_HOST = "huggingface.co";
const repoSchema = z
  .string()
  .regex(/^[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._-]*$/);
const revisionSchema = z.string().regex(/^[a-fA-F0-9]{40}$/);
const sha256Schema = z.string().regex(/^[a-fA-F0-9]{64}$/);
const bytesSchema = z.number().int().nonnegative();

export const annotationStatusSchema = z.enum(["qualified", "verified"]);
export type AnnotationStatus = z.infer<typeof annotationStatusSchema>;

export const memoryPeakSchema = z
  .object({
    /** Quantization or file variant that was loaded. */
    variant: z.string().min(1).max(100),
    /** The launch shape it was measured at, e.g. `ctx=8192;slots=1;kv=q8_0/q8_0`. */
    launchShape: z.string().min(1).max(200),
    /** The largest of accelerator + host buffers, peak RSS and system delta. */
    peakBytes: bytesSchema,
    backendAcceleratorBytes: bytesSchema,
    hostBufferBytes: bytesSchema,
    peakRssBytes: bytesSchema,
    systemDeltaBytes: bytesSchema.optional(),
    measuredAt: z.string().regex(/^\d{4}-\d{2}-\d{2}$/),
    hardware: z.string().min(1).max(200),
  })
  .strict();
export type MemoryPeak = z.infer<typeof memoryPeakSchema>;

export const recipeFileSchema = z
  .object({
    repositoryUrl: z.string().url(),
    revision: revisionSchema,
    path: z.string().min(1).max(500),
    sizeBytes: z.number().int().positive(),
    sha256: sha256Schema,
    role: z.enum(["primary", "supplementary"]),
  })
  .strict();

export const recipeSchema = z
  .object({
    /** The catalog entry that installs this file set. */
    modelId: z.string().min(1).max(200),
    files: z.array(recipeFileSchema).min(1).max(64),
    totalBytes: z.number().int().positive(),
  })
  .strict();
export type ModelRecipe = z.infer<typeof recipeSchema>;

/** One published annotation, as the generator writes it. */
export const feedAnnotationSchema = z
  .object({
    repo: repoSchema,
    revision: revisionSchema,
    file: z.string().min(1).max(500).optional(),
    modelId: z.string().min(1).max(200).optional(),
    kind: z.enum(["llm", "stt", "tts", "image", "video"]).optional(),
    status: annotationStatusSchema.optional(),
    unsupportedCapabilities: z.array(unsupportedCapabilitySchema).optional(),
    memoryPeaks: z.array(memoryPeakSchema).optional(),
    recipe: recipeSchema.optional(),
  })
  .strict();
export type FeedAnnotation = z.infer<typeof feedAnnotationSchema>;

function validItems<T>(schema: z.ZodType<T>) {
  return z
    .array(z.unknown())
    .max(64)
    .optional()
    .catch(undefined)
    .transform((items) =>
      items?.flatMap((item) => {
        const parsed = schema.safeParse(item);
        return parsed.success ? [parsed.data] : [];
      }),
    );
}

/**
 * The reader's view: keys are required, every other field degrades to
 * undefined (or drops the bad item) instead of rejecting the annotation.
 */
const tolerantAnnotationSchema = z.object({
  repo: repoSchema,
  revision: revisionSchema,
  file: z.string().min(1).max(500).optional().catch(undefined),
  modelId: z.string().min(1).max(200).optional().catch(undefined),
  kind: z
    .enum(["llm", "stt", "tts", "image", "video"])
    .optional()
    .catch(undefined),
  status: annotationStatusSchema.optional().catch(undefined),
  unsupportedCapabilities: validItems(unsupportedCapabilitySchema),
  memoryPeaks: validItems(memoryPeakSchema),
  recipe: recipeSchema.optional().catch(undefined),
});

function compact(annotation: z.infer<typeof tolerantAnnotationSchema>) {
  return Object.fromEntries(
    Object.entries(annotation).filter(
      ([, value]) =>
        value !== undefined && !(Array.isArray(value) && value.length === 0),
    ),
  ) as FeedAnnotation;
}

/** Reads a feed's annotations, skipping the ones that do not validate. */
export function parseFeedAnnotations(
  items: readonly unknown[],
): Readonly<{ annotations: FeedAnnotation[]; skipped: number }> {
  const annotations: FeedAnnotation[] = [];
  let skipped = 0;
  for (const item of items.slice(0, MAX_FEED_ANNOTATIONS)) {
    const parsed = tolerantAnnotationSchema.safeParse(item);
    if (parsed.success) annotations.push(compact(parsed.data));
    else skipped += 1;
  }
  return {
    annotations,
    skipped: skipped + Math.max(0, items.length - MAX_FEED_ANNOTATIONS),
  };
}

// Repository keys -----------------------------------------------------------

/** `org/name` for a Hugging Face model URL; null for any other host. */
export function huggingFaceRepoOf(url: string): string | null {
  const parsed = URL.parse(url);
  if (!parsed || parsed.hostname !== HF_HOST) return null;
  const [org, name] = parsed.pathname.split("/").filter(Boolean);
  const repo = org && name ? `${org}/${name}` : "";
  return repoSchema.safeParse(repo).success ? repo : null;
}

function repoKey(repo: string): string {
  return repo.toLowerCase();
}

type PrimaryKey = Readonly<{ repo: string; revision: string; file: string }>;

function primaryKey(model: ModelSpec): PrimaryKey | null {
  const artifact = model.artifacts.find(({ role }) => role === "primary");
  if (!artifact) return null;
  const repo = huggingFaceRepoOf(
    artifact.source?.repositoryUrl ?? model.source,
  );
  if (!repo) return null;
  return {
    repo,
    revision: (
      artifact.source?.revision ?? model.repositoryRevision
    ).toLowerCase(),
    file: artifact.sourcePath,
  };
}

// State ---------------------------------------------------------------------

let current: readonly FeedAnnotation[] = [];
let byRepo = new Map<string, FeedAnnotation[]>();
let generation = 0;

/** Replaces the annotations in force (from the cached feed). */
export function setFeedAnnotations(
  annotations: readonly FeedAnnotation[],
): void {
  current = annotations;
  generation += 1;
  byRepo = new Map();
  for (const annotation of annotations) {
    const key = repoKey(annotation.repo);
    byRepo.set(key, [...(byRepo.get(key) ?? []), annotation]);
  }
}

export function feedAnnotations(): readonly FeedAnnotation[] {
  return current;
}

/** Bumps whenever the annotations in force change; lets readers drop caches. */
export function feedAnnotationsGeneration(): number {
  return generation;
}

/** Every annotation for one repository, any revision. */
export function annotationsForRepo(repo: string): readonly FeedAnnotation[] {
  return byRepo.get(repoKey(repo)) ?? [];
}

export type AnnotationMatch = Readonly<{
  annotation: FeedAnnotation;
  /**
   * The annotation describes exactly this entry's primary file at its
   * pinned revision. Only exact matches change capabilities.
   */
  exact: boolean;
}>;

/**
 * The annotation for a catalog entry: an exact repo + revision (+ file)
 * match first, else one published for the same model id and repository
 * at another revision (shown, never applied).
 */
export function annotationFor(model: ModelSpec): AnnotationMatch | null {
  const key = primaryKey(model);
  if (!key) return null;
  const candidates = annotationsForRepo(key.repo);
  const exact = candidates.find(
    (annotation) =>
      annotation.revision.toLowerCase() === key.revision &&
      (annotation.file === undefined || annotation.file === key.file),
  );
  if (exact) return { annotation: exact, exact: true };
  const sameId = candidates.find(
    (annotation) => annotation.modelId === model.modelId,
  );
  return sameId ? { annotation: sameId, exact: false } : null;
}

/**
 * Measured peaks memory planning may use for `model`: only from an
 * annotation that describes exactly its primary file at the pinned revision
 * and, when the annotation carries a recipe, the same primary sha256. The
 * caller still matches each peak's launch shape to the launch it plans.
 */
export function plannableFeedPeaks(model: ModelSpec): readonly MemoryPeak[] {
  const match = annotationFor(model);
  if (!match?.exact) return [];
  const pinned = model.artifacts
    .find(({ role }) => role === "primary")
    ?.sha256?.toLowerCase();
  const published = match.annotation.recipe?.files.find(
    ({ role }) => role === "primary",
  )?.sha256;
  if (pinned && published && published.toLowerCase() !== pinned) return [];
  return match.annotation.memoryPeaks ?? [];
}

/** The entry is the user's: a user file added or patched it. */
function userControlled(model: ModelSpec): boolean {
  const provenance = catalogProvenance(model.modelId);
  return (
    provenance.tier === "user" ||
    provenance.origin === "user" ||
    provenance.trust === "user" ||
    provenance.trust === "modified"
  );
}

/**
 * Feed marks that apply to `model`: exact matches only, minus ids the spec
 * marks itself, minus capabilities a user-controlled entry declares.
 */
export function appliedFeedMarks(
  model: ModelSpec,
): readonly UnsupportedCapability[] {
  const match = annotationFor(model);
  if (!match?.exact) return [];
  const own = new Set(
    (model.unsupportedCapabilities ?? []).map(({ id }) => id),
  );
  const declared = new Set(model.capabilities.map(({ id }) => id));
  const userOwned = userControlled(model);
  return (match.annotation.unsupportedCapabilities ?? []).filter(
    ({ id }) => !own.has(id) && !(userOwned && declared.has(id)),
  );
}

/** The spec's own marks followed by the feed marks that apply. */
export function effectiveUnsupportedCapabilities(
  model: ModelSpec,
): readonly UnsupportedCapability[] {
  const feed = appliedFeedMarks(model);
  const own = model.unsupportedCapabilities ?? [];
  return feed.length === 0 ? own : [...own, ...feed];
}

/** What the API and UI show about a feed annotation. */
export const annotationSummarySchema = z
  .object({
    repo: z.string(),
    revision: z.string(),
    file: z.string().nullable(),
    modelId: z.string().nullable(),
    status: annotationStatusSchema.nullable(),
    /** Describes exactly this entry or file at its pinned revision. */
    revisionMatch: z.boolean(),
    unsupportedCapabilities: z.array(
      z
        .object({
          id: capabilitySchema,
          label: z.string(),
          reason: z.string(),
          /** Applied to this entry (false when the user's entry overrides it). */
          applied: z.boolean(),
        })
        .strict(),
    ),
    memoryPeaks: z.array(memoryPeakSchema),
    recipe: z
      .object({
        modelId: z.string(),
        fileCount: z.number().int().positive(),
        totalBytes: z.number().int().positive(),
        files: z.array(
          z.object({ path: z.string(), sizeBytes: z.number() }).strict(),
        ),
      })
      .strict()
      .nullable(),
  })
  .strict();
export type AnnotationSummary = z.infer<typeof annotationSummarySchema>;

export function summarizeAnnotation(
  annotation: FeedAnnotation,
  options: Readonly<{
    revisionMatch: boolean;
    applied?: ReadonlySet<Capability>;
  }>,
): AnnotationSummary {
  return {
    repo: annotation.repo,
    revision: annotation.revision,
    file: annotation.file ?? null,
    modelId: annotation.modelId ?? null,
    status: annotation.status ?? null,
    revisionMatch: options.revisionMatch,
    unsupportedCapabilities: (annotation.unsupportedCapabilities ?? []).map(
      ({ id, reason }) => ({
        id,
        label: capabilityLabels[id] ?? id,
        reason,
        applied: options.applied?.has(id) ?? false,
      }),
    ),
    memoryPeaks: annotation.memoryPeaks ?? [],
    recipe: annotation.recipe
      ? {
          modelId: annotation.recipe.modelId,
          fileCount: annotation.recipe.files.length,
          totalBytes: annotation.recipe.totalBytes,
          files: annotation.recipe.files.map(({ path, sizeBytes }) => ({
            path,
            sizeBytes,
          })),
        }
      : null,
  };
}

/** The summary for an effective catalog entry, or null without one. */
export function annotationSummaryFor(
  model: ModelSpec,
): AnnotationSummary | null {
  const match = annotationFor(model);
  if (!match) return null;
  return summarizeAnnotation(match.annotation, {
    revisionMatch: match.exact,
    applied: new Set(appliedFeedMarks(model).map(({ id }) => id)),
  });
}

/** The largest single measured peak, the number the UI headlines. */
export function headlinePeak(
  peaks: readonly MemoryPeak[],
): MemoryPeak | undefined {
  return peaks.reduce<MemoryPeak | undefined>(
    (best, peak) => (!best || peak.peakBytes > best.peakBytes ? peak : best),
    undefined,
  );
}

// Generation ----------------------------------------------------------------

function launchShapeOf(record: MemoryMeasurement): string {
  switch (record.kind) {
    case "llm":
      return `ctx=${record.shape.ctxTokens};slots=${record.shape.slots};kv=${record.shape.cacheTypeK}/${record.shape.cacheTypeV}`;
    case "stt":
      return "default";
    case "tts":
      return `ctx=${record.shape.ctxTokens}`;
    case "image":
      return `${record.shape.width}x${record.shape.height};steps=${record.shape.steps};diffusion-fa=${record.shape.diffusionFlashAttention ? "on" : "off"}`;
    case "video":
      return `${record.shape.width}x${record.shape.height};frames=${record.shape.frames};cpu-offload=${record.shape.cpuOffload ? "on" : "off"};diffusion-fa=${record.shape.diffusionFlashAttention ? "on" : "off"}`;
  }
}

export function memoryPeakOf(record: MemoryMeasurement): MemoryPeak {
  return {
    variant: record.variant,
    launchShape: launchShapeOf(record),
    peakBytes: Math.max(
      record.backendAcceleratorBytes + record.hostBufferBytes,
      record.peakRssBytes,
      record.systemDeltaBytes ?? 0,
    ),
    backendAcceleratorBytes: record.backendAcceleratorBytes,
    hostBufferBytes: record.hostBufferBytes,
    peakRssBytes: record.peakRssBytes,
    ...(record.systemDeltaBytes !== undefined
      ? { systemDeltaBytes: record.systemDeltaBytes }
      : {}),
    measuredAt: record.date,
    hardware: record.hardware,
  };
}

/**
 * Annotations for the catalog this repository ships: one per entry whose
 * primary file lives on Hugging Face, with its status, marks, measured
 * peaks and (for multi-file entries) the install recipe.
 */
export function buildFeedAnnotations(
  models: readonly ModelSpec[],
  measurements: readonly MemoryMeasurement[],
  options: Readonly<{ status?: AnnotationStatus }> = {},
): FeedAnnotation[] {
  return models.flatMap((model): FeedAnnotation[] => {
    const key = primaryKey(model);
    if (!key) return [];
    const peaks = measurements
      .filter((record) => record.modelId === model.modelId)
      .map(memoryPeakOf);
    const status: AnnotationStatus =
      options.status ??
      (model.qualificationState === "qualified" && !model.videoCatalogOnly
        ? "qualified"
        : "verified");
    const pinned = model.artifacts.flatMap((artifact) =>
      artifact.sha256 && artifact.expectedSizeBytes
        ? [
            {
              ...artifact,
              sha256: artifact.sha256,
              expectedSizeBytes: artifact.expectedSizeBytes,
            },
          ]
        : [],
    );
    const recipe: ModelRecipe | undefined =
      model.artifacts.length > 1 && pinned.length === model.artifacts.length
        ? {
            modelId: model.modelId,
            files: pinned.map((artifact) => ({
              repositoryUrl: artifact.source?.repositoryUrl ?? model.source,
              revision: (
                artifact.source?.revision ?? model.repositoryRevision
              ).toLowerCase(),
              path: artifact.sourcePath,
              sizeBytes: artifact.expectedSizeBytes,
              sha256: artifact.sha256.toLowerCase(),
              role: artifact.role,
            })),
            totalBytes: pinned.reduce(
              (sum, artifact) => sum + artifact.expectedSizeBytes,
              0,
            ),
          }
        : undefined;
    const annotation: FeedAnnotation = {
      repo: key.repo,
      revision: key.revision,
      file: key.file,
      modelId: model.modelId,
      kind: model.kind,
      status,
      ...(model.unsupportedCapabilities?.length
        ? { unsupportedCapabilities: [...model.unsupportedCapabilities] }
        : {}),
      ...(peaks.length > 0 ? { memoryPeaks: peaks } : {}),
      ...(recipe ? { recipe } : {}),
    };
    return [feedAnnotationSchema.parse(annotation)];
  });
}
