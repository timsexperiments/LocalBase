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

/** Why the Hub refused, when a token would plausibly help. */
export type HubErrorCode = "hub_auth_required" | "hub_rate_limited";

export class HubError extends Error {
  override name = "HubError";
  constructor(
    message: string,
    readonly code?: HubErrorCode,
  ) {
    super(message);
  }
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
    /**
     * Set by LocalBase, not the Hub: the `prediction_type` a diffusers
     * repository declares in `scheduler/scheduler_config.json`.
     */
    schedulerPrediction: z.enum(["eps", "v"]).optional(),
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
    sha: z.string().optional(),
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
    const auth = response.status === 401 || response.status === 403;
    throw new HubError(
      response.status === 404
        ? `Hugging Face has no public repository for that name (${what}).`
        : auth
          ? `Hugging Face refused access (${response.status}) while trying to ${what}. The repository may be gated: accept its terms on huggingface.co and save a token with \`local-base configure --hf-token\`.`
          : response.status === 429
            ? `Hugging Face answered 429 (rate limited) while trying to ${what}. Wait a minute, or save a Hugging Face token for a higher limit; nothing was changed.`
            : `Hugging Face answered ${response.status} while trying to ${what}. Try again later; nothing was changed.`,
      auth
        ? "hub_auth_required"
        : response.status === 429
          ? "hub_rate_limited"
          : undefined,
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
  const info = parsed.data;
  const prediction = await schedulerPrediction(options, info);
  return prediction ? { ...info, schedulerPrediction: prediction } : info;
}

const SCHEDULER_CONFIG = "scheduler/scheduler_config.json";

/**
 * Reads the prediction type an image repository's diffusers scheduler
 * declares, so a single-file checkpoint shipped beside it launches with
 * the right parameterization. Best effort: a missing or unreadable
 * config leaves the name and tag checks to decide.
 */
async function schedulerPrediction(
  options: HubClientOptions,
  info: HubModelInfo,
): Promise<"eps" | "v" | null> {
  if (!info.siblings.some((file) => file.rfilename === SCHEDULER_CONFIG))
    return null;
  const likely = likelyKind(info.pipeline_tag, info.tags, {
    repo: info.id,
    library: info.library_name ?? null,
  });
  if (likely.kind !== "image" || likely.pipeline) return null;
  try {
    const { body } = await hubJson(
      options,
      `${HF_ENDPOINT}/${info.id}/resolve/${info.sha}/${SCHEDULER_CONFIG}`,
      `read the scheduler config of ${info.id}`,
    );
    const type = (body as { prediction_type?: unknown } | null)
      ?.prediction_type;
    return type === "v_prediction" ? "v" : type === "epsilon" ? "eps" : null;
  } catch {
    return null;
  }
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
  /**
   * For multi-file image pipelines: the LocalBase runtime profile whose
   * text encoders and VAE the entry reuses.
   */
  pipeline?: ImagePipelineId;
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
 * Multi-file image pipelines LocalBase can run: sd-server loads the
 * repository's diffusion model with the text encoders and VAE of the
 * matching built-in entry, pinned at that entry's revisions.
 */
export const IMAGE_PIPELINES = {
  flux1: {
    label: "FLUX.1",
    template: "flux1-schnell-q4_0",
    family: "FLUX.1",
    size: "12B",
  },
  "z-image-turbo": {
    label: "Z-Image-Turbo",
    template: "z-image-turbo-q4_0",
    family: "Z-Image",
    size: "6B",
  },
  "flux2-klein-4b": {
    label: "FLUX.2 klein 4B",
    template: "flux2-klein-4b-q4_0",
    family: "FLUX.2-klein",
    size: "4B",
  },
  "flux2-klein-9b": {
    label: "FLUX.2 klein 9B",
    template: "flux2-klein-9b-q4_0",
    family: "FLUX.2-klein",
    size: "9B",
  },
} as const;
export type ImagePipelineId = keyof typeof IMAGE_PIPELINES;

type PipelineMatch =
  Readonly<{ pipeline: ImagePipelineId }> | Readonly<{ unsupported: string }>;

/**
 * Image architectures sd-server needs separately mapped files for, in
 * match order: the families LocalBase has a runtime profile for, then the
 * ones it cannot launch yet with the reason.
 */
const PIPELINE_RULES: readonly (readonly [RegExp, PipelineMatch])[] = [
  [
    /flux[-_.]?1[^ ]*?(kontext|fill|redux|canny|depth)/i,
    {
      unsupported:
        "FLUX.1 editing pipeline (Kontext, Fill, Redux, Canny and Depth need image conditioning LocalBase has no profile for)",
    },
  ],
  [/flux[-_.]?1/i, { pipeline: "flux1" }],
  [
    /flux[-_.]?2[-_.]?klein[^ ]*?base/i,
    {
      unsupported:
        "FLUX.2 klein base (undistilled; LocalBase profiles the distilled four-step klein models)",
    },
  ],
  [/flux[-_.]?2[-_.]?klein[-_.]?4b/i, { pipeline: "flux2-klein-4b" }],
  [/flux[-_.]?2[-_.]?klein[-_.]?9b/i, { pipeline: "flux2-klein-9b" }],
  [
    /flux[-_.]?2/i,
    {
      unsupported:
        "FLUX.2 dev (needs a Mistral text encoder LocalBase has no runtime profile for)",
    },
  ],
  [/z[-_]?image[-_]?turbo/i, { pipeline: "z-image-turbo" }],
  [
    /z[-_]?image/i,
    {
      unsupported:
        "Z-Image base or edit model (LocalBase profiles Z-Image-Turbo only)",
    },
  ],
  [
    /(^|[^a-z])wan[-_]?\d|video|ltx|mochi|cogvideo|hunyuan[-_]?video/i,
    {
      unsupported:
        "video model (LocalBase adds video models from its measured catalog only)",
    },
  ],
  [
    /sd-?3|stable-diffusion-3/i,
    {
      unsupported:
        "Stable Diffusion 3 (needs CLIP-G, CLIP-L and T5 encoders; LocalBase has no SD3 runtime profile yet)",
    },
  ],
  [
    /qwen[-_]?image/i,
    {
      unsupported:
        "Qwen-Image (needs a Qwen2.5-VL encoder; LocalBase has no Qwen-Image runtime profile yet)",
    },
  ],
  [
    /hunyuan|lumina|kolors|pixart|sana|hidream|cogview|auraflow|chroma|omnigen|minimax|ovis/i,
    {
      unsupported:
        "multi-file image pipeline LocalBase has no runtime profile for",
    },
  ],
];

/** The repository name and the base models its tags name. */
function lineage(repo: string, tags: readonly string[]): string {
  return [
    repo,
    ...tags.flatMap((tag) => {
      const match = /^base_model:(?:[a-z]+:)?(.+)$/i.exec(tag);
      return match ? [match[1]!] : [];
    }),
  ].join(" ");
}

/** The multi-file pipeline a repository's name or base model belongs to. */
export function imagePipelineFor(
  repo: string,
  tags: readonly string[],
): PipelineMatch | null {
  // The repository name wins over its base model: a FLUX.1 Kontext
  // finetune of FLUX.1-dev is still an editing model.
  for (const text of [repo, lineage(repo, tags)]) {
    for (const [pattern, match] of PIPELINE_RULES) {
      if (pattern.test(text)) return match;
    }
  }
  return null;
}

export type KindContext = Readonly<{
  repo?: string;
  library?: string | null;
}>;

/**
 * Hub tags that mark a System One decision model (Cloudflare Clef and its
 * GGUF conversions). llama.cpp b11671 serves them as `clef` architecture.
 */
const DECISION_TAGS = new Set(["decision-model", "systemone"]);
const DECISION_ARCHITECTURES = new Set(["clef"]);

/** True when Hub tags or GGUF metadata mark a decision model. */
export function isDecisionRepository(
  tags: readonly string[],
  ggufArchitecture?: string | null,
): boolean {
  return (
    tags.some((tag) => DECISION_TAGS.has(tag.toLowerCase())) ||
    (ggufArchitecture != null && DECISION_ARCHITECTURES.has(ggufArchitecture))
  );
}

/** Classifies a repository from its pipeline tag, tags and library. */
export function likelyKind(
  pipelineTag: string | undefined,
  tags: readonly string[],
  context: KindContext = {},
): LikelyKind {
  const repo = context.repo ?? "";
  const library = context.library?.toLowerCase() ?? null;
  if (
    isDecisionRepository(tags) &&
    (!pipelineTag ||
      VISION_PIPELINES.has(pipelineTag) ||
      TEXT_PIPELINES.has(pipelineTag))
  )
    return { kind: "llm", label: "decision" };
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
  const imagePipelineTag =
    pipelineTag === "text-to-image" || pipelineTag === "image-to-image";
  const pipeline =
    imagePipelineTag || !pipelineTag ? imagePipelineFor(repo, tags) : null;
  // Untagged repositories count as images only when the library or a
  // known pipeline family says so; untagged GGUF is usually a language
  // model.
  const imageLike =
    pipelineTag === "text-to-image" ||
    (pipelineTag === "image-to-image" && pipeline !== null) ||
    (!pipelineTag &&
      (library === "diffusion-single-file" ||
        (pipeline !== null &&
          (library === null ||
            library === "gguf" ||
            library === "diffusers" ||
            library === "ggml"))));
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
    if (pipeline && "unsupported" in pipeline)
      return { kind: null, label: `image: ${pipeline.unsupported}` };
    if (pipeline)
      return {
        kind: "image",
        label: `image (${IMAGE_PIPELINES[pipeline.pipeline].label} pipeline)`,
        pipeline: pipeline.pipeline,
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
  /** The current commit sha, when the Hub returned one with the search. */
  sha?: string;
  likely: LikelyKind;
}>;

const SEARCH_FIELDS = [
  "downloads",
  "likes",
  "pipeline_tag",
  "tags",
  "gated",
  "library_name",
  "sha",
] as const;

/**
 * Searches the Hub, most downloaded first. Language models are limited to
 * GGUF repositories (the only files llama.cpp runs), speech to recognition
 * repositories and images to text-to-image ones, plus image-to-image and
 * GGUF repositories of a diffusion pipeline LocalBase can launch (FLUX.2
 * klein GGUF releases are tagged image-to-image or not tagged at all).
 */
export async function searchHub(
  options: HubClientOptions & Readonly<{ limit?: number; kind?: AddableKind }>,
  query: string,
): Promise<Readonly<{ hits: HubSearchHit[]; cached: boolean }>> {
  const kind = options.kind ?? "llm";
  const limit = options.limit ?? 20;
  const words = query.split(/\s+/).filter(Boolean);
  const text = (
    kind === "llm"
      ? words.filter((word) => word.toLowerCase() !== "gguf")
      : words
  ).join(" ");
  const search = text || (kind === "stt" ? "whisper" : "");
  const filters: Record<string, string>[] =
    kind === "llm"
      ? [{ filter: "gguf" }]
      : kind === "stt"
        ? [{ pipeline_tag: "automatic-speech-recognition" }]
        : [
            { pipeline_tag: "text-to-image" },
            { pipeline_tag: "image-to-image" },
            ...(search ? [{ filter: "gguf" }] : []),
          ];
  const pages = await Promise.all(
    filters.map((filter, index) =>
      searchPage(options, { ...filter, limit, search }, index > 0),
    ),
  );
  const seen = new Set<string>();
  const hits = pages
    .flatMap(({ hits: page }, index) =>
      // Extra image searches only add repositories of a launchable pipeline.
      index === 0 ? page : page.filter((hit) => hit.likely.pipeline),
    )
    .filter((hit) => !seen.has(hit.repo) && seen.add(hit.repo));
  if (pages.length > 1) hits.sort((a, b) => b.downloads - a.downloads);
  return {
    cached: pages.every(({ cached }) => cached),
    hits: hits.slice(0, limit),
  };
}

async function searchPage(
  options: HubClientOptions,
  query: Readonly<{
    limit: number;
    search: string;
    filter?: string;
    pipeline_tag?: string;
  }>,
  optional: boolean,
): Promise<Readonly<{ hits: HubSearchHit[]; cached: boolean }>> {
  const params = new URLSearchParams({
    sort: "downloads",
    direction: "-1",
    limit: String(query.limit),
  });
  if (query.filter) params.set("filter", query.filter);
  if (query.pipeline_tag) params.set("pipeline_tag", query.pipeline_tag);
  if (query.search) params.set("search", query.search);
  for (const field of SEARCH_FIELDS) params.append("expand[]", field);
  const url = `${HF_ENDPOINT}/api/models?${params}`;
  let response: Awaited<ReturnType<typeof hubJson>>;
  try {
    response = await hubJson(options, url, "search models");
  } catch (error) {
    if (optional) return { hits: [], cached: true };
    throw error;
  }
  const parsed = z.array(searchHitSchema).safeParse(response.body);
  if (!parsed.success) {
    if (optional) return { hits: [], cached: true };
    throw new HubError(
      "Hugging Face returned search results LocalBase does not understand.",
    );
  }
  return {
    cached: response.cached,
    hits: parsed.data.map((hit) => ({
      repo: hit.id,
      downloads: hit.downloads,
      ...(hit.likes === undefined ? {} : { likes: hit.likes }),
      license: hubLicense({ tags: hit.tags }),
      pipelineTag: hit.pipeline_tag ?? null,
      libraryName: hit.library_name ?? null,
      gated: isGated(hit.gated),
      ...(hit.sha && /^[a-fA-F0-9]{40}$/.test(hit.sha)
        ? { sha: hit.sha.toLowerCase() }
        : {}),
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

/**
 * Diffusion-model files of a multi-file pipeline: GGUF or safetensors at
 * the root or in a ComfyUI `diffusion_models` folder. Encoders and VAEs
 * come from the LocalBase profile, and the FP4, INT8 and scaled-FP8
 * formats some repositories ship are ones sd.cpp cannot read.
 */
function pipelineChoices(files: readonly HubFile[]): HubFile[] {
  return files.filter((file) => {
    const name = basename(file.path);
    return (
      (!file.path.includes("/") ||
        /^(split_files\/)?diffusion_models\/[^/]+$/.test(file.path)) &&
      /\.(safetensors|gguf)$/i.test(name) &&
      file.size >= MIN_IMAGE_CHECKPOINT_BYTES &&
      !/vae|(^|[-_.])ae[-_.]|lora|text[-_]?encoder|clip|t5|qwen|mmproj|nvfp4|mxfp4|fp4|int8|int4|convrot|scaled|svdq|nunchaku/i.test(
        name,
      )
    );
  });
}

/** Quantizations the measured pipeline profiles use, smallest first. */
const PREFERRED_PIPELINE_QUANTS = ["Q4_0", "Q4_K", "Q4_K_S", "Q5_0", "Q8_0"];

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
    "has no single-file Stable Diffusion checkpoint (.safetensors, .ckpt or .gguf at the repository root). LoRAs and multi-file pipelines outside FLUX.1, FLUX.2 klein and Z-Image-Turbo are not supported by models add.",
};

const NO_DIFFUSERS_FILES =
  "is a diffusers folder layout (model_index.json with separate unet, text encoder and VAE folders), which sd.cpp cannot load. Look for a single-file .safetensors or GGUF release of the same model.";

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
  /** The multi-file image pipeline the files belong to, if any. */
  pipeline: ImagePipelineId | null;
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
      `${info.id} looks like ${likely.label}. models add supports GGUF language models, whisper.cpp speech models, single-file Stable Diffusion checkpoints and FLUX.1, FLUX.2 klein and Z-Image-Turbo diffusion models.`,
    );
  }
  const files = lfsFiles(info);
  const pipeline = likely.pipeline ?? null;
  const choices =
    likely.kind === "llm"
      ? llmChoices(files)
      : likely.kind === "stt"
        ? sttChoices(files)
        : pipeline
          ? pipelineChoices(files)
          : imageChoices(files);
  if (choices.length === 0) {
    const diffusers =
      likely.kind === "image" &&
      info.siblings.some((file) => file.rfilename === "model_index.json");
    throw new HubError(
      `${info.id} ${
        pipeline
          ? `has no ${IMAGE_PIPELINES[pipeline].label} diffusion model file sd.cpp can load (GGUF or BF16/FP16/FP8 safetensors at the root or in diffusion_models/).`
          : diffusers
            ? NO_DIFFUSERS_FILES
            : NO_FILES[likely.kind]
      }`,
    );
  }
  return {
    kind: likely.kind,
    choices,
    default: pipeline
      ? pipelineDefault(choices)
      : defaultChoice(likely.kind, choices),
    pipeline,
  };
}

/** A pipeline's default: the measured GGUF quant, else the smallest file. */
function pipelineDefault(choices: readonly HubFile[]): HubFile | null {
  const sorted = [...choices].sort(bySize);
  for (const quant of PREFERRED_PIPELINE_QUANTS) {
    const match = sorted.find((file) => quantOf(file.path) === quant);
    if (match) return match;
  }
  return sorted[0] ?? null;
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

/**
 * Filenames other catalog models already install, with their checksums, to
 * avoid collisions. A file with the same name and checksum is the same bytes
 * and is shared instead of downloaded twice.
 */
function takenFilenames(modelId: string): Map<string, string> {
  return new Map(
    CATALOG.filter((model) => model.modelId !== modelId).flatMap((model) =>
      model.artifacts.map(
        ({ filename, sha256 }) => [filename, sha256 ?? ""] as const,
      ),
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
    const owner = taken.get(name);
    return owner !== undefined && owner !== file.sha256 && !sharded
      ? `${modelId}-${name}`
      : name;
  };
  for (const file of weights) {
    const owner = taken.get(basename(file.path));
    if (SHARD.test(file.path) && owner !== undefined && owner !== file.sha256) {
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

  if (kind === "image" && available.pipeline) {
    return {
      ...result,
      ...pipelineEntry(available.pipeline, {
        common,
        primary,
        first,
        repoName,
        stem,
        storageGb,
      }),
    };
  }

  if (kind === "image") {
    const quant = imageQuant(first.path, stem);
    const prediction = vPrediction(info, stem);
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
        imageLaunchOptions: {
          diffusionFlashAttention: false,
          ...(prediction ? { prediction: "v" } : {}),
        },
        capabilities: ["text-to-image", "image-to-image"],
        ...(prediction
          ? {
              notes: `${common.notes} Launches with --prediction v because ${prediction}.`,
            }
          : {}),
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
  const decision = isDecisionRepository(
    info.tags,
    info.gguf?.architecture ?? null,
  );
  const vision =
    projector !== null &&
    (likely.label === "llm (vision)" ||
      (decision && info.pipeline_tag === "image-text-to-text") ||
      info.tags.some(
        (tag) =>
          tag === "vision" ||
          tag === "multimodal" ||
          tag === "image-text-to-text",
      ));
  const coder = /coder|code/i.test(repo) || info.tags.includes("code");
  const template = info.gguf?.chat_template;
  // A decision model's template drives typed questions, not chat.
  const opinions = template && !decision ? analyzeChatTemplate(template) : {};
  const detected: Capability[] = decision
    ? ["decision"]
    : (Object.entries(opinions) as [Capability, boolean][])
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
    ...(decision
      ? {
          llmRuntime: { capability: "decision-only" },
          capabilities: ["decision"],
        }
      : {
          capabilities: [
            "text-generation",
            ...(coder ? ["code-generation"] : []),
          ],
        }),
  };
  return {
    ...result,
    spec,
    projector: vision ? projector : null,
    detected,
    contextLength,
  };
}

const whoamiSchema = z.object({
  name: z.string().max(200).optional(),
  auth: z
    .object({
      accessToken: z
        .object({ role: z.string().max(40).optional() })
        .partial()
        .optional(),
    })
    .partial()
    .optional(),
});

export type HubTokenCheck =
  | Readonly<{ state: "valid"; name: string | null; role: string | null }>
  | Readonly<{ state: "invalid" }>
  | Readonly<{ state: "unreachable"; message: string }>;

/**
 * Asks the Hub who a token belongs to. Never cached, and the token goes
 * only to huggingface.co; the result carries no part of the token.
 */
export async function checkHubToken(
  token: string,
  fetcher: HubFetch = fetch as unknown as HubFetch,
): Promise<HubTokenCheck> {
  let response: Awaited<ReturnType<HubFetch>>;
  try {
    response = await fetcher(`${HF_ENDPOINT}/api/whoami-v2`, {
      headers: { accept: "application/json", authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch {
    return {
      state: "unreachable",
      message: "Could not reach Hugging Face to check the token.",
    };
  }
  if (response.status === 401 || response.status === 403)
    return { state: "invalid" };
  if (!response.ok)
    return {
      state: "unreachable",
      message: `Hugging Face answered ${response.status} while checking the token.`,
    };
  const parsed = whoamiSchema.safeParse(
    await response.json().catch(() => null),
  );
  if (!parsed.success)
    return {
      state: "unreachable",
      message:
        "Hugging Face sent an unexpected answer while checking the token.",
    };
  return {
    state: "valid",
    name: parsed.data.name ?? null,
    role: parsed.data.auth?.accessToken?.role ?? null,
  };
}

function imageQuant(path: string, stem: string): string {
  return (
    quantOf(path) ??
    (/(^|[-_.])bf16($|[-_.])/i.test(stem)
      ? "BF16"
      : /(^|[-_.])fp16($|[-_.])/i.test(stem)
        ? "F16"
        : /(^|[-_.])fp8/i.test(stem)
          ? "FP8"
          : /(^|[-_.])fp32($|[-_.])/i.test(stem)
            ? "F32"
            : "unknown")
  );
}

const V_PRED_NAME = /v[-_]?pred|768[-_]v(?![a-z0-9])|v2[-_.]?1[-_]768/i;

/**
 * Why a single-file checkpoint needs `--prediction v`, or null for an
 * ordinary epsilon model. Names and tags win over the scheduler config:
 * several v-pred SDXL fine-tunes ship a stale epsilon scheduler_config.json.
 */
export function vPrediction(info: HubModelInfo, stem = ""): string | null {
  if (V_PRED_NAME.test(info.id) || V_PRED_NAME.test(stem)) {
    return "the name marks a v-prediction checkpoint";
  }
  if (info.tags.some((tag) => /^v[-_]?pred(iction)?$/i.test(tag))) {
    return "the repository is tagged v-prediction";
  }
  if (info.schedulerPrediction === "v") {
    return "scheduler_config.json sets prediction_type v_prediction";
  }
  return null;
}

/**
 * A catalog entry for a FLUX.1, FLUX.2 klein or Z-Image-Turbo diffusion model
 * file: the chosen file replaces the measured built-in's diffusion model and
 * the built-in's pinned VAE and text encoders come along unchanged.
 */
function pipelineEntry(
  pipeline: ImagePipelineId,
  input: Readonly<{
    common: Readonly<{ notes: string }> & Record<string, unknown>;
    primary: Readonly<{ filename: string }> & Record<string, unknown>;
    first: HubFile;
    repoName: string;
    stem: string;
    storageGb: (bytes: number) => number;
  }>,
): Pick<GeneratedEntry, "spec" | "projector" | "detected" | "contextLength"> {
  const profile = IMAGE_PIPELINES[pipeline];
  const template = CATALOG.find(({ modelId }) => modelId === profile.template);
  const runtime = template?.imageRuntime;
  if (!template || !runtime || runtime.kind === "checkpoint") {
    throw new HubError(
      `The ${profile.label} pipeline profile (${profile.template}) is missing from this LocalBase build.`,
    );
  }
  const supplementary = template.artifacts
    .filter(({ role }) => role === "supplementary")
    .map((artifact) => ({
      ...artifact,
      source: artifact.source ?? {
        repositoryUrl: template.source,
        revision: template.repositoryRevision,
      },
    }));
  // FLUX.1 dev and other guidance-distilled FLUX.1 checkpoints need more
  // than the four steps Schnell is distilled for.
  const steps =
    pipeline === "flux1" && !/schnell/i.test(`${input.repoName} ${input.stem}`)
      ? 20
      : runtime.generation.steps;
  const totalBytes =
    input.first.size +
    supplementary.reduce((sum, file) => sum + (file.expectedSizeBytes ?? 0), 0);
  return {
    projector: null,
    detected: [],
    contextLength: null,
    spec: {
      ...input.common,
      displayName: `${input.repoName} (${input.stem})`,
      kind: "image",
      family: profile.family,
      size: profile.size,
      quant: imageQuant(input.first.path, input.stem),
      storageGb: input.storageGb(totalBytes),
      artifacts: [input.primary, ...supplementary],
      imageRuntime: {
        ...runtime,
        artifacts: {
          ...runtime.artifacts,
          diffusionModel: input.primary.filename,
        },
        generation: { ...runtime.generation, steps },
      },
      inputModalities: template.inputModalities,
      outputModalities: template.outputModalities,
      ...(template.imageLaunchOptions
        ? { imageLaunchOptions: template.imageLaunchOptions }
        : {}),
      capabilities: template.capabilities.map(({ id }) => id),
      notes: `${input.common.notes} ${profile.label} pipeline: the VAE and text encoders are the ones pinned by the ${template.modelId} profile.`,
    },
  };
}
