// Golden catalog-feed parsing, annotations and their application from the
// TypeScript code.
//
//   bun run go/tools/golden/feed.ts > go/internal/catalog/testdata/feed.json
//
// Feeds: the published catalog/feed-v2.json and catalog/feed.json, and
// crafted documents (checksums over JSON.stringify of tricky values, newer
// versions, malformed headers). Annotations: tolerant parsing of good and
// bad items, then every built-in model's summary, plannable peaks and
// effective unsupported capabilities under the published annotations.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { BUILT_IN_CATALOG } from "../../../src/catalog";
import {
  annotationSummaryFor,
  effectiveUnsupportedCapabilities,
  parseFeedAnnotations,
  plannableFeedPeaks,
  setFeedAnnotations,
} from "../../../src/catalog/annotations";
import {
  buildCatalogFeed,
  catalogFeedUrlProblem,
  parseCatalogFeed,
} from "../../../src/catalog/remote-feed";

const repo = join(import.meta.dir, "../../..");
const published = readFileSync(join(repo, "catalog/feed-v2.json"), "utf8");
const legacy = readFileSync(join(repo, "catalog/feed.json"), "utf8");

const sha = (text: string) => createHash("sha256").update(text).digest("hex");
const header = (schemaVersion: number, body: Record<string, unknown>, extra: Record<string, unknown> = {}) => {
  const sorted = Object.fromEntries(Object.keys(body).sort().map((k) => [k, body[k]]));
  return JSON.stringify({ format: "localbase-catalog-feed", schemaVersion, ...extra, sha256: sha(JSON.stringify(sorted)), ...body });
};
const tricky = [{ modelId: "x", n: [1e21, 1e-7, 0.1, -0, 100], "10": "a", "2": "b", s: "é😀 \u0001" }];

const feeds: { name: string; text: string }[] = [
  { name: "published v2", text: published },
  { name: "published v1", text: legacy },
  { name: "built v2", text: JSON.stringify(buildCatalogFeed(tricky, [])) },
  { name: "built v2 reformatted", text: JSON.stringify(buildCatalogFeed(tricky, []), null, 2) },
  { name: "v2 extra body key", text: header(2, { models: tricky, annotations: [], notes: { b: 1, a: 2 } }, { minReaderVersion: 2 }) },
  { name: "v3 readable", text: header(3, { models: [], annotations: [], future: true }, { minReaderVersion: 2 }) },
  { name: "v3 unreadable", text: header(3, { models: [], annotations: [] }, { minReaderVersion: 3 }) },
  { name: "v3 no min reader", text: header(3, { models: [], annotations: [] }) },
  { name: "v1 built", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: 1, sha256: sha(JSON.stringify(tricky)), models: tricky }) },
  { name: "altered", text: published.replace('"modelId"', '"modelId" ') .replace(/"sha256":"[0-9a-f]{64}"/, (m) => m) .replace("qwen", "Qwen") },
  { name: "not json", text: "{" },
  { name: "array", text: "[]" },
  { name: "wrong format", text: JSON.stringify({ format: "other", schemaVersion: 2 }) },
  { name: "string version", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: "2" }) },
  { name: "bad sha", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: 2, sha256: "XYZ", models: [] }) },
  { name: "models not array", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: 2, sha256: "0".repeat(64), models: {} }) },
  { name: "missing models", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: 2, sha256: "0".repeat(64) }) },
  { name: "annotations not array", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: 2, sha256: "0".repeat(64), models: [], annotations: 5 }) },
  { name: "min reader zero", text: JSON.stringify({ format: "localbase-catalog-feed", schemaVersion: 2, minReaderVersion: 0, sha256: "0".repeat(64), models: [] }) },
  { name: "too many models", text: header(2, { models: Array.from({ length: 5001 }, () => 0), annotations: [] }) },
];

const feedResults = feeds.map(({ name, text }) => {
  try {
    const feed = parseCatalogFeed(text);
    return { name, text, ok: { schemaVersion: feed.schemaVersion, sha256: feed.sha256, models: feed.models.length, annotations: feed.annotations?.length ?? null } };
  } catch (error) {
    return { name, text, error: (error as Error).message };
  }
});

const peak = { variant: "Q4_K_M", launchShape: "ctx=8192;slots=1;kv=q8_0/q8_0", peakBytes: 100, backendAcceleratorBytes: 50, hostBufferBytes: 10, peakRssBytes: 90, measuredAt: "2026-10-01", hardware: "M2" };
const mark = { id: "tool-calling", supported: false, source: "declared", verified: true, reason: "Breaks." };
const annotationItems = [
  { repo: "org/name", revision: "a".repeat(40) },
  { repo: "org/name", revision: "A".repeat(40), file: "m.gguf", modelId: "m", kind: "llm", status: "verified", unsupportedCapabilities: [mark, { id: "bogus" }], memoryPeaks: [peak, { ...peak, peakBytes: -1 }], recipe: { modelId: "m", files: [], totalBytes: 1 } },
  { repo: "org/name", revision: "b".repeat(40), status: "unknown", kind: "x", file: "", modelId: "", unsupportedCapabilities: "nope", memoryPeaks: [] },
  { repo: "bad repo", revision: "a".repeat(40) },
  { repo: "org/name", revision: "short" },
  { repo: "org/name", revision: "c".repeat(40), extra: 1 },
  "not an object",
  null,
  { repo: "o/n", revision: "d".repeat(40), recipe: { modelId: "m", files: [{ repositoryUrl: "https://huggingface.co/o/n", revision: "d".repeat(40), path: "a.gguf", sizeBytes: 5, sha256: "e".repeat(64), role: "primary" }], totalBytes: 5 } },
];
const annotationResult = parseFeedAnnotations(annotationItems);

const publishedFeed = parseCatalogFeed(published);
const publishedAnnotations = parseFeedAnnotations(publishedFeed.annotations ?? []);
setFeedAnnotations(publishedAnnotations.annotations);
const application = BUILT_IN_CATALOG.map((model) => ({
  modelId: model.modelId,
  summary: annotationSummaryFor(model),
  peaks: plannableFeedPeaks(model),
  unsupported: effectiveUnsupportedCapabilities(model),
}));

const urls = ["https://example.com/feed.json", "http://localhost:8080/f", "http://127.0.0.1/f", "http://[::1]/f", "http://example.com/f",
  "https://user:pw@example.com/f", "ftp://example.com/f", "not a url", "", "https://", "HTTPS://EXAMPLE.COM/F", "http://LOCALHOST/f", "file:///etc/passwd"];
const urlProblems = urls.map((url) => ({ url, problem: catalogFeedUrlProblem(url) }));

console.log(JSON.stringify({ feeds: feedResults, annotations: { items: annotationItems, result: annotationResult },
  published: { annotations: publishedAnnotations.annotations.length, skipped: publishedAnnotations.skipped }, application, urlProblems }, null, 1));
