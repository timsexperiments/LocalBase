/**
 * Search-first model discovery: Hugging Face search results with the files
 * `models add` would pin and how each fits this machine's memory, and the
 * add step the admin UI runs. The CLI and the gateway share these helpers so
 * both write the same pinned user entries.
 */
import { z } from "zod";
import { byId, modelSpecSchema, type ModelSpec } from "../../catalog";
import {
  annotationsForRepo,
  feedAnnotations,
  summarizeAnnotation,
  type AnnotationSummary,
  type FeedAnnotation,
} from "../../catalog/annotations";
import type { UnsupportedCapability } from "../../catalog/capabilities";
import { catalogProvenance } from "../../catalog/sources";
import { writeUserCatalogEntry } from "../../catalog/user-catalog";
import {
  ADDABLE_KINDS,
  entryChoices,
  fetchModelInfo,
  generateEntry,
  hubRepoUrl,
  HubError,
  quantOf,
  searchHub,
  type AddableKind,
  type GeneratedEntry,
  type HubClientOptions,
  type HubErrorCode,
  type HubModelInfo,
  type HubSearchHit,
} from "./huggingface";
import { evaluateModelFit, type MemoryFitVerdict } from "./memory-fit";

/** The saved token, else HF_TOKEN. Sent only to huggingface.co. */
export function hubTokenFrom(
  config: Readonly<{ hfToken?: string }>,
): string | undefined {
  return config.hfToken || process.env.HF_TOKEN || undefined;
}

export type CatalogOwner = "built-in" | "remote" | "user" | null;

/** Which catalog tier currently provides `modelId`, if any. */
export function catalogOwner(modelId: string): CatalogOwner {
  if (!byId(modelId)) return null;
  return catalogProvenance(modelId).origin as Exclude<CatalogOwner, null>;
}

export type DiscoveryChoice = Readonly<{
  path: string;
  sizeBytes: number;
  /** All weight shards are downloaded together. */
  partCount: number;
  quant: string | null;
  modelId: string;
  /** The catalog tier that already has this id, if any. */
  existing: CatalogOwner;
  fit: Readonly<{
    verdict: MemoryFitVerdict;
    minVramGb: number;
    message: string;
  }> | null;
  /**
   * What the catalog feed publishes about this file. Its marks apply (and
   * Add writes them into the new entry) only at the same revision.
   */
  annotation: AnnotationSummary | null;
}>;

/** A catalog-feed annotation for a search result's repository. */
export type DiscoveryCatalogNote = AnnotationSummary &
  Readonly<{
    /** The catalog entry's name, when this binary's catalog has it. */
    displayName: string | null;
    /** The recipe installs an entry this catalog has. */
    installable: boolean;
  }>;

export type DiscoveryPreview = Readonly<{
  kind: AddableKind;
  choices: readonly DiscoveryChoice[];
  /** The file Add uses when none is chosen; null when the user must pick. */
  defaultPath: string | null;
}>;

export type DiscoveryHit = HubSearchHit &
  Readonly<{
    url: string;
    preview: DiscoveryPreview | null;
    /** Why there is no preview (unsupported kind, no usable files, offline). */
    reason: string | null;
    /** Set when the Hub refused in a way a token could fix. */
    reasonCode?: HubErrorCode;
    /** Catalog-feed annotations for this repository, recipes first. */
    catalog: readonly DiscoveryCatalogNote[];
  }>;

/** Repositories with more files are trimmed to keep responses small. */
const MAX_CHOICES = 40;
const PREVIEW_CONCURRENCY = 4;
/** At most this many feed annotations accompany one search result. */
const MAX_CATALOG_NOTES = 3;

function sameRevision(annotation: FeedAnnotation, sha: string | undefined) {
  return !!sha && annotation.revision.toLowerCase() === sha.toLowerCase();
}

/**
 * The feed annotation for one file of a repository: the one published for
 * the repository's current revision first, else one for another revision.
 */
export function annotationForFile(
  repo: string,
  sha: string | undefined,
  path: string,
): Readonly<{ annotation: FeedAnnotation; revisionMatch: boolean }> | null {
  const candidates = annotationsForRepo(repo).filter(
    (annotation) => annotation.file === undefined || annotation.file === path,
  );
  const exact = candidates.find((annotation) => sameRevision(annotation, sha));
  if (exact) return { annotation: exact, revisionMatch: true };
  const other = candidates[0];
  return other ? { annotation: other, revisionMatch: false } : null;
}

function choiceAnnotation(
  info: HubModelInfo,
  path: string,
): AnnotationSummary | null {
  const match = annotationForFile(info.id, info.sha, path);
  if (!match) return null;
  return summarizeAnnotation(match.annotation, {
    revisionMatch: match.revisionMatch,
    applied: new Set(
      match.revisionMatch
        ? (match.annotation.unsupportedCapabilities ?? []).map(({ id }) => id)
        : [],
    ),
  });
}

/** The feed annotations to show beside a search result. */
export function catalogNotesFor(
  repo: string,
  sha?: string,
): DiscoveryCatalogNote[] {
  const rank = (annotation: FeedAnnotation) =>
    (annotation.recipe ? 0 : 2) + (annotation.status ? 0 : 1);
  return [...annotationsForRepo(repo)]
    .sort((a, b) => rank(a) - rank(b))
    .slice(0, MAX_CATALOG_NOTES)
    .map((annotation) => {
      const model = annotation.modelId ? byId(annotation.modelId) : undefined;
      const recipeModel = annotation.recipe
        ? byId(annotation.recipe.modelId)
        : undefined;
      return {
        ...summarizeAnnotation(annotation, {
          revisionMatch: sameRevision(annotation, sha),
        }),
        displayName: model?.displayName ?? null,
        installable: !!recipeModel,
      };
    });
}

/**
 * Writes the feed's verified-unsupported marks into a generated entry, so
 * an added file starts with what the feed knows and the user can edit it.
 * Marked ids are removed from the declared capabilities, which a catalog
 * entry may not list in both places.
 */
export function withFeedMarks(
  spec: Record<string, unknown>,
  marks: readonly UnsupportedCapability[],
): Record<string, unknown> {
  if (marks.length === 0) return spec;
  const ids = new Set(marks.map(({ id }) => id));
  const capabilities = Array.isArray(spec.capabilities)
    ? spec.capabilities.filter((capability: unknown) => {
        const id =
          typeof capability === "string"
            ? capability
            : (capability as { id?: unknown } | null)?.id;
        return !ids.has(id as UnsupportedCapability["id"]);
      })
    : spec.capabilities;
  return {
    ...spec,
    capabilities,
    unsupportedCapabilities: marks.map((mark) => ({ ...mark })),
  };
}

function previewFor(
  info: HubModelInfo,
  systemVramGb: number,
): DiscoveryPreview {
  const available = entryChoices(info);
  const choices: DiscoveryChoice[] = [];
  for (const file of available.choices.slice(0, MAX_CHOICES)) {
    let entry: GeneratedEntry;
    try {
      entry = generateEntry(info, { repo: info.id, selector: file.path });
    } catch {
      continue;
    }
    let fit: DiscoveryChoice["fit"] = null;
    try {
      const evaluation = evaluateModelFit(
        modelSpecSchema.parse(entry.spec) as ModelSpec,
        systemVramGb,
      );
      fit = {
        verdict: evaluation.verdict,
        minVramGb: evaluation.minVramGb,
        message: evaluation.message,
      };
    } catch {
      fit = null;
    }
    choices.push({
      path: file.path,
      sizeBytes: downloadBytes(entry),
      partCount: entry.weights.length,
      quant: quantOf(file.path),
      modelId: entry.modelId,
      existing: catalogOwner(entry.modelId),
      fit,
      annotation: choiceAnnotation(info, file.path),
    });
  }
  if (choices.length === 0) {
    throw new HubError(`${info.id} has no files models add can use.`);
  }
  const addable = (choice: DiscoveryChoice) =>
    choice.existing === null || choice.existing === "user";
  const preferred = available.default
    ? choices.find((choice) => choice.path === available.default!.path)
    : undefined;
  const fallback =
    choices.length === 1 || available.default ? choices.find(addable) : null;
  const picked = preferred && addable(preferred) ? preferred : fallback;
  return {
    kind: available.kind,
    choices,
    defaultPath: picked?.path ?? null,
  };
}

/** Every file the entry downloads, including shared pipeline encoders. */
function downloadBytes(entry: GeneratedEntry): number {
  const weights = entry.weights.reduce((sum, weight) => sum + weight.size, 0);
  const artifacts = (entry.spec.artifacts ?? []) as ReadonlyArray<{
    expectedSizeBytes?: number;
  }>;
  const pinned = artifacts.reduce(
    (sum, artifact) => sum + (artifact.expectedSizeBytes ?? 0),
    0,
  );
  return Math.max(weights, pinned);
}

async function mapLimited<T, R>(
  items: readonly T[],
  limit: number,
  run: (item: T) => Promise<R>,
): Promise<R[]> {
  const results: R[] = new Array(items.length);
  let next = 0;
  const workers = Array.from(
    { length: Math.min(limit, items.length) },
    async () => {
      while (next < items.length) {
        const index = next++;
        results[index] = await run(items[index]!);
      }
    },
  );
  await Promise.all(workers);
  return results;
}

/**
 * Searches the Hub and previews the most downloaded addable results: the
 * files LocalBase would pin and a memory-fit estimate from their size.
 */
export async function discoverModels(
  options: HubClientOptions &
    Readonly<{
      kind: AddableKind;
      systemVramGb: number;
      limit?: number;
      previewLimit?: number;
    }>,
  query: string,
): Promise<Readonly<{ hits: DiscoveryHit[]; cached: boolean }>> {
  const { hits, cached } = await searchHub(
    { ...options, limit: options.limit ?? 20 },
    query,
  );
  let previews = options.previewLimit ?? 12;
  const previewed = await mapLimited(
    hits,
    PREVIEW_CONCURRENCY,
    async (hit): Promise<DiscoveryHit> => {
      const url = hubRepoUrl(hit.repo);
      const catalog = catalogNotesFor(hit.repo, hit.sha);
      if (hit.likely.kind === null) {
        return {
          ...hit,
          url,
          preview: null,
          reason: hit.likely.label,
          catalog,
        };
      }
      if (previews <= 0) {
        return { ...hit, url, preview: null, reason: null, catalog };
      }
      previews -= 1;
      try {
        const info = await fetchModelInfo(options, hit.repo);
        return {
          ...hit,
          url,
          preview: previewFor(info, options.systemVramGb),
          reason: null,
          catalog: catalogNotesFor(hit.repo, info.sha),
        };
      } catch (error) {
        return {
          ...hit,
          url,
          preview: null,
          reason:
            error instanceof HubError
              ? error.message
              : "Could not read this repository from Hugging Face.",
          ...(error instanceof HubError && error.code
            ? { reasonCode: error.code }
            : {}),
          catalog,
        };
      }
    },
  );
  return { hits: previewed, cached };
}

/** Previews one repository, for results that were not previewed in search. */
export async function previewRepository(
  options: HubClientOptions & Readonly<{ systemVramGb: number }>,
  repo: string,
): Promise<DiscoveryPreview> {
  const info = await fetchModelInfo(options, repo);
  return previewFor(info, options.systemVramGb);
}

/** The published annotation whose recipe installs `modelId`. */
function annotationForRecipe(modelId: string): FeedAnnotation | undefined {
  return feedAnnotations().find(
    (annotation) => annotation.recipe?.modelId === modelId,
  );
}

export class CatalogConflictError extends Error {
  override name = "CatalogConflictError";
}

/**
 * Pins one repository file as a user catalog entry. Refuses ids a built-in
 * or remote entry owns; replaces an existing user entry with the same id.
 */
export async function addFromHub(
  options: HubClientOptions,
  input: Readonly<{ repo: string; selector?: string; modelId?: string }>,
): Promise<
  Readonly<{
    entry: GeneratedEntry;
    path: string;
    model: ModelSpec;
    replaced: boolean;
  }>
> {
  const conflict = (modelId: string) => {
    const owner = catalogOwner(modelId);
    if (owner !== null && owner !== "user") {
      throw new CatalogConflictError(
        `"${modelId}" is already a ${owner} catalog model. Choose another id.`,
      );
    }
    return owner === "user";
  };
  if (input.modelId) conflict(input.modelId);
  const info = await fetchModelInfo(options, input.repo);
  const entry = generateEntry(
    info,
    {
      repo: input.repo,
      ...(input.selector ? { selector: input.selector } : {}),
    },
    input.modelId ? { modelId: input.modelId } : {},
  );
  const replaced = conflict(entry.modelId);
  const primary = entry.weights[0]?.path;
  const match = primary ? annotationForFile(info.id, info.sha, primary) : null;
  const spec = withFeedMarks(
    entry.spec,
    match?.revisionMatch
      ? (match.annotation.unsupportedCapabilities ?? [])
      : [],
  );
  const { path, model } = writeUserCatalogEntry(options.root, spec);
  return { entry, path, model, replaced };
}

const repoField = z.string().min(3).max(200);

/** The admin UI's discovery requests; every action is a POST body. */
export const discoveryRequestSchema = z.discriminatedUnion("action", [
  z
    .object({
      action: z.literal("search"),
      query: z.string().max(200).default(""),
      kind: z.enum(ADDABLE_KINDS).default("llm"),
    })
    .strict(),
  z.object({ action: z.literal("preview"), repo: repoField }).strict(),
  z
    .object({
      action: z.literal("add"),
      repo: repoField,
      selector: z.string().min(1).max(300).optional(),
      modelId: z.string().min(1).max(200).optional(),
      install: z.boolean().default(false),
    })
    .strict(),
  z
    .object({
      action: z.literal("install-recipe"),
      modelId: z.string().min(1).max(200),
    })
    .strict(),
]);
export type DiscoveryRequest = z.infer<typeof discoveryRequestSchema>;

export type DiscoveryDependencies = HubClientOptions &
  Readonly<{
    systemVramGb: number;
    /** Starts an install for a just-added model; returns its operation. */
    install: (modelId: string) => Promise<unknown>;
    /** Lets the runtime see the new catalog entry. */
    refresh: () => Promise<void>;
  }>;

export type DiscoveryOutcome = Readonly<{
  status: number;
  body:
    | Readonly<Record<string, unknown>>
    | Readonly<{ error: Readonly<{ message: string; code: string }> }>;
}>;

function failure(
  status: number,
  code: string,
  message: string,
): DiscoveryOutcome {
  return { status, body: { error: { message, code } } };
}

/**
 * Runs one discovery request for the gateway. Hub failures become 502s and
 * bad input 400s; the token never appears in any outcome.
 */
export async function runDiscoveryRequest(
  deps: DiscoveryDependencies,
  request: DiscoveryRequest,
): Promise<DiscoveryOutcome> {
  try {
    if (request.action === "search") {
      const { hits, cached } = await discoverModels(
        { ...deps, kind: request.kind },
        request.query,
      );
      return {
        status: 200,
        body: { query: request.query, kind: request.kind, cached, hits },
      };
    }
    if (request.action === "preview") {
      return {
        status: 200,
        body: {
          repo: request.repo,
          url: hubRepoUrl(request.repo),
          preview: await previewRepository(deps, request.repo),
        },
      };
    }
    if (request.action === "install-recipe") {
      const model = byId(request.modelId);
      if (!model)
        return failure(
          404,
          "model_not_found",
          `"${request.modelId}" is not in this catalog. Update LocalBase or the catalog feed.`,
        );
      const published = annotationForRecipe(request.modelId);
      if (!published)
        return failure(
          400,
          "no_recipe",
          `The catalog feed has no install recipe for "${request.modelId}".`,
        );
      const operation = await deps.install(model.modelId);
      return {
        status: 200,
        body: {
          modelId: model.modelId,
          displayName: model.displayName,
          kind: model.kind,
          replaced: false,
          operation,
        },
      };
    }
    const added = await addFromHub(deps, request);
    await deps.refresh();
    const operation = request.install
      ? await deps.install(added.model.modelId)
      : null;
    return {
      status: 200,
      body: {
        modelId: added.model.modelId,
        displayName: added.model.displayName,
        kind: added.entry.kind,
        repo: added.entry.repo,
        revision: added.entry.revision,
        files: added.entry.weights.map((file) => ({
          path: file.path,
          sizeBytes: file.size,
          sha256: file.sha256,
        })),
        replaced: added.replaced,
        operation,
      },
    };
  } catch (error) {
    if (error instanceof CatalogConflictError)
      return failure(409, "catalog_conflict", error.message);
    if (error instanceof z.ZodError)
      return failure(
        400,
        "validation_failed",
        "Expected a Hugging Face repository as <org>/<repo>.",
      );
    if (error instanceof HubError) {
      const upstream =
        /Could not reach|answered \d|not JSON|does not understand/.test(
          error.message,
        );
      return failure(
        upstream ? 502 : 400,
        error.code ?? (upstream ? "hub_unavailable" : "hub_rejected"),
        error.message,
      );
    }
    throw error;
  }
}
