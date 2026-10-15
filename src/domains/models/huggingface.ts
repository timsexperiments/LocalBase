/**
 * Hugging Face Hub client for `models add hf:` and `models search`.
 *
 * Only these commands talk to the Hub; nothing on the request path does.
 * Responses are cached under `<root>/cache/hf/`; a failed request falls back
 * to the cached copy when one exists, and otherwise fails with a plain
 * explanation instead of a stack trace.
 */
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { basename, join } from "node:path";
import { z } from "zod";
import { CATALOG, type ModelKind } from "../../catalog";
import { analyzeChatTemplate } from "./capability-detection";
import type { Capability } from "../../catalog/capabilities";

export const HF_ENDPOINT = "https://huggingface.co";

/** Fresh cache entries are reused without asking the Hub again. */
const CACHE_TTL_MS = 10 * 60 * 1000;
const REQUEST_TIMEOUT_MS = 15_000;

export type HubFetch = (
  url: string,
  init?: RequestInit,
) => Promise<Pick<Response, "ok" | "status" | "json">>;

export type HubClientOptions = Readonly<{
  root: string;
  fetch?: HubFetch;
  /** Sent only to huggingface.co, for gated repositories. Never logged. */
  token?: string;
  now?: () => number;
}>;

export class HubError extends Error {
  override name = "HubError";
}

const repoIdSchema = z
  .string()
  .regex(
    /^[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._-]*$/,
    "expected <org>/<repo>",
  )
  .refine((id) => !id.split("/").some((part) => part === ".." || part === "."));

const siblingSchema = z.object({
  rfilename: z.string().min(1),
  size: z.number().int().nonnegative().optional(),
  lfs: z
    .object({
      sha256: z.string().regex(/^[a-fA-F0-9]{64}$/),
      size: z.number().int().positive(),
    })
    .passthrough()
    .optional(),
});

const modelInfoSchema = z
  .object({
    id: z.string().min(1),
    sha: z.string().regex(/^[a-fA-F0-9]{40}$/),
    pipeline_tag: z.string().optional(),
    tags: z.array(z.string()).default([]),
    gated: z.union([z.boolean(), z.string()]).optional(),
    cardData: z
      .object({
        license: z.union([z.string(), z.array(z.string())]).optional(),
      })
      .passthrough()
      .optional(),
    gguf: z
      .object({
        architecture: z.string().optional(),
        context_length: z.number().int().positive().optional(),
        chat_template: z.string().optional(),
      })
      .passthrough()
      .optional(),
    siblings: z.array(siblingSchema).default([]),
  })
  .passthrough();
export type HubModelInfo = z.infer<typeof modelInfoSchema>;

const searchHitSchema = z
  .object({
    id: z.string().min(1),
    downloads: z.number().int().nonnegative().default(0),
    likes: z.number().int().nonnegative().optional(),
    pipeline_tag: z.string().optional(),
    tags: z.array(z.string()).default([]),
  })
  .passthrough();

const cacheEntrySchema = z.object({
  url: z.string(),
  fetchedAt: z.number(),
  body: z.unknown(),
});

function cachePath(root: string, url: string): string {
  const key = createHash("sha256").update(url).digest("hex").slice(0, 32);
  return join(root, "cache", "hf", `${key}.json`);
}

function readCache(root: string, url: string) {
  try {
    const entry = cacheEntrySchema.parse(
      JSON.parse(readFileSync(cachePath(root, url), "utf8")),
    );
    return entry.url === url ? entry : null;
  } catch {
    return null;
  }
}

function writeCache(root: string, url: string, body: unknown, now: number) {
  // Never create files in a directory that is not a LocalBase root yet;
  // `local-base init` refuses nonempty directories.
  if (!existsSync(join(root, ".localbase-root.json"))) return;
  try {
    const path = cachePath(root, url);
    mkdirSync(join(root, "cache", "hf"), { recursive: true, mode: 0o700 });
    const temporary = `${path}.${process.pid}.tmp`;
    writeFileSync(temporary, JSON.stringify({ url, fetchedAt: now, body }), {
      mode: 0o600,
    });
    renameSync(temporary, path);
  } catch {
    // The cache is an optimisation; a read-only root still works online.
  }
}

/** Fetches JSON from the Hub with caching and offline-safe errors. */
async function hubJson(
  options: HubClientOptions,
  url: string,
  what: string,
): Promise<{ body: unknown; cached: boolean }> {
  const now = options.now?.() ?? Date.now();
  const cached = readCache(options.root, url);
  if (cached && now - cached.fetchedAt < CACHE_TTL_MS) {
    return { body: cached.body, cached: true };
  }
  const fetcher = options.fetch ?? (fetch as unknown as HubFetch);
  let response: Awaited<ReturnType<HubFetch>>;
  try {
    response = await fetcher(url, {
      headers: {
        accept: "application/json",
        ...(options.token && new URL(url).origin === HF_ENDPOINT
          ? { authorization: `Bearer ${options.token}` }
          : {}),
      },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch (error) {
    if (cached) return { body: cached.body, cached: true };
    throw new HubError(
      `Could not reach Hugging Face to ${what} (${(error as Error).message}). Check the network connection and try again; nothing was changed.`,
    );
  }
  if (!response.ok) {
    if (response.status >= 500 && cached) {
      return { body: cached.body, cached: true };
    }
    throw new HubError(
      response.status === 404
        ? `Hugging Face has no public repository for that name (${what}).`
        : response.status === 401 || response.status === 403
          ? `Hugging Face refused access (${response.status}) while trying to ${what}. The repository may be gated: accept its terms on huggingface.co and save a token with \`local-base configure --hf-token\`.`
          : `Hugging Face answered ${response.status} while trying to ${what}. Try again later; nothing was changed.`,
    );
  }
  let body: unknown;
  try {
    body = await response.json();
  } catch {
    throw new HubError(
      `Hugging Face sent a response that is not JSON while trying to ${what}.`,
    );
  }
  writeCache(options.root, url, body, now);
  return { body, cached: false };
}

export async function fetchModelInfo(
  options: HubClientOptions,
  repo: string,
): Promise<HubModelInfo> {
  const repoId = repoIdSchema.parse(repo);
  const url = `${HF_ENDPOINT}/api/models/${repoId}?blobs=true`;
  const { body } = await hubJson(options, url, `look up ${repoId}`);
  const parsed = modelInfoSchema.safeParse(body);
  if (!parsed.success) {
    throw new HubError(
      `Hugging Face returned model information for ${repoId} that LocalBase does not understand.`,
    );
  }
  return parsed.data;
}

// ---------------------------------------------------------------------------
// Classification

/** The `license:` tag or card license, lowercased. */
export function hubLicense(
  info: Pick<HubModelInfo, "tags" | "cardData">,
): string | null {
  const card = info.cardData?.license;
  const fromCard = Array.isArray(card) ? card[0] : card;
  const fromTag = info.tags
    .find((tag) => tag.startsWith("license:"))
    ?.slice("license:".length);
  const license = fromCard ?? fromTag;
  return license ? license.toLowerCase() : null;
}

const OPEN_LICENSES = new Set([
  "apache-2.0",
  "mit",
  "bsd",
  "bsd-2-clause",
  "bsd-3-clause",
  "cc-by-4.0",
  "cc-by-sa-4.0",
  "cc0-1.0",
  "unlicense",
  "openrail",
  "bigscience-openrail-m",
  "creativeml-openrail-m",
  "openrail++",
  "artistic-2.0",
  "mpl-2.0",
  "gpl-3.0",
  "lgpl-3.0",
  "agpl-3.0",
]);

export function commercialStatusForLicense(
  license: string | null,
): "open" | "conditional" | "prohibited" {
  if (!license) return "conditional";
  if (/(^|-)nc($|-)|non-?commercial|research/.test(license))
    return "prohibited";
  return OPEN_LICENSES.has(license) ? "open" : "conditional";
}

const TEXT_PIPELINES = new Set([
  "text-generation",
  "text2text-generation",
  "conversational",
]);
const VISION_PIPELINES = new Set([
  "image-text-to-text",
  "visual-question-answering",
]);

export type LikelyKind = Readonly<{
  /** What LocalBase would run it as, or null when it cannot. */
  kind: ModelKind | null;
  label: string;
}>;

/** Classifies a repository from its pipeline tag and tags. */
export function likelyKind(
  pipelineTag: string | undefined,
  tags: readonly string[],
): LikelyKind {
  if (pipelineTag && VISION_PIPELINES.has(pipelineTag))
    return { kind: "llm", label: "llm (vision)" };
  if (pipelineTag && TEXT_PIPELINES.has(pipelineTag))
    return { kind: "llm", label: "llm" };
  if (
    pipelineTag === "feature-extraction" ||
    pipelineTag === "sentence-similarity"
  )
    return { kind: null, label: "embeddings (not supported by models add)" };
  if (pipelineTag === "automatic-speech-recognition")
    return {
      kind: null,
      label: "speech to text (not supported by models add)",
    };
  if (pipelineTag === "text-to-speech")
    return {
      kind: null,
      label: "text to speech (not supported by models add)",
    };
  if (pipelineTag === "text-to-image")
    return { kind: null, label: "image (not supported by models add)" };
  if (!pipelineTag && tags.includes("gguf"))
    return { kind: "llm", label: "llm (no pipeline tag)" };
  return {
    kind: null,
    label: pipelineTag ? `${pipelineTag} (not supported)` : "unknown",
  };
}

// ---------------------------------------------------------------------------
// Search

export type HubSearchHit = Readonly<{
  repo: string;
  downloads: number;
  likes?: number;
  license: string | null;
  pipelineTag: string | null;
  likely: LikelyKind;
}>;

export async function searchHub(
  options: HubClientOptions & Readonly<{ limit?: number }>,
  query: string,
): Promise<Readonly<{ hits: HubSearchHit[]; cached: boolean }>> {
  // Only GGUF files run on llama.cpp, so the search is limited to GGUF repos
  // and the word "gguf" adds nothing to the query.
  const text = query
    .split(/\s+/)
    .filter((word) => word && word.toLowerCase() !== "gguf")
    .join(" ");
  const params = new URLSearchParams({
    filter: "gguf",
    sort: "downloads",
    direction: "-1",
    limit: String(options.limit ?? 20),
  });
  if (text) params.set("search", text);
  const url = `${HF_ENDPOINT}/api/models?${params}`;
  const { body, cached } = await hubJson(options, url, "search models");
  const parsed = z.array(searchHitSchema).safeParse(body);
  if (!parsed.success) {
    throw new HubError(
      "Hugging Face returned search results LocalBase does not understand.",
    );
  }
  return {
    cached,
    hits: parsed.data.map((hit) => ({
      repo: hit.id,
      downloads: hit.downloads,
      ...(hit.likes === undefined ? {} : { likes: hit.likes }),
      license: hubLicense({ tags: hit.tags }),
      pipelineTag: hit.pipeline_tag ?? null,
      likely: likelyKind(hit.pipeline_tag, hit.tags),
    })),
  };
}

// ---------------------------------------------------------------------------
// Entry generation

export type HfReference = Readonly<{ repo: string; selector?: string }>;

/** Parses `hf:<org>/<repo>[:<file-or-quant>]`. */
export function parseHfReference(value: string): HfReference {
  const match = /^hf:([^:]+)(?::(.+))?$/.exec(value.trim());
  if (!match) {
    throw new HubError(
      `Expected hf:<org>/<repo>[:<file-or-quant>], got "${value}".`,
    );
  }
  const repo = repoIdSchema.safeParse(match[1]);
  if (!repo.success) {
    throw new HubError(
      `"${match[1]}" is not a Hugging Face repository name (expected <org>/<repo>).`,
    );
  }
  return { repo: repo.data, ...(match[2] ? { selector: match[2] } : {}) };
}

type HubFile = Readonly<{ path: string; size: number; sha256: string }>;

const SHARD = /-(\d{5})-of-(\d{5})\.gguf$/i;
const QUANT =
  /(?:^|[-_.])((?:IQ|Q)\d(?:_[A-Z0-9]+)*|BF16|F16|F32|MXFP4(?:_MOE)?)(?=[-_.]|$)/i;
const PREFERRED_QUANTS = ["Q4_K_M", "Q4_K_S", "Q5_K_M", "Q8_0"];

function lfsFiles(info: HubModelInfo): HubFile[] {
  return info.siblings.flatMap((sibling) =>
    sibling.lfs
      ? [
          {
            path: sibling.rfilename,
            size: sibling.lfs.size,
            sha256: sibling.lfs.sha256.toLowerCase(),
          },
        ]
      : [],
  );
}

export function quantOf(path: string): string | null {
  return (
    QUANT.exec(basename(path).replace(/\.gguf$/i, ""))?.[1]?.toUpperCase() ??
    null
  );
}

/** First shard or single-file GGUF weights, without projectors. */
function weightChoices(files: readonly HubFile[]): HubFile[] {
  return files.filter(
    (file) =>
      /\.gguf$/i.test(file.path) &&
      !/mmproj/i.test(basename(file.path)) &&
      (!SHARD.test(file.path) || SHARD.exec(file.path)![1] === "00001"),
  );
}

function selectWeights(
  repo: string,
  files: readonly HubFile[],
  selector: string | undefined,
): HubFile {
  const choices = weightChoices(files);
  if (choices.length === 0) {
    throw new HubError(
      `${repo} has no GGUF weights LocalBase can download. LocalBase runs language models as GGUF files; look for a repository with "GGUF" in its name (try \`local-base models search\`).`,
    );
  }
  const list = () =>
    choices
      .map(
        (file) =>
          `  ${file.path}${quantOf(file.path) ? ` (${quantOf(file.path)})` : ""}`,
      )
      .join("\n");
  if (selector) {
    const wanted = selector.toLowerCase();
    const byPath = choices.find(
      (file) =>
        file.path.toLowerCase() === wanted ||
        basename(file.path).toLowerCase() === wanted,
    );
    if (byPath) return byPath;
    const byQuant = choices.filter(
      (file) => quantOf(file.path)?.toLowerCase() === wanted,
    );
    if (byQuant.length === 1) return byQuant[0]!;
    throw new HubError(
      byQuant.length > 1
        ? `More than one file in ${repo} is ${selector.toUpperCase()}; name the file instead:\n${list()}`
        : `${repo} has no GGUF file or quant named "${selector}". Choose one of:\n${list()}`,
    );
  }
  if (choices.length === 1) return choices[0]!;
  for (const quant of PREFERRED_QUANTS) {
    const match = choices.filter((file) => quantOf(file.path) === quant);
    if (match.length === 1) return match[0]!;
  }
  throw new HubError(
    `${repo} has ${choices.length} GGUF files; pick one with hf:${repo}:<file-or-quant>:\n${list()}`,
  );
}

function shardsOf(files: readonly HubFile[], first: HubFile): HubFile[] {
  const match = SHARD.exec(first.path);
  if (!match) return [first];
  const prefix = first.path.slice(0, match.index);
  const total = match[2]!;
  const shards = files
    .filter((file) => {
      const shard = SHARD.exec(file.path);
      return (
        shard &&
        shard[2] === total &&
        file.path.slice(0, shard.index) === prefix
      );
    })
    .sort((a, b) => a.path.localeCompare(b.path));
  if (shards.length !== Number(total)) {
    throw new HubError(
      `${first.path} is split into ${Number(total)} parts but ${shards.length} are in the repository.`,
    );
  }
  return shards;
}

/** Picks the projector to pair with the weights: f16 first, then any. */
function selectProjector(files: readonly HubFile[]): HubFile | null {
  const projectors = files.filter(
    (file) => /\.gguf$/i.test(file.path) && /mmproj/i.test(basename(file.path)),
  );
  return (
    projectors.find((file) => /(^|[^b])f16/i.test(basename(file.path))) ??
    projectors[0] ??
    null
  );
}

export function modelIdFor(path: string): string {
  return basename(path)
    .replace(SHARD, "")
    .replace(/\.gguf$/i, "")
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^[-._]+|[-._]+$/g, "");
}

function sizeOf(name: string): string {
  const match = /(?:^|[-_.])(\d+(?:\.\d+)?)([BM])(?=[-_.]|$)/i.exec(name);
  return match ? `${match[1]}${match[2]!.toUpperCase()}` : "unknown";
}

/** Filenames other catalog models already install, to avoid collisions. */
function takenFilenames(modelId: string): Set<string> {
  return new Set(
    CATALOG.filter((model) => model.modelId !== modelId).flatMap((model) =>
      model.artifacts.map(({ filename }) => filename),
    ),
  );
}

export type GeneratedEntry = Readonly<{
  /** The JSON written to `catalog.d`. */
  spec: Record<string, unknown>;
  modelId: string;
  repo: string;
  revision: string;
  license: string | null;
  pipelineTag: string | null;
  weights: readonly HubFile[];
  projector: HubFile | null;
  /** Capabilities the Hub's GGUF metadata shows, before install. */
  detected: readonly Capability[];
  contextLength: number | null;
}>;

/**
 * Builds a user catalog entry for one GGUF file. Kind and declared
 * capabilities come from the pipeline tag and tags; capabilities read from
 * the GGUF chat template are reported as `detected` and confirmed again from
 * the installed file, never written into the catalog.
 */
export function generateEntry(
  info: HubModelInfo,
  reference: HfReference,
  options: Readonly<{ modelId?: string }> = {},
): GeneratedEntry {
  const repo = info.id;
  const likely = likelyKind(info.pipeline_tag, info.tags);
  if (likely.kind !== "llm") {
    throw new HubError(
      `${repo} looks like ${likely.label}. models add only supports GGUF language models for now.`,
    );
  }
  const files = lfsFiles(info);
  const first = selectWeights(repo, files, reference.selector);
  const weights = shardsOf(files, first);
  const modelId = options.modelId ?? modelIdFor(first.path);
  if (!/^[a-z0-9][a-z0-9._-]*$/.test(modelId)) {
    throw new HubError(
      `"${modelId}" is not a usable model id; choose one with --id (lowercase letters, digits, ".", "_" and "-").`,
    );
  }
  const projector = selectProjector(files);
  const taken = takenFilenames(modelId);
  const filenameFor = (file: HubFile, sharded: boolean) => {
    const name = basename(file.path);
    // Shard names must stay intact; llama.cpp finds the parts by name.
    return taken.has(name) && !sharded ? `${modelId}-${name}` : name;
  };
  const projectorFilename = projector
    ? (() => {
        const name = basename(projector.path);
        // Projectors share generic names across repositories.
        return name.toLowerCase().includes(modelId)
          ? name
          : `${modelId}-${name}`;
      })()
    : null;
  for (const file of weights) {
    if (SHARD.test(file.path) && taken.has(basename(file.path))) {
      throw new HubError(
        `${basename(file.path)} is already installed for another catalog model.`,
      );
    }
  }
  const license = hubLicense(info);
  const repoName = repo.split("/")[1]!;
  const family = repoName.replace(/[-_]?GGUF$/i, "") || repoName;
  const quant = quantOf(first.path) ?? "unknown";
  const vision =
    projector !== null &&
    (likely.label === "llm (vision)" ||
      info.tags.some(
        (tag) =>
          tag === "vision" ||
          tag === "multimodal" ||
          tag === "image-text-to-text",
      ));
  const coder = /coder|code/i.test(repo) || info.tags.includes("code");
  const template = info.gguf?.chat_template;
  const opinions = template ? analyzeChatTemplate(template) : {};
  const detected = (Object.entries(opinions) as [Capability, boolean][])
    .filter(([, value]) => value)
    .map(([id]) => id);
  const contextLength = info.gguf?.context_length ?? null;
  const totalBytes =
    weights.reduce((sum, file) => sum + file.size, 0) +
    (vision ? projector!.size : 0);
  const sharded = weights.length > 1;
  const spec: Record<string, unknown> = {
    modelId,
    displayName: `${family} ${quant}`,
    kind: "llm",
    provider: repo.split("/")[0],
    family,
    version: info.sha.slice(0, 7),
    size:
      sizeOf(repoName) !== "unknown" ? sizeOf(repoName) : sizeOf(first.path),
    quant,
    storageGb: Math.max(0.01, Math.round((totalBytes / 1e9) * 100) / 100),
    source: `${HF_ENDPOINT}/${repo}`,
    repositoryRevision: info.sha,
    artifacts: [
      ...weights.map((file, index) => ({
        sourcePath: file.path,
        filename: filenameFor(file, sharded),
        expectedSizeBytes: file.size,
        sha256: file.sha256,
        role: index === 0 ? "primary" : "supplementary",
      })),
      ...(vision
        ? [
            {
              sourcePath: projector!.path,
              filename: projectorFilename!,
              expectedSizeBytes: projector!.size,
              sha256: projector!.sha256,
              role: "supplementary",
            },
          ]
        : []),
    ],
    inputModalities: vision ? ["text", "image"] : ["text"],
    outputModalities: ["text"],
    contextWindowTokens: contextLength,
    capabilities: ["text-generation", ...(coder ? ["code-generation"] : [])],
    commercialStatus: commercialStatusForLicense(license),
    catch: `License: ${license ?? "not stated"}. Added from Hugging Face; not qualified by LocalBase.`,
    notes: `Added with local-base models add hf:${repo}:${basename(first.path)} at revision ${info.sha}${info.pipeline_tag ? ` (pipeline ${info.pipeline_tag})` : ""}.`,
  };
  return {
    spec,
    modelId,
    repo,
    revision: info.sha,
    license,
    pipelineTag: info.pipeline_tag ?? null,
    weights,
    projector: vision ? projector : null,
    detected,
    contextLength,
  };
}
