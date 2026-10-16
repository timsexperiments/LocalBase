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
import { CATALOG } from "../../catalog";
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
    library_name: z.string().optional(),
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
    library_name: z.string().optional(),
    tags: z.array(z.string()).default([]),
    gated: z.union([z.boolean(), z.string()]).optional(),
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

/** Kinds `models add` can write an entry for. */
export const ADDABLE_KINDS = ["llm", "stt", "image"] as const;
export type AddableKind = (typeof ADDABLE_KINDS)[number];

export type LikelyKind = Readonly<{
  /** What LocalBase would run it as, or null when it cannot. */
  kind: AddableKind | null;
  label: string;
}>;

/** Libraries whose speech weights whisper.cpp cannot load. */
const NON_GGML_SPEECH_LIBRARIES = new Set([
  "transformers",
  "ctranslate2",
  "whisperkit",
  "nemo",
  "mlx",
  "onnx",
  "coreml",
  "pytorch",
  "speechbrain",
  "espnet",
  "transformers.js",
]);

/**
 * Image pipelines sd-server can only run with several separately mapped
 * files (diffusion model, text encoders, VAE) or not at all. LocalBase's
 * reference entries cover some of them; `models add` writes single-file
 * checkpoints only.
 */
const MULTI_FILE_IMAGE =
  /flux|sd-?3|stable-diffusion-3|qwen[-_]?image|(^|[^a-z])wan[-_]?\d|hunyuan|lumina|z[-_]?image|kolors|pixart|sana|hidream|cogview|auraflow|chroma|omnigen|krea|minimax|video/i;

export type KindContext = Readonly<{
  repo?: string;
  library?: string | null;
}>;

/** Classifies a repository from its pipeline tag, tags and library. */
export function likelyKind(
  pipelineTag: string | undefined,
  tags: readonly string[],
  context: KindContext = {},
): LikelyKind {
  const repo = context.repo ?? "";
  const library = context.library?.toLowerCase() ?? null;
  if (pipelineTag && VISION_PIPELINES.has(pipelineTag))
    return { kind: "llm", label: "llm (vision)" };
  if (pipelineTag && TEXT_PIPELINES.has(pipelineTag))
    return { kind: "llm", label: "llm" };
  if (
    pipelineTag === "feature-extraction" ||
    pipelineTag === "sentence-similarity"
  )
    return { kind: null, label: "embeddings (not supported by models add)" };
  if (pipelineTag === "automatic-speech-recognition") {
    if (library && NON_GGML_SPEECH_LIBRARIES.has(library))
      return {
        kind: null,
        label: `speech to text (${library} weights; whisper.cpp needs ggml files)`,
      };
    if (!/whisper/i.test(repo) && !tags.some((tag) => /whisper/i.test(tag)))
      return {
        kind: null,
        label: "speech to text (only Whisper models run on whisper.cpp)",
      };
    return { kind: "stt", label: "speech to text" };
  }
  if (pipelineTag === "text-to-speech")
    return {
      kind: null,
      label: "text to speech (not supported by models add)",
    };
  const imageLike =
    pipelineTag === "text-to-image" ||
    (!pipelineTag && library === "diffusion-single-file");
  if (imageLike) {
    if (
      library === "peft" ||
      tags.includes("lora") ||
      tags.some((tag) => tag.startsWith("base_model:adapter:"))
    )
      return { kind: null, label: "image LoRA (not supported by models add)" };
    if (library === "mlx" || library === "onnx" || tags.includes("coreml"))
      return {
        kind: null,
        label: `image (${library ?? "coreml"} weights; sd.cpp needs a checkpoint file)`,
      };
    if (MULTI_FILE_IMAGE.test(repo))
      return {
        kind: null,
        label: "image (multi-file pipeline; not supported by models add)",
      };
    return { kind: "image", label: "image" };
  }
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
  libraryName: string | null;
  /** The repository asks users to accept terms before downloading. */
  gated: boolean;
  likely: LikelyKind;
}>;

const SEARCH_FIELDS = [
  "downloads",
  "likes",
  "pipeline_tag",
  "tags",
  "gated",
  "library_name",
] as const;

/**
 * Searches the Hub, most downloaded first. Language models are limited to
 * GGUF repositories (the only files llama.cpp runs), speech to recognition
 * repositories and images to text-to-image ones.
 */
export async function searchHub(
  options: HubClientOptions & Readonly<{ limit?: number; kind?: AddableKind }>,
  query: string,
): Promise<Readonly<{ hits: HubSearchHit[]; cached: boolean }>> {
  const kind = options.kind ?? "llm";
  const words = query.split(/\s+/).filter(Boolean);
  const text = (
    kind === "llm"
      ? words.filter((word) => word.toLowerCase() !== "gguf")
      : words
  ).join(" ");
  const params = new URLSearchParams({
    sort: "downloads",
    direction: "-1",
    limit: String(options.limit ?? 20),
  });
  if (kind === "llm") params.set("filter", "gguf");
  if (kind === "stt")
    params.set("pipeline_tag", "automatic-speech-recognition");
  if (kind === "image") params.set("pipeline_tag", "text-to-image");
  const search = text || (kind === "stt" ? "whisper" : "");
  if (search) params.set("search", search);
  for (const field of SEARCH_FIELDS) params.append("expand[]", field);
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
      libraryName: hit.library_name ?? null,
      gated: isGated(hit.gated),
      likely: likelyKind(hit.pipeline_tag, hit.tags, {
        repo: hit.id,
        library: hit.library_name ?? null,
      }),
    })),
  };
}

/** The Hub reports `false`, `"auto"` or `"manual"`. */
export function isGated(value: boolean | string | undefined): boolean {
  return value !== undefined && value !== false && value !== "false";
}

/** Where a user accepts a gated repository's terms. */
export function hubRepoUrl(repo: string): string {
  return `${HF_ENDPOINT}/${repoIdSchema.parse(repo)}`;
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

export type HubFile = Readonly<{ path: string; size: number; sha256: string }>;

const SHARD = /-(\d{5})-of-(\d{5})\.gguf$/i;
const QUANT =
  /(?:^|[-_.])((?:IQ|Q)\d(?:_[A-Z0-9]+)*|BF16|F16|F32|MXFP4(?:_MOE)?)(?=[-_.]|$)/i;
const PREFERRED_QUANTS = ["Q4_K_M", "Q4_K_S", "Q5_K_M", "Q8_0"];
const PREFERRED_IMAGE_QUANTS = ["Q8_0", "Q5_0", "Q5_1", "Q4_K", "Q4_0"];
const WEIGHT_EXTENSION = /\.(gguf|safetensors|ckpt|bin)$/i;
/** Image checkpoints smaller than this are VAEs, embeddings or adapters. */
const MIN_IMAGE_CHECKPOINT_BYTES = 500_000_000;

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
    QUANT.exec(
      basename(path).replace(WEIGHT_EXTENSION, ""),
    )?.[1]?.toUpperCase() ?? null
  );
}

/** First shard or single-file GGUF weights, without projectors. */
function llmChoices(files: readonly HubFile[]): HubFile[] {
  return files.filter(
    (file) =>
      /\.gguf$/i.test(file.path) &&
      !/mmproj/i.test(basename(file.path)) &&
      (!SHARD.test(file.path) || SHARD.exec(file.path)![1] === "00001"),
  );
}

/** whisper.cpp ggml model files; Core ML encoders and VAD models excluded. */
function sttChoices(files: readonly HubFile[]): HubFile[] {
  return files.filter((file) => {
    const name = basename(file.path);
    return (
      /ggml.*\.bin$/i.test(name) && !/encoder|coreml|silero|vad/i.test(name)
    );
  });
}

/** Single-file checkpoints sd-server loads with `-m`, at the repo root. */
function imageChoices(files: readonly HubFile[]): HubFile[] {
  return files.filter((file) => {
    const name = basename(file.path);
    return (
      !file.path.includes("/") &&
      /\.(safetensors|ckpt|gguf)$/i.test(name) &&
      file.size >= MIN_IMAGE_CHECKPOINT_BYTES &&
      !/vae|lora|lycoris|controlnet|refiner|inpaint|text[-_]?encoder|clip|t5|unet|embedding|adapter|mmproj/i.test(
        name,
      )
    );
  });
}

const bySize = (a: HubFile, b: HubFile) =>
  a.size - b.size || a.path.localeCompare(b.path);

/** The file chosen when the reference names none, or null if ambiguous. */
function defaultChoice(
  kind: AddableKind,
  choices: readonly HubFile[],
): HubFile | null {
  if (choices.length === 1) return choices[0]!;
  if (kind === "llm") {
    for (const quant of PREFERRED_QUANTS) {
      const match = choices.filter((file) => quantOf(file.path) === quant);
      if (match.length === 1) return match[0]!;
    }
    return null;
  }
  const sorted = [...choices].sort(bySize);
  if (kind === "stt") {
    return (
      sorted.find(
        (file) =>
          /base/i.test(basename(file.path)) && quantOf(file.path) === "Q8_0",
      ) ??
      sorted.find((file) => quantOf(file.path) === "Q8_0") ??
      sorted[0] ??
      null
    );
  }
  const safetensors = sorted.filter((file) =>
    /\.safetensors$/i.test(file.path),
  );
  if (safetensors.length > 0) return safetensors[0]!;
  for (const quant of PREFERRED_IMAGE_QUANTS) {
    const match = sorted.find((file) => quantOf(file.path) === quant);
    if (match) return match;
  }
  return sorted[0] ?? null;
}

const NO_FILES: Readonly<Record<AddableKind, string>> = {
  llm: 'has no GGUF weights LocalBase can download. LocalBase runs language models as GGUF files; look for a repository with "GGUF" in its name (try `local-base models search`).',
  stt: "has no whisper.cpp ggml-*.bin files. LocalBase runs speech recognition with whisper.cpp; try ggerganov/whisper.cpp or a repository with ggml files.",
  image:
    "has no single-file Stable Diffusion checkpoint (.safetensors, .ckpt or .gguf at the repository root). Diffusers folders, LoRAs and multi-file pipelines are not supported by models add.",
};

const NOUN: Readonly<Record<AddableKind, string>> = {
  llm: "GGUF",
  stt: "ggml",
  image: "checkpoint",
};

export type EntryChoices = Readonly<{
  kind: AddableKind;
  choices: readonly HubFile[];
  /** Picked when no file is named; null when the user must choose. */
  default: HubFile | null;
}>;

/**
 * The files `models add` could pin for a repository, after the kind check.
 * Throws a HubError explaining why the repository cannot be added.
 */
export function entryChoices(info: HubModelInfo): EntryChoices {
  const likely = likelyKind(info.pipeline_tag, info.tags, {
    repo: info.id,
    library: info.library_name ?? null,
  });
  if (likely.kind === null) {
    throw new HubError(
      `${info.id} looks like ${likely.label}. models add supports GGUF language models, whisper.cpp speech models and single-file Stable Diffusion checkpoints.`,
    );
  }
  const files = lfsFiles(info);
  const choices =
    likely.kind === "llm"
      ? llmChoices(files)
      : likely.kind === "stt"
        ? sttChoices(files)
        : imageChoices(files);
  if (choices.length === 0) {
    throw new HubError(`${info.id} ${NO_FILES[likely.kind]}`);
  }
  return {
    kind: likely.kind,
    choices,
    default: defaultChoice(likely.kind, choices),
  };
}

function selectFile(
  repo: string,
  available: EntryChoices,
  selector: string | undefined,
): HubFile {
  const { choices, kind } = available;
  const noun = NOUN[kind];
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
        basename(file.path).toLowerCase() === wanted ||
        basename(file.path).replace(WEIGHT_EXTENSION, "").toLowerCase() ===
          wanted ||
        modelIdFor(file.path, kind) === wanted,
    );
    if (byPath) return byPath;
    const byQuant = choices.filter(
      (file) => quantOf(file.path)?.toLowerCase() === wanted,
    );
    if (byQuant.length === 1) return byQuant[0]!;
    throw new HubError(
      byQuant.length > 1
        ? `More than one file in ${repo} is ${selector.toUpperCase()}; name the file instead:\n${list()}`
        : `${repo} has no ${noun} file or quant named "${selector}". Choose one of:\n${list()}`,
    );
  }
  if (available.default) return available.default;
  throw new HubError(
    `${repo} has ${choices.length} ${noun} files; pick one with hf:${repo}:<file-or-quant>:\n${list()}`,
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

function slug(value: string): string {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^[-._]+|[-._]+$/g, "");
}

/** The catalog id derived from a weights file name. */
export function modelIdFor(path: string, kind: AddableKind = "llm"): string {
  const stem = basename(path).replace(SHARD, "").replace(WEIGHT_EXTENSION, "");
  if (kind === "stt") {
    const variant = slug(stem.replace(/^ggml[-_]?/i, ""));
    return variant.includes("whisper") ? variant : `whisper-${variant}`;
  }
  return slug(stem);
}

function sizeOf(name: string): string {
  const match = /(?:^|[-_.])(\d+(?:\.\d+)?)([BM])(?=[-_.]|$)/i.exec(name);
  return match ? `${match[1]}${match[2]!.toUpperCase()}` : "unknown";
}

const WHISPER_SIZE = /(tiny|base|small|medium|large(?:-v\d)?(?:-turbo)?)/i;

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
  kind: AddableKind;
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
 * Builds a user catalog entry for one weights file: a GGUF language model,
 * a whisper.cpp ggml speech model or a single-file Stable Diffusion
 * checkpoint. Kind and declared capabilities come from the pipeline tag,
 * tags and file layout; capabilities read from a GGUF chat template are
 * reported as `detected` and confirmed again from the installed file, never
 * written into the catalog.
 */
export function generateEntry(
  info: HubModelInfo,
  reference: HfReference,
  options: Readonly<{ modelId?: string }> = {},
): GeneratedEntry {
  const repo = info.id;
  const available = entryChoices(info);
  const kind = available.kind;
  const files = lfsFiles(info);
  const first = selectFile(repo, available, reference.selector);
  const weights = kind === "llm" ? shardsOf(files, first) : [first];
  const modelId = options.modelId ?? modelIdFor(first.path, kind);
  if (!/^[a-z0-9][a-z0-9._-]*$/.test(modelId)) {
    throw new HubError(
      `"${modelId}" is not a usable model id; choose one with --id (lowercase letters, digits, ".", "_" and "-").`,
    );
  }
  const taken = takenFilenames(modelId);
  const filenameFor = (file: HubFile, sharded: boolean) => {
    const name = basename(file.path);
    // Shard names must stay intact; llama.cpp finds the parts by name.
    return taken.has(name) && !sharded ? `${modelId}-${name}` : name;
  };
  for (const file of weights) {
    if (SHARD.test(file.path) && taken.has(basename(file.path))) {
      throw new HubError(
        `${basename(file.path)} is already installed for another catalog model.`,
      );
    }
  }
  const license = hubLicense(info);
  const repoName = repo.split("/")[1]!;
  const stem = basename(first.path).replace(WEIGHT_EXTENSION, "");
  const totalWeightBytes = weights.reduce((sum, file) => sum + file.size, 0);
  const storageGb = (bytes: number) =>
    Math.max(0.01, Math.round((bytes / 1e9) * 100) / 100);
  const common = {
    modelId,
    provider: repo.split("/")[0],
    version: info.sha.slice(0, 7),
    source: `${HF_ENDPOINT}/${repo}`,
    repositoryRevision: info.sha,
    commercialStatus: commercialStatusForLicense(license),
    catch: `License: ${license ?? "not stated"}. Added from Hugging Face; not qualified by LocalBase.`,
    notes: `Added with local-base models add hf:${repo}:${basename(first.path)} at revision ${info.sha}${info.pipeline_tag ? ` (pipeline ${info.pipeline_tag})` : ""}.`,
  };
  const primary = {
    sourcePath: first.path,
    filename: filenameFor(first, false),
    expectedSizeBytes: first.size,
    sha256: first.sha256,
    role: "primary",
  };
  const result = {
    modelId,
    kind,
    repo,
    revision: info.sha,
    license,
    pipelineTag: info.pipeline_tag ?? null,
    weights,
  };

  if (kind === "stt") {
    const variant = stem.replace(/^ggml[-_]?/i, "");
    const english = /(^|[-_.])en($|[-_.])/i.test(variant);
    return {
      ...result,
      projector: null,
      detected: [],
      contextLength: null,
      spec: {
        ...common,
        displayName: `Whisper ${variant}`,
        kind: "stt",
        family: "Whisper",
        size: WHISPER_SIZE.exec(variant)?.[1]?.toLowerCase() ?? "unknown",
        quant: quantOf(first.path) ?? "F16",
        storageGb: storageGb(first.size),
        artifacts: [primary],
        inputModalities: ["audio"],
        outputModalities: ["text"],
        capabilities: ["speech-to-text", ...(english ? [] : ["multilingual"])],
      },
    };
  }

  if (kind === "image") {
    const quant =
      quantOf(first.path) ??
      (/(^|[-_.])fp16($|[-_.])/i.test(stem)
        ? "F16"
        : /(^|[-_.])fp32($|[-_.])/i.test(stem)
          ? "F32"
          : "unknown");
    return {
      ...result,
      projector: null,
      detected: [],
      contextLength: null,
      spec: {
        ...common,
        displayName:
          available.choices.length > 1 ? `${repoName} (${stem})` : repoName,
        kind: "image",
        family: repoName,
        size: `${(first.size / 1e9).toFixed(1)}GB`,
        quant,
        storageGb: storageGb(first.size),
        artifacts: [primary],
        inputModalities: ["text"],
        outputModalities: ["image"],
        // Off until LocalBase has measured the checkpoint; flash attention
        // is not safe for every architecture sd-server loads with -m.
        imageLaunchOptions: { diffusionFlashAttention: false },
        capabilities: ["text-to-image", "image-to-image"],
      },
    };
  }

  const likely = likelyKind(info.pipeline_tag, info.tags, {
    repo,
    library: info.library_name ?? null,
  });
  const projector = selectProjector(files);
  const projectorFilename = projector
    ? (() => {
        const name = basename(projector.path);
        // Projectors share generic names across repositories.
        return name.toLowerCase().includes(modelId)
          ? name
          : `${modelId}-${name}`;
      })()
    : null;
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
  const totalBytes = totalWeightBytes + (vision ? projector!.size : 0);
  const sharded = weights.length > 1;
  const spec: Record<string, unknown> = {
    ...common,
    displayName: `${family} ${quant}`,
    kind: "llm",
    family,
    size:
      sizeOf(repoName) !== "unknown" ? sizeOf(repoName) : sizeOf(first.path),
    quant,
    storageGb: storageGb(totalBytes),
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
  };
  return {
    ...result,
    spec,
    projector: vision ? projector : null,
    detected,
    contextLength,
  };
}
