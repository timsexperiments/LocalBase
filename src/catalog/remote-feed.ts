/**
 * The remote catalog feed (the `remote` tier in `./sources.ts`).
 *
 * The repository publishes its catalog data as `catalog/feed.json`. A
 * gateway downloads that feed on a schedule so new models arrive without a
 * binary release. The feed is checked as a whole (format, version and a
 * SHA-256 of its models) and every entry is then validated like any other
 * catalog entry, so pinned revisions and file checksums are required.
 * Entries that fail are skipped and reported; they never stop LocalBase.
 *
 * The last good download is kept under `<root>/cache/catalog-feed/` and is
 * what every process loads. A failed refresh keeps it. Without a cache the
 * built-in catalog is the offline fallback.
 *
 * Remote entries add models. They never replace a built-in (qualified)
 * entry, whose updates ship with releases, and the user tier sits above the
 * remote tier, so a feed can never override a user entry.
 */
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { z } from "zod";
import { BUILT_IN_CATALOG, CATALOG } from "../catalog";
import {
  catalogDiagnostics,
  catalogProvenance,
  composeCatalog,
  type CatalogLayer,
} from "./sources";

export const CATALOG_FEED_FORMAT = "localbase-catalog-feed";
export const CATALOG_FEED_SCHEMA_VERSION = 1;
export const DEFAULT_CATALOG_FEED_URL =
  "https://raw.githubusercontent.com/timsexperiments/LocalBase/main/catalog/feed.json";
/** A cached feed younger than this is not downloaded again. */
export const CATALOG_FEED_TTL_MS = 24 * 60 * 60 * 1000;
/** `LOCALBASE_CATALOG_FEED_URL=off` disables the feed; a URL replaces it. */
export const CATALOG_FEED_URL_ENV = "LOCALBASE_CATALOG_FEED_URL";

const MAX_FEED_BYTES = 8 * 1024 * 1024;
const MAX_FEED_MODELS = 5_000;
const REQUEST_TIMEOUT_MS = 20_000;

const sha256Schema = z.string().regex(/^[0-9a-f]{64}$/);

const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "[::1]"]);

/** Feeds are HTTPS; plain HTTP is accepted only on the loopback interface. */
export function catalogFeedUrlProblem(url: string): string | null {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return "is not a URL";
  }
  if (parsed.username || parsed.password) {
    return "must not contain credentials";
  }
  if (parsed.protocol === "https:") return null;
  if (parsed.protocol === "http:" && LOOPBACK_HOSTS.has(parsed.hostname)) {
    return null;
  }
  return "must use https:// (plain http:// is allowed only for localhost)";
}

export const catalogFeedUrlSchema = z
  .string()
  .min(1)
  .superRefine((url, context) => {
    const problem = catalogFeedUrlProblem(url);
    if (problem) context.addIssue({ code: "custom", message: problem });
  });

/** The published feed document. */
export const catalogFeedSchema = z
  .object({
    format: z.literal(CATALOG_FEED_FORMAT),
    schemaVersion: z.number().int().positive(),
    /** SHA-256 of `JSON.stringify(models)`; also the feed's revision. */
    sha256: sha256Schema,
    models: z.array(z.unknown()).max(MAX_FEED_MODELS),
  })
  .strict();
export type CatalogFeed = z.infer<typeof catalogFeedSchema>;

/** Builds the feed document for a list of catalog entries. */
export function buildCatalogFeed(models: readonly unknown[]): CatalogFeed {
  return {
    format: CATALOG_FEED_FORMAT,
    schemaVersion: CATALOG_FEED_SCHEMA_VERSION,
    sha256: feedChecksum(models),
    models: [...models],
  };
}

function feedChecksum(models: readonly unknown[]): string {
  return createHash("sha256").update(JSON.stringify(models)).digest("hex");
}

// Settings ----------------------------------------------------------------

const settingsSchema = z
  .object({ url: catalogFeedUrlSchema.nullable() })
  .strict();

export type CatalogFeedSource = "default" | "settings" | "environment";
export type CatalogFeedSettings = Readonly<{
  /** Null when the feed is disabled. */
  url: string | null;
  source: CatalogFeedSource;
  /** Why a configured URL was ignored, if one was. */
  problem?: string;
}>;

/** `<root>/catalog-feed.json`: the feed URL chosen with `models feed use`. */
export function catalogFeedSettingsPath(root: string): string {
  return join(root, "catalog-feed.json");
}

/** The feed URL in force: the environment, then the root's settings, then the default. */
export function catalogFeedSettings(
  root: string,
  environment: Record<string, string | undefined> = process.env,
): CatalogFeedSettings {
  const fromEnvironment = environment[CATALOG_FEED_URL_ENV]?.trim();
  if (fromEnvironment) {
    if (fromEnvironment === "off") return { url: null, source: "environment" };
    const problem = catalogFeedUrlProblem(fromEnvironment);
    return problem
      ? {
          url: null,
          source: "environment",
          problem: `${CATALOG_FEED_URL_ENV} ${problem}; the feed is off.`,
        }
      : { url: fromEnvironment, source: "environment" };
  }
  const path = catalogFeedSettingsPath(root);
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch {
    return { url: DEFAULT_CATALOG_FEED_URL, source: "default" };
  }
  try {
    return {
      url: settingsSchema.parse(JSON.parse(text)).url,
      source: "settings",
    };
  } catch (error) {
    return {
      url: DEFAULT_CATALOG_FEED_URL,
      source: "default",
      problem: `${path} is invalid (${error instanceof z.ZodError ? error.issues[0]?.message : (error as Error).message}); using the default feed.`,
    };
  }
}

/**
 * Saves the feed URL for `root`: a URL, `null` to turn the feed off, or
 * `"default"` to go back to the published feed.
 */
export function writeCatalogFeedSettings(
  root: string,
  url: string | null | "default",
): void {
  const path = catalogFeedSettingsPath(root);
  if (url === "default") {
    rmSync(path, { force: true });
    return;
  }
  const settings = settingsSchema.parse({ url });
  const temporary = `${path}.${process.pid}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(settings, null, 2)}\n`, {
    mode: 0o600,
  });
  renameSync(temporary, path);
}

// Cache -------------------------------------------------------------------

const cachedFeedSchema = z
  .object({
    url: z.string(),
    fetchedAt: z.iso.datetime(),
    etag: z.string().nullable(),
    feed: catalogFeedSchema,
  })
  .strict();
type CachedFeed = z.infer<typeof cachedFeedSchema>;

const feedStateSchema = z
  .object({
    url: z.string(),
    checkedAt: z.iso.datetime(),
    lastError: z.string().nullable(),
  })
  .strict();
type FeedState = z.infer<typeof feedStateSchema>;

export function catalogFeedCacheDirectory(root: string): string {
  return join(root, "cache", "catalog-feed");
}

function readJson<T>(path: string, schema: z.ZodType<T>): T | null {
  try {
    return schema.parse(JSON.parse(readFileSync(path, "utf8")));
  } catch {
    return null;
  }
}

function readCachedFeed(root: string, url: string): CachedFeed | null {
  const cached = readJson(
    join(catalogFeedCacheDirectory(root), "feed.json"),
    cachedFeedSchema,
  );
  if (!cached || cached.url !== url) return null;
  // A cache written by a newer LocalBase is not readable here.
  if (cached.feed.schemaVersion !== CATALOG_FEED_SCHEMA_VERSION) return null;
  return cached;
}

function readFeedState(root: string, url: string): FeedState | null {
  const state = readJson(
    join(catalogFeedCacheDirectory(root), "state.json"),
    feedStateSchema,
  );
  return state && state.url === url ? state : null;
}

function writePrivateJson(path: string, value: unknown): void {
  const temporary = `${path}.${process.pid}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(value)}\n`, { mode: 0o600 });
  renameSync(temporary, path);
}

const BUILT_IN_IDS = new Set(BUILT_IN_CATALOG.map(({ modelId }) => modelId));

function entryModelId(data: unknown): string | undefined {
  if (typeof data !== "object" || data === null) return undefined;
  const modelId = (data as Record<string, unknown>).modelId;
  return typeof modelId === "string" ? modelId : undefined;
}

/** Turns a feed into a catalog layer, leaving out ids this binary ships. */
function feedLayer(url: string, feed: CatalogFeed): CatalogLayer {
  return {
    tier: "remote",
    entries: feed.models.flatMap((data, index) => {
      const modelId = entryModelId(data);
      return modelId && BUILT_IN_IDS.has(modelId)
        ? []
        : [{ path: `${url}#models[${index}]`, data }];
    }),
  };
}

/**
 * The remote layer from the last good download, or null when the feed is
 * off or nothing has been downloaded yet. Never touches the network.
 */
export function readRemoteCatalogLayer(
  root: string,
  environment: Record<string, string | undefined> = process.env,
): CatalogLayer | null {
  const { url } = catalogFeedSettings(root, environment);
  if (!url) return null;
  const cached = readCachedFeed(root, url);
  return cached ? feedLayer(url, cached.feed) : null;
}

// Refresh -----------------------------------------------------------------

export type CatalogFeedFetch = (
  url: string,
  init: {
    headers: Record<string, string>;
    signal: AbortSignal;
    redirect: "follow";
  },
) => Promise<Response>;

export type CatalogFeedRefreshOptions = Readonly<{
  /** Download even when the cached copy is still fresh. */
  force?: boolean;
  fetch?: CatalogFeedFetch;
  now?: () => number;
  environment?: Record<string, string | undefined>;
}>;

export type CatalogFeedRefreshOutcome =
  /** The feed is turned off. */
  | "disabled"
  /** The cached copy is younger than the TTL; nothing was downloaded. */
  | "fresh"
  /** The server's feed matches the cached copy. */
  | "unchanged"
  /** A new feed was downloaded and cached. */
  | "updated"
  /** The download failed or was rejected; the last good copy stays. */
  | "failed";

export type CatalogFeedRefreshResult = Readonly<{
  outcome: CatalogFeedRefreshOutcome;
  url: string | null;
  /** Why the refresh failed, when it did. */
  error: string | null;
}>;

export class CatalogFeedError extends Error {}

async function readLimitedText(response: Response): Promise<string> {
  const declared = Number(response.headers.get("content-length") ?? "0");
  if (declared > MAX_FEED_BYTES) {
    throw new CatalogFeedError("The feed is larger than 8 MiB.");
  }
  const reader = response.body?.getReader();
  if (!reader) return "";
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > MAX_FEED_BYTES) {
      await reader.cancel();
      throw new CatalogFeedError("The feed is larger than 8 MiB.");
    }
    chunks.push(value);
  }
  return Buffer.concat(chunks).toString("utf8");
}

/** Parses and checks a downloaded feed document as a whole. */
export function parseCatalogFeed(text: string): CatalogFeed {
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    throw new CatalogFeedError("The feed is not JSON.");
  }
  const header = z
    .object({
      format: z.literal(CATALOG_FEED_FORMAT),
      schemaVersion: z.number().int().positive(),
    })
    .safeParse(body);
  if (!header.success) {
    throw new CatalogFeedError(
      `The document is not a LocalBase catalog feed (expected "format": "${CATALOG_FEED_FORMAT}").`,
    );
  }
  if (header.data.schemaVersion !== CATALOG_FEED_SCHEMA_VERSION) {
    throw new CatalogFeedError(
      `The feed uses schema version ${header.data.schemaVersion}; this LocalBase reads version ${CATALOG_FEED_SCHEMA_VERSION}. Upgrade LocalBase to use it.`,
    );
  }
  const parsed = catalogFeedSchema.safeParse(body);
  if (!parsed.success) {
    const issue = parsed.error.issues[0];
    throw new CatalogFeedError(
      `The feed is malformed (${issue ? `${issue.path.join(".") || "document"}: ${issue.message}` : "invalid"}).`,
    );
  }
  if (feedChecksum(parsed.data.models) !== parsed.data.sha256) {
    throw new CatalogFeedError(
      "The feed's sha256 does not match its models; it was truncated or altered.",
    );
  }
  return parsed.data;
}

/**
 * Downloads the feed when the cached copy is older than the TTL (or
 * always, with `force`), validates it and replaces the cache. On any
 * failure the last good copy stays in place and the error is recorded.
 * Callers reload the catalog (`loadCatalogSources`) after `updated`.
 */
export async function refreshCatalogFeed(
  root: string,
  options: CatalogFeedRefreshOptions = {},
): Promise<CatalogFeedRefreshResult> {
  const { url } = catalogFeedSettings(root, options.environment);
  if (!url) return { outcome: "disabled", url: null, error: null };
  const now = options.now?.() ?? Date.now();
  const cached = readCachedFeed(root, url);
  const state = readFeedState(root, url);
  const lastCheck = Date.parse(state?.checkedAt ?? cached?.fetchedAt ?? "");
  if (!options.force && cached && now - lastCheck < CATALOG_FEED_TTL_MS) {
    return { outcome: "fresh", url, error: null };
  }
  // Never create files in a directory that is not a LocalBase root yet.
  const writable = existsSync(join(root, ".localbase-root.json"));
  const directory = catalogFeedCacheDirectory(root);
  const record = (lastError: string | null) => {
    if (!writable) return;
    try {
      mkdirSync(directory, { recursive: true, mode: 0o700 });
      writePrivateJson(join(directory, "state.json"), {
        url,
        checkedAt: new Date(now).toISOString(),
        lastError,
      } satisfies FeedState);
    } catch {
      // State is advisory; a read-only root still refreshes in memory.
    }
  };
  const fail = (error: string): CatalogFeedRefreshResult => {
    record(error);
    return { outcome: "failed", url, error };
  };
  const fetcher = options.fetch ?? (fetch as unknown as CatalogFeedFetch);
  let response: Response;
  try {
    response = await fetcher(url, {
      headers: {
        accept: "application/json",
        ...(cached?.etag ? { "if-none-match": cached.etag } : {}),
      },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      redirect: "follow",
    });
  } catch (error) {
    return fail(`Could not reach the feed (${(error as Error).message}).`);
  }
  if (response.url && response.url !== url) {
    const problem = catalogFeedUrlProblem(response.url);
    if (problem) return fail(`The feed redirected to a URL that ${problem}.`);
  }
  if (response.status === 304 && cached) {
    record(null);
    return { outcome: "unchanged", url, error: null };
  }
  if (!response.ok) {
    return fail(`The feed server answered ${response.status}.`);
  }
  let feed: CatalogFeed;
  try {
    feed = parseCatalogFeed(await readLimitedText(response));
  } catch (error) {
    return fail(
      error instanceof CatalogFeedError
        ? error.message
        : `The feed download failed (${(error as Error).message}).`,
    );
  }
  const unchanged = cached?.feed.sha256 === feed.sha256;
  if (writable) {
    try {
      mkdirSync(directory, { recursive: true, mode: 0o700 });
      writePrivateJson(join(directory, "feed.json"), {
        url,
        fetchedAt: new Date(now).toISOString(),
        etag: response.headers.get("etag"),
        feed,
      } satisfies CachedFeed);
    } catch (error) {
      return fail(
        `The feed could not be cached (${(error as Error).message}).`,
      );
    }
  }
  record(null);
  return { outcome: unchanged ? "unchanged" : "updated", url, error: null };
}

// Status ------------------------------------------------------------------

export type CatalogFeedStatus = Readonly<{
  enabled: boolean;
  url: string | null;
  source: CatalogFeedSource;
  /** A configured URL that was ignored, and why. */
  problem: string | null;
  /** When the cached copy was downloaded; null without a cache. */
  fetchedAt: string | null;
  /** When the server was last asked. */
  checkedAt: string | null;
  /** The cached feed's sha256. */
  revision: string | null;
  /** Older than the TTL, or never downloaded. */
  stale: boolean;
  lastError: string | null;
  /** Entries in the cached feed. */
  entries: number;
  /** Entries left out because this binary ships the same model id. */
  builtIn: number;
  /** Models the feed adds to the effective catalog. */
  added: readonly string[];
  /** Feed entries skipped as invalid. */
  skipped: number;
}>;

/**
 * Describes the feed for `root`. The counts read the effective catalog, so
 * load the catalog for the same root first.
 */
export function catalogFeedStatus(
  root: string,
  options: Readonly<{
    now?: number;
    environment?: Record<string, string | undefined>;
  }> = {},
): CatalogFeedStatus {
  const settings = catalogFeedSettings(root, options.environment);
  const base = {
    url: settings.url,
    source: settings.source,
    problem: settings.problem ?? null,
  };
  if (!settings.url) {
    return {
      ...base,
      enabled: false,
      fetchedAt: null,
      checkedAt: null,
      revision: null,
      stale: false,
      lastError: null,
      entries: 0,
      builtIn: 0,
      added: [],
      skipped: 0,
    };
  }
  const cached = readCachedFeed(root, settings.url);
  const state = readFeedState(root, settings.url);
  const now = options.now ?? Date.now();
  const lastCheck = Date.parse(state?.checkedAt ?? cached?.fetchedAt ?? "");
  const builtIn = cached
    ? cached.feed.models.filter((data) => {
        const modelId = entryModelId(data);
        return modelId !== undefined && BUILT_IN_IDS.has(modelId);
      }).length
    : 0;
  return {
    ...base,
    enabled: true,
    fetchedAt: cached?.fetchedAt ?? null,
    checkedAt: state?.checkedAt ?? cached?.fetchedAt ?? null,
    revision: cached?.feed.sha256 ?? null,
    stale: !cached || !(now - lastCheck < CATALOG_FEED_TTL_MS),
    lastError: state?.lastError ?? null,
    entries: cached?.feed.models.length ?? 0,
    builtIn,
    added: CATALOG.filter(
      ({ modelId }) => catalogProvenance(modelId).origin === "remote",
    ).map(({ modelId }) => modelId),
    skipped: catalogDiagnostics().filter(({ tier }) => tier === "remote")
      .length,
  };
}

/** Validates a feed's entries without applying them (for refresh output). */
export function previewCatalogFeed(url: string, feed: CatalogFeed) {
  return composeCatalog(BUILT_IN_CATALOG, [feedLayer(url, feed)]);
}
