/**
 * Search-first model discovery: Hugging Face search results with the files
 * `models add` would pin and how each fits this machine's memory, and the
 * add step the admin UI runs. The CLI and the gateway share these helpers so
 * both write the same pinned user entries.
 */
import { z } from "zod";
import { byId, modelSpecSchema, type ModelSpec } from "../../catalog";
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
  quant: string | null;
  modelId: string;
  /** The catalog tier that already has this id, if any. */
  existing: CatalogOwner;
  fit: Readonly<{
    verdict: MemoryFitVerdict;
    minVramGb: number;
    message: string;
  }> | null;
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
  }>;

/** Repositories with more files are trimmed to keep responses small. */
const MAX_CHOICES = 40;
const PREVIEW_CONCURRENCY = 4;

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
      sizeBytes: entry.weights.reduce((sum, weight) => sum + weight.size, 0),
      quant: quantOf(file.path),
      modelId: entry.modelId,
      existing: catalogOwner(entry.modelId),
      fit,
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
      if (hit.likely.kind === null) {
        return { ...hit, url, preview: null, reason: hit.likely.label };
      }
      if (previews <= 0) {
        return { ...hit, url, preview: null, reason: null };
      }
      previews -= 1;
      try {
        const info = await fetchModelInfo(options, hit.repo);
        return {
          ...hit,
          url,
          preview: previewFor(info, options.systemVramGb),
          reason: null,
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
  const { path, model } = writeUserCatalogEntry(options.root, entry.spec);
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
        upstream ? "hub_unavailable" : "hub_rejected",
        error.message,
      );
    }
    throw error;
  }
}
