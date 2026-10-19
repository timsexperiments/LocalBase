/**
 * Catalog source tiers.
 *
 * The effective catalog is built from three tiers, lowest first:
 *
 * 1. built-in: the JSON files under `src/catalog/models/`, shipped in the binary
 * 2. remote: the downloaded community feed (`./remote-feed.ts`)
 * 3. user: one JSON file per model under `<root>/catalog.d/`
 *
 * A higher tier wins. An entry whose `modelId` is new adds a model and must
 * be a complete model spec. An entry whose `modelId` already exists in a
 * lower tier is a shallow patch: each top-level key it sets replaces that
 * key, and keys it leaves out keep the lower tier's value. Nested objects
 * (such as `llmRuntime` or `artifacts`) are replaced whole, never merged.
 * The merged result must pass the same validation as a built-in entry.
 *
 * Trust follows the entry: built-in entries are `qualified`, remote entries
 * `community`, new user entries `user`. A patch that changes how the model
 * launches (its files, runtime profile or context window) makes a
 * qualified or community entry `modified`, and LocalBase stops using the
 * reference memory measurements that were taken with the original files.
 *
 * A file that fails to parse or validate is skipped and reported; it never
 * stops LocalBase from starting.
 */
import { z } from "zod";
import {
  BUILT_IN_CATALOG,
  CATALOG,
  modelSpecObjectSchema,
  modelSpecSchema,
  type ModelSpec,
} from "../catalog";
import type { FeedAnnotation } from "./annotations";
import { formerCatalogEntry } from "./former";

export const catalogTierSchema = z.enum(["built-in", "remote", "user"]);
export type CatalogTier = z.infer<typeof catalogTierSchema>;

export const catalogTrustSchema = z.enum([
  "qualified",
  "community",
  "user",
  "modified",
]);
export type CatalogTrust = z.infer<typeof catalogTrustSchema>;

/** Fields that change what launches; patching one makes an entry `modified`. */
export const LAUNCH_SHAPED_FIELDS: ReadonlySet<string> = new Set([
  "kind",
  "source",
  "repositoryRevision",
  "artifacts",
  "llmRuntime",
  "ttsRuntime",
  "imageRuntime",
  "imageLaunchOptions",
  "videoRuntime",
  "contextWindowTokens",
]);

export type CatalogProvenance = Readonly<{
  trust: CatalogTrust;
  /** The tier the entry first came from. */
  origin: CatalogTier;
  /** The highest tier that contributed to the entry. */
  tier: CatalogTier;
  /** The file of the highest non-built-in tier that contributed, if any. */
  path?: string;
  /** Top-level fields a higher tier changed, in spec order. */
  patchedFields: readonly string[];
}>;

export type CatalogDiagnostic = Readonly<{
  tier: CatalogTier;
  path: string;
  modelId?: string;
  message: string;
}>;

export type CatalogLayerEntry = Readonly<{ path: string; data: unknown }>;

export type CatalogLayer = Readonly<{
  tier: Exclude<CatalogTier, "built-in">;
  entries: readonly CatalogLayerEntry[];
  /** Problems found while reading the layer, before validation. */
  diagnostics?: readonly CatalogDiagnostic[];
  /** Feed annotations published with a remote layer (see `./annotations.ts`). */
  annotations?: readonly FeedAnnotation[];
}>;

export type CatalogComposition = Readonly<{
  models: readonly ModelSpec[];
  provenance: ReadonlyMap<string, CatalogProvenance>;
  diagnostics: readonly CatalogDiagnostic[];
}>;

const SPEC_KEYS: readonly string[] = Object.keys(modelSpecObjectSchema.shape);

const BUILT_IN_PROVENANCE: CatalogProvenance = Object.freeze({
  trust: "qualified",
  origin: "built-in",
  tier: "built-in",
  patchedFields: Object.freeze([]),
});

function describeIssues(error: z.ZodError): string {
  return error.issues
    .slice(0, 5)
    .map((issue) =>
      issue.path.length > 0
        ? `${issue.path.join(".")}: ${issue.message}`
        : issue.message,
    )
    .join("; ");
}

function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical);
  if (typeof value !== "object" || value === null) return value;
  return Object.fromEntries(
    Object.keys(value)
      .sort()
      .map((key) => [key, canonical((value as Record<string, unknown>)[key])]),
  );
}

/** Deep equality that ignores object key order. */
function sameValue(a: unknown, b: unknown): boolean {
  return JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
}

/**
 * Applies the remote and user layers to the built-in catalog. Pure: it reads
 * no files and keeps no state.
 */
export function composeCatalog(
  builtIn: readonly ModelSpec[],
  layers: readonly CatalogLayer[],
): CatalogComposition {
  const models = new Map<string, ModelSpec>(
    builtIn.map((model) => [model.modelId, model]),
  );
  const provenance = new Map<string, CatalogProvenance>(
    builtIn.map((model) => [model.modelId, BUILT_IN_PROVENANCE]),
  );
  const diagnostics: CatalogDiagnostic[] = [];
  const order = [...rankedTiers(layers)];
  for (const layer of order) {
    diagnostics.push(...(layer.diagnostics ?? []));
    const seen = new Map<string, string>();
    for (const entry of layer.entries) {
      const report = (message: string, modelId?: string) =>
        diagnostics.push({
          tier: layer.tier,
          path: entry.path,
          ...(modelId ? { modelId } : {}),
          message,
        });
      const data = entry.data;
      if (typeof data !== "object" || data === null || Array.isArray(data)) {
        report("The file must contain one JSON object (a model spec).");
        continue;
      }
      const record = data as Record<string, unknown>;
      const modelId = record.modelId;
      if (typeof modelId !== "string" || modelId.length === 0) {
        report('The entry needs a non-empty "modelId".');
        continue;
      }
      const unknownKeys = Object.keys(record).filter(
        (key) => !SPEC_KEYS.includes(key),
      );
      if (unknownKeys.length > 0) {
        report(`Unknown field(s): ${unknownKeys.join(", ")}.`, modelId);
        continue;
      }
      const earlier = seen.get(modelId);
      if (earlier) {
        report(`Duplicate entry for "${modelId}"; ${earlier} wins.`, modelId);
        continue;
      }
      const base = models.get(modelId);
      const parsed = modelSpecSchema.safeParse(
        base ? { ...base, ...record } : record,
      );
      if (!parsed.success) {
        report(
          `${base ? "The patched entry" : "The entry"} is invalid: ${describeIssues(parsed.error)}`,
          modelId,
        );
        continue;
      }
      seen.set(modelId, entry.path);
      const spec = parsed.data as ModelSpec;
      models.set(modelId, spec);
      if (!base) {
        provenance.set(modelId, {
          trust: layer.tier === "remote" ? "community" : "user",
          origin: layer.tier,
          tier: layer.tier,
          path: entry.path,
          patchedFields: [],
        });
        continue;
      }
      const previous = provenance.get(modelId) ?? BUILT_IN_PROVENANCE;
      const changed = SPEC_KEYS.filter(
        (key) =>
          key in record &&
          !sameValue(
            (base as Record<string, unknown>)[key],
            (spec as Record<string, unknown>)[key],
          ),
      );
      const patchedFields = SPEC_KEYS.filter(
        (key) => changed.includes(key) || previous.patchedFields.includes(key),
      );
      const launchShaped = changed.some((key) => LAUNCH_SHAPED_FIELDS.has(key));
      provenance.set(modelId, {
        trust:
          launchShaped &&
          (previous.trust === "qualified" || previous.trust === "community")
            ? "modified"
            : previous.trust,
        origin: previous.origin,
        tier: layer.tier,
        path: entry.path,
        patchedFields,
      });
    }
  }
  return { models: [...models.values()], provenance, diagnostics };
}

function* rankedTiers(layers: readonly CatalogLayer[]): Iterable<CatalogLayer> {
  for (const tier of ["remote", "user"] as const) {
    for (const layer of layers) if (layer.tier === tier) yield layer;
  }
}

let currentProvenance: ReadonlyMap<string, CatalogProvenance> = new Map();
let currentDiagnostics: readonly CatalogDiagnostic[] = [];

/**
 * Replaces the effective catalog with the built-in entries plus `layers`.
 * Every `byId`/`listModels` caller sees the result.
 */
export function applyCatalogLayers(
  layers: readonly CatalogLayer[],
): CatalogComposition {
  const composition = composeCatalog(BUILT_IN_CATALOG, layers);
  CATALOG.splice(0, CATALOG.length, ...composition.models);
  currentProvenance = composition.provenance;
  currentDiagnostics = composition.diagnostics;
  return composition;
}

/** Where an effective catalog entry came from and how far to trust it. */
export function catalogProvenance(modelId: string): CatalogProvenance {
  return currentProvenance.get(modelId) ?? BUILT_IN_PROVENANCE;
}

/** Files skipped while loading the catalog layers, with the reason. */
export function catalogDiagnostics(): readonly CatalogDiagnostic[] {
  return currentDiagnostics;
}

/**
 * Whether the checked-in reference memory measurements describe this entry.
 * They were taken with the qualified files and launch shape, so they do not
 * apply to user-added or modified entries. A former built-in entry kept as a
 * user entry (`./former`) still launches the measured files, so its records
 * apply while every launch-shaped field matches what was measured.
 */
export function usesReferenceMeasurements(
  model: Pick<ModelSpec, "modelId">,
): boolean {
  if (catalogProvenance(model.modelId).trust === "qualified") return true;
  return matchesFormerEntry(model);
}

/** The entry launches exactly what an earlier release shipped for its id. */
export function matchesFormerEntry(model: Pick<ModelSpec, "modelId">): boolean {
  const former = formerCatalogEntry(model.modelId);
  if (!former) return false;
  const current =
    CATALOG.find(({ modelId }) => modelId === model.modelId) ?? model;
  return [...LAUNCH_SHAPED_FIELDS].every((key) =>
    sameValue(
      (former as Record<string, unknown>)[key],
      (current as Record<string, unknown>)[key],
    ),
  );
}
