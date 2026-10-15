import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  test,
} from "bun:test";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
  mkdirSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BUILT_IN_CATALOG, CATALOG } from "../catalog";
import type { AppContext } from "../context";
import type { CommandExecution } from "../domains/app/commands/framework";
import {
  runFeedRefresh,
  runFeedStatus,
  runFeedUse,
} from "../domains/models/commands/feed";
import {
  CATALOG_FEED_PATH,
  renderCatalogFeed,
} from "../../scripts/build-catalog-feed";
import { startCatalogFeedScheduler } from "./feed-scheduler";
import {
  buildCatalogFeed,
  CATALOG_FEED_FORMAT,
  CATALOG_FEED_TTL_MS,
  CATALOG_FEED_URL_ENV,
  catalogFeedCacheDirectory,
  catalogFeedSettings,
  catalogFeedStatus,
  catalogFeedUrlProblem,
  DEFAULT_CATALOG_FEED_URL,
  parseCatalogFeed,
  readRemoteCatalogLayer,
  refreshCatalogFeed,
  writeCatalogFeedSettings,
} from "./remote-feed";
import {
  applyCatalogLayers,
  catalogDiagnostics,
  catalogProvenance,
  composeCatalog,
} from "./sources";
import { loadCatalogSources, userCatalogDirectory } from "./user-catalog";

const PORT = 24000 + Math.floor(Math.random() * 5000);
const BASE = `http://127.0.0.1:${PORT}`;
const FEED_URL = `${BASE}/feed.json`;

type Reply = { status?: number; body?: string; etag?: string };
let reply: Reply = {};
let requests: Request[] = [];
let server: ReturnType<typeof Bun.serve>;

beforeAll(() => {
  server = Bun.serve({
    port: PORT,
    hostname: "127.0.0.1",
    fetch(request) {
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === "/redirect") {
        return new Response(null, {
          status: 302,
          headers: { location: "http://example.com/feed.json" },
        });
      }
      if (reply.etag && request.headers.get("if-none-match") === reply.etag) {
        return new Response(null, { status: 304 });
      }
      return new Response(reply.body ?? "", {
        status: reply.status ?? 200,
        headers: reply.etag ? { etag: reply.etag } : {},
      });
    },
  });
});

afterAll(() => {
  server.stop(true);
});

const roots: string[] = [];
const previousEnv = process.env[CATALOG_FEED_URL_ENV];

function scratchRoot(initialized = true): string {
  const root = realpathSync(mkdtempSync(join(tmpdir(), "lb-feed-")));
  if (initialized) writeFileSync(join(root, ".localbase-root.json"), "{}");
  roots.push(root);
  return root;
}

afterEach(() => {
  reply = {};
  requests = [];
  process.env[CATALOG_FEED_URL_ENV] = previousEnv;
  applyCatalogLayers([]);
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

const env = { [CATALOG_FEED_URL_ENV]: FEED_URL };

function communityModel(modelId: string): Record<string, unknown> {
  const base = BUILT_IN_CATALOG.find(({ kind }) => kind === "llm")!;
  return {
    ...structuredClone(base),
    modelId,
    displayName: `Community ${modelId}`,
  };
}

function serveFeed(models: unknown[], etag?: string) {
  reply = { body: JSON.stringify(buildCatalogFeed(models)), etag };
}

function load(root: string) {
  return loadCatalogSources(root, {
    remote: readRemoteCatalogLayer(root, env),
  });
}

const execution = (lines: string[]): CommandExecution => ({
  global: { nonInteractive: true, json: false },
  output: {
    info: (message) => lines.push(message),
    error: (message) => lines.push(message),
    lifecycle() {},
  },
});

const context = (root: string) =>
  ({ config: { root } }) as unknown as Pick<AppContext, "config">;

describe("feed URLs", () => {
  test("require https except on loopback and reject credentials", () => {
    expect(catalogFeedUrlProblem("https://example.com/feed.json")).toBeNull();
    expect(catalogFeedUrlProblem(FEED_URL)).toBeNull();
    expect(catalogFeedUrlProblem("http://localhost:1/x")).toBeNull();
    expect(
      catalogFeedUrlProblem("http://example.com/feed.json"),
    ).not.toBeNull();
    expect(catalogFeedUrlProblem("https://a:b@example.com/x")).not.toBeNull();
    expect(catalogFeedUrlProblem("ftp://example.com/x")).not.toBeNull();
    expect(catalogFeedUrlProblem("not a url")).not.toBeNull();
  });

  test("resolve environment, then settings, then the default", () => {
    const root = scratchRoot();
    expect(catalogFeedSettings(root, {})).toEqual({
      url: DEFAULT_CATALOG_FEED_URL,
      source: "default",
    });
    writeCatalogFeedSettings(root, "https://example.com/mine.json");
    expect(catalogFeedSettings(root, {})).toEqual({
      url: "https://example.com/mine.json",
      source: "settings",
    });
    writeCatalogFeedSettings(root, null);
    expect(catalogFeedSettings(root, {}).url).toBeNull();
    expect(catalogFeedSettings(root, env).url).toBe(FEED_URL);
    expect(
      catalogFeedSettings(root, { [CATALOG_FEED_URL_ENV]: "off" }).url,
    ).toBeNull();
    const insecure = catalogFeedSettings(root, {
      [CATALOG_FEED_URL_ENV]: "http://example.com/feed.json",
    });
    expect(insecure.url).toBeNull();
    expect(insecure.problem).toContain(CATALOG_FEED_URL_ENV);
    writeCatalogFeedSettings(root, "default");
    expect(catalogFeedSettings(root, {}).source).toBe("default");
  });
});

describe("parseCatalogFeed", () => {
  test("rejects a checksum mismatch", () => {
    const feed = buildCatalogFeed([communityModel("community-a")]);
    const altered = { ...feed, models: [communityModel("community-b")] };
    expect(() => parseCatalogFeed(JSON.stringify(altered))).toThrow(
      /does not match its models/,
    );
  });

  test("rejects a newer schema version and other documents", () => {
    const feed = { ...buildCatalogFeed([]), schemaVersion: 2 };
    expect(() => parseCatalogFeed(JSON.stringify(feed))).toThrow(
      /Upgrade LocalBase/,
    );
    expect(() => parseCatalogFeed("{")).toThrow(/not JSON/);
    expect(() => parseCatalogFeed(JSON.stringify({ format: "other" }))).toThrow(
      new RegExp(CATALOG_FEED_FORMAT),
    );
  });
});

describe("refreshCatalogFeed", () => {
  test("adds community entries with remote provenance", async () => {
    const root = scratchRoot();
    serveFeed([communityModel("community-a")], '"v1"');
    const result = await refreshCatalogFeed(root, { environment: env });
    expect(result).toEqual({ outcome: "updated", url: FEED_URL, error: null });
    expect(existsSync(join(catalogFeedCacheDirectory(root), "feed.json"))).toBe(
      true,
    );
    load(root);
    expect(CATALOG.some(({ modelId }) => modelId === "community-a")).toBe(true);
    const provenance = catalogProvenance("community-a");
    expect(provenance.origin).toBe("remote");
    expect(provenance.trust).toBe("community");
    expect(provenance.path).toBe(`${FEED_URL}#models[0]`);
    const status = catalogFeedStatus(root, { environment: env });
    expect(status.added).toEqual(["community-a"]);
    expect(status.stale).toBe(false);
    expect(status.revision).toMatch(/^[0-9a-f]{64}$/);
  });

  test("skips the download while the cache is fresh", async () => {
    const root = scratchRoot();
    serveFeed([communityModel("community-a")]);
    let now = Date.parse("2026-10-01T00:00:00Z");
    await refreshCatalogFeed(root, { environment: env, now: () => now });
    expect(requests).toHaveLength(1);
    now += CATALOG_FEED_TTL_MS - 1;
    const fresh = await refreshCatalogFeed(root, {
      environment: env,
      now: () => now,
    });
    expect(fresh.outcome).toBe("fresh");
    expect(requests).toHaveLength(1);
    now += 2;
    const stale = await refreshCatalogFeed(root, {
      environment: env,
      now: () => now,
    });
    expect(stale.outcome).toBe("unchanged");
    expect(requests).toHaveLength(2);
  });

  test("revalidates with the cached etag", async () => {
    const root = scratchRoot();
    serveFeed([communityModel("community-a")], '"v1"');
    await refreshCatalogFeed(root, { environment: env });
    const again = await refreshCatalogFeed(root, {
      environment: env,
      force: true,
    });
    expect(again.outcome).toBe("unchanged");
    expect(requests[1]?.headers.get("if-none-match")).toBe('"v1"');
  });

  test("keeps the last good copy when the server fails", async () => {
    const root = scratchRoot();
    serveFeed([communityModel("community-a")]);
    await refreshCatalogFeed(root, { environment: env });
    reply = { status: 500, body: "nope" };
    const failed = await refreshCatalogFeed(root, {
      environment: env,
      force: true,
    });
    expect(failed.outcome).toBe("failed");
    expect(failed.error).toContain("500");
    reply = {
      body: '{"format":"localbase-catalog-feed","schemaVersion":1,"sha256":"',
    };
    const truncated = await refreshCatalogFeed(root, {
      environment: env,
      force: true,
    });
    expect(truncated.outcome).toBe("failed");
    load(root);
    expect(catalogProvenance("community-a").origin).toBe("remote");
    expect(catalogFeedStatus(root, { environment: env }).lastError).toBe(
      truncated.error,
    );
  });

  test("fails without a cache when the server is unreachable", async () => {
    const root = scratchRoot();
    const offline = {
      [CATALOG_FEED_URL_ENV]: `http://127.0.0.1:${PORT + 1}/feed.json`,
    };
    const result = await refreshCatalogFeed(root, { environment: offline });
    expect(result.outcome).toBe("failed");
    expect(result.error).toContain("Could not reach the feed");
    expect(readRemoteCatalogLayer(root, offline)).toBeNull();
  });

  test("rejects a redirect to plain http", async () => {
    const root = scratchRoot();
    const redirect = { [CATALOG_FEED_URL_ENV]: `${BASE}/redirect` };
    const result = await refreshCatalogFeed(root, {
      environment: redirect,
      // Do not follow to the real host; report where it would have gone.
      fetch: async () => {
        const response = new Response("{}", { status: 200 });
        Object.defineProperty(response, "url", {
          value: "http://example.com/feed.json",
        });
        return response;
      },
    });
    expect(result.outcome).toBe("failed");
    expect(result.error).toContain("redirected");
  });

  test("rejects feeds over the size limit", async () => {
    const root = scratchRoot();
    reply = { body: " ".repeat(8 * 1024 * 1024 + 1) };
    const result = await refreshCatalogFeed(root, { environment: env });
    expect(result.outcome).toBe("failed");
    expect(result.error).toContain("8 MiB");
  });

  test("reports invalid entries and applies the rest", async () => {
    const root = scratchRoot();
    serveFeed([
      { modelId: "broken", kind: "llm" },
      communityModel("community-a"),
    ]);
    await refreshCatalogFeed(root, { environment: env });
    load(root);
    expect(catalogProvenance("community-a").origin).toBe("remote");
    const remote = catalogDiagnostics().filter(({ tier }) => tier === "remote");
    expect(remote).toHaveLength(1);
    expect(remote[0]?.path).toBe(`${FEED_URL}#models[0]`);
    expect(catalogFeedStatus(root, { environment: env }).skipped).toBe(1);
  });

  test("never overrides built-in or user entries", async () => {
    const root = scratchRoot();
    const builtIn = BUILT_IN_CATALOG[0]!;
    serveFeed([
      { ...structuredClone(builtIn), displayName: "Hijacked" },
      communityModel("community-a"),
    ]);
    await refreshCatalogFeed(root, { environment: env });
    mkdirSync(userCatalogDirectory(root), { recursive: true });
    writeFileSync(
      join(userCatalogDirectory(root), "community-a.json"),
      JSON.stringify({ modelId: "community-a", displayName: "Mine" }),
    );
    load(root);
    const builtInEntry = CATALOG.find(
      ({ modelId }) => modelId === builtIn.modelId,
    );
    expect(builtInEntry?.displayName).toBe(builtIn.displayName);
    expect(catalogProvenance(builtIn.modelId).origin).toBe("built-in");
    const community = CATALOG.find(({ modelId }) => modelId === "community-a");
    expect(community?.displayName).toBe("Mine");
    const status = catalogFeedStatus(root, { environment: env });
    expect(status.builtIn).toBe(1);
    expect(status.entries).toBe(2);
  });

  test("does not write into an uninitialized root", async () => {
    const root = scratchRoot(false);
    serveFeed([communityModel("community-a")]);
    const result = await refreshCatalogFeed(root, { environment: env });
    expect(result.outcome).toBe("updated");
    expect(existsSync(join(root, "cache"))).toBe(false);
  });

  test("ignores a cache written for another URL", async () => {
    const root = scratchRoot();
    serveFeed([communityModel("community-a")]);
    await refreshCatalogFeed(root, { environment: env });
    const other = { [CATALOG_FEED_URL_ENV]: `${BASE}/other.json` };
    expect(readRemoteCatalogLayer(root, other)).toBeNull();
    expect(
      readRemoteCatalogLayer(root, { [CATALOG_FEED_URL_ENV]: "off" }),
    ).toBeNull();
  });
});

describe("feed commands", () => {
  test("refresh, status and use", async () => {
    const root = scratchRoot();
    process.env[CATALOG_FEED_URL_ENV] = FEED_URL;
    serveFeed([communityModel("community-a")]);
    const lines: string[] = [];
    const refreshed = await runFeedRefresh(
      { force: false },
      context(root),
      execution(lines),
    );
    expect(refreshed.data.outcome).toBe("updated");
    expect(refreshed.data.feed.added).toEqual(["community-a"]);
    expect(lines.join("\n")).toContain("1 community model(s)");

    const status = runFeedStatus({}, context(root), execution([]));
    expect(status.data.feed.url).toBe(FEED_URL);

    reply = { status: 503 };
    const failed = await runFeedRefresh(
      { force: true },
      context(root),
      execution([]),
    );
    expect(failed.exitCode).toBe(1);
    expect(failed.data.feed.added).toEqual(["community-a"]);

    const useLines: string[] = [];
    runFeedUse({ feed: "off" }, context(root), execution(useLines));
    expect(
      JSON.parse(readFileSync(join(root, "catalog-feed.json"), "utf8")),
    ).toEqual({
      url: null,
    });
    expect(useLines.join("\n")).toContain(CATALOG_FEED_URL_ENV);
  });

  test("use refuses an uninitialized root", () => {
    const root = scratchRoot(false);
    expect(() =>
      runFeedUse({ feed: "off" }, context(root), execution([])),
    ).toThrow(/local-base init/);
  });
});

describe("startCatalogFeedScheduler", () => {
  test("refreshes on start and reloads the catalog", async () => {
    const root = scratchRoot();
    process.env[CATALOG_FEED_URL_ENV] = FEED_URL;
    serveFeed([communityModel("community-a")]);
    const outcomes: string[] = [];
    const scheduler = startCatalogFeedScheduler(root, {
      onResult: (result) => outcomes.push(result.outcome),
      intervalMs: 60_000,
    });
    for (let i = 0; i < 100 && outcomes.length === 0; i++) {
      await Bun.sleep(10);
    }
    await scheduler.stop();
    expect(outcomes).toEqual(["updated"]);
    expect(catalogProvenance("community-a").origin).toBe("remote");
  });
});

describe("published feed", () => {
  test("catalog/feed.json matches the catalog data", () => {
    expect(readFileSync(CATALOG_FEED_PATH, "utf8")).toBe(renderCatalogFeed());
  });

  test("every published entry is a valid complete spec", () => {
    const feed = parseCatalogFeed(readFileSync(CATALOG_FEED_PATH, "utf8"));
    const composed = composeCatalog(
      [],
      [
        {
          tier: "remote",
          entries: feed.models.map((data, index) => ({
            path: `feed#models[${index}]`,
            data,
          })),
        },
      ],
    );
    expect(composed.diagnostics).toEqual([]);
    expect(composed.models).toHaveLength(feed.models.length);
  });
});
