// Golden Hugging Face discovery from the TypeScript code.
//
//   bun run go/tools/golden/hub.ts > go/internal/hub/testdata/golden.json
//
// Every Hub request goes through a recording fetch: responses are read from
// go/internal/hub/testdata/hub/ (keyed by the exact URL) and fetched from
// huggingface.co only when missing, then saved. Re-running replays offline.
// The Go tests serve the same files from a local hub.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { setFeedAnnotations, parseFeedAnnotations } from "../../../src/catalog/annotations";
import { parseCatalogFeed } from "../../../src/catalog/remote-feed";
import {
  commercialStatusForLicense,
  entryChoices,
  fetchModelInfo,
  generateEntry,
  likelyKind,
  modelIdFor,
  parseHfReference,
  quantOf,
  searchHub,
  vPrediction,
  type HubFetch,
  type HubModelInfo,
} from "../../../src/domains/models/huggingface";
import { catalogNotesFor, previewRepository } from "../../../src/domains/models/discovery";

const repoRoot = join(import.meta.dir, "../../..");
const fixtures = join(repoRoot, "go/internal/hub/testdata/hub");
mkdirSync(fixtures, { recursive: true });
const key = (url: string) => createHash("sha256").update(url).digest("hex").slice(0, 24);

const recording: HubFetch = async (url) => {
  const file = join(fixtures, `${key(url)}.json`);
  if (!existsSync(file)) {
    const response = await fetch(url, { headers: { accept: "application/json" } });
    const text = await response.text();
    writeFileSync(file, JSON.stringify({ url, status: response.status, body: text }, null, 1) + "\n");
  }
  const saved = JSON.parse(readFileSync(file, "utf8")) as { url: string; status: number; body: string };
  return {
    ok: saved.status >= 200 && saved.status < 300,
    status: saved.status,
    json: async () => JSON.parse(saved.body),
  };
};

// A scratch root that is not initialized: the cache is never written.
const root = join(tmpdir(), "lbgo-hub-golden-uninitialized");
const options = { root, fetch: recording };

const repos = [
  "bartowski/Qwen_Qwen3.5-0.8B-GGUF", "Qwen/Qwen2.5-Coder-1.5B-Instruct-GGUF", "Qwen/Qwen3-Coder-Next-GGUF",
  "ggml-org/Clef-Flash-GGUF", "ggerganov/whisper.cpp", "openai/whisper-large-v3", "Qwen/Qwen3-Embedding-0.6B-GGUF",
  "stabilityai/stable-diffusion-xl-base-1.0", "black-forest-labs/FLUX.1-schnell", "nerijs/pixel-art-xl",
  "Lykon/dreamshaper-8-lcm", "meta-llama/Llama-3.2-1B", "Laxhar/noobai-XL-Vpred-1.0", "nobody/does-not-exist-404",
];

const settle = async <T>(run: () => Promise<T> | T) => {
  try {
    return { ok: await run() };
  } catch (error) {
    return { error: (error as Error).message };
  }
};

const selectors: Record<string, (string | undefined)[]> = {
  "bartowski/Qwen_Qwen3.5-0.8B-GGUF": [undefined, "Q8_0", "q4_k_m", "Qwen_Qwen3.5-0.8B-Q6_K.gguf", "nope", "IQ4_XS"],
  "Qwen/Qwen2.5-Coder-1.5B-Instruct-GGUF": [undefined, "q8_0", "qwen2.5-coder-1.5b-instruct-fp16"],
  "Qwen/Qwen3-Coder-Next-GGUF": [undefined, "Q4_K_M", "Q8_0"],
  "ggml-org/Clef-Flash-GGUF": [undefined],
  "ggerganov/whisper.cpp": [undefined, "ggml-tiny.en-q8_0.bin", "q5_1", "whisper-small"],
  "stabilityai/stable-diffusion-xl-base-1.0": [undefined],
  "black-forest-labs/FLUX.1-schnell": [undefined],
  "nerijs/pixel-art-xl": [undefined],
  "Lykon/dreamshaper-8-lcm": [undefined],
  "Laxhar/noobai-XL-Vpred-1.0": [undefined],
};

const infos: Record<string, HubModelInfo> = {};
const models = [];
for (const repo of repos) {
  const info = await settle(() => fetchModelInfo(options, repo));
  if ("ok" in info) infos[repo] = info.ok;
  const entry = "ok" in info ? info.ok : null;
  models.push({
    repo,
    info: "ok" in info ? { id: entry!.id, sha: entry!.sha, schedulerPrediction: entry!.schedulerPrediction ?? null } : info,
    likely: entry ? likelyKind(entry.pipeline_tag, entry.tags, { repo: entry.id, library: entry.library_name ?? null }) : null,
    choices: entry ? await settle(() => {
      const c = entryChoices(entry);
      return { kind: c.kind, choices: c.choices.map((f) => f.path), default: c.default?.path ?? null, pipeline: c.pipeline, lora: c.lora };
    }) : null,
    vPrediction: entry ? vPrediction(entry) : null,
    entries: entry ? await Promise.all((selectors[repo] ?? [undefined]).map(async (selector) => ({
      selector: selector ?? null,
      result: await settle(() => {
        const generated = generateEntry(entry, { repo, ...(selector ? { selector } : {}) });
        return { ...generated, weights: generated.weights.map((w) => ({ ...w })) };
      }),
    }))) : [],
  });
}
// An explicit id, and an id a built-in owns (generateEntry itself accepts it).
const explicitID = await settle(() => generateEntry(infos["ggerganov/whisper.cpp"]!, { repo: "ggerganov/whisper.cpp", selector: "ggml-base.en-q8_0.bin" }, { modelId: "my-whisper" }).spec);
const badID = await settle(() => generateEntry(infos["ggerganov/whisper.cpp"]!, { repo: "ggerganov/whisper.cpp" }, { modelId: "Bad ID" }).spec);

const searches = [];
for (const [kind, query, limit] of [["llm", "qwen coder", 5], ["llm", "gguf llama", 5], ["stt", "", 5], ["image", "flux", 8], ["image", "", 5]] as const) {
  searches.push({ kind, query, limit, result: await settle(() => searchHub({ ...options, kind, limit }, query)) });
}

// Discovery with the published feed's annotations in force.
const feed = parseCatalogFeed(readFileSync(join(repoRoot, "catalog/feed-v2.json"), "utf8"));
setFeedAnnotations(parseFeedAnnotations(feed.annotations ?? []).annotations);
const previews = [];
for (const repo of repos) {
  previews.push({ repo, preview: await settle(() => previewRepository({ ...options, systemVramGb: 64 }, repo)), notes: catalogNotesFor(repo, infos[repo]?.sha) });
}

const references = ["hf:org/repo", "hf:org/repo:Q4_K_M", " hf:org/repo:file.gguf ", "hf:org/repo:a:b", "org/repo", "hf:bad", "hf:../x", "hf:org/.."].map((value) => ({ value, result: (() => { try { return { ok: parseHfReference(value) }; } catch (error) { return { error: (error as Error).message }; } })() }));
const quants = ["Qwen-Q4_K_M.gguf", "model.IQ4_XS.gguf", "x-bf16.safetensors", "ggml-base.en-q5_1.bin", "m-MXFP4_MOE.gguf", "noquant.gguf", "a-Q8_0-00001-of-00002.gguf", "q4_k_m"].map((path) => ({ path, quant: quantOf(path), llm: modelIdFor(path, "llm"), stt: modelIdFor(path, "stt"), image: modelIdFor(path, "image") }));
const licenses = [null, "apache-2.0", "cc-by-nc-4.0", "llama3.2", "research-only", "openrail++", "other"].map((license) => ({ license, status: commercialStatusForLicense(license) }));

// The gateway's search with discovery previews, the feed off (as the
// contract gateways run).
setFeedAnnotations([]);
const { discoverModels } = await import("../../../src/domains/models/discovery");
const discovery = await settle(() => discoverModels({ ...options, kind: "llm", systemVramGb: 64 }, "qwen coder"));

const cli = await runCLICases();
console.log(JSON.stringify({ models, explicitID, badID, searches, previews, references, quants, licenses, discovery, cli }, null, 1));

// The CLI handlers themselves, on a scratch root (catalog.d only; the
// root has no marker, so nothing else is written), output captured.
async function runCLICases() {
  const { mkdtempSync, readFileSync: read, realpathSync } = await import("node:fs");
  const { runAdd, runSearch, runRemoveEntry } = await import("../../../src/domains/models/commands/hf");
  const { loadCatalogSources } = await import("../../../src/catalog/user-catalog");
  const scratch = realpathSync(mkdtempSync(join(tmpdir(), "lbgo-hub-cli-")));
  setFeedAnnotations([]);
  loadCatalogSources(scratch);
  const config = { root: scratch, hfToken: "", selectedLlmModels: [], selectedSttModels: ["whisper-base.en-q8_0"], selectedTtsModels: [], selectedImageModels: [], selectedVideoModels: [] };
  const run = async (label: string, fn: (execution: unknown) => Promise<unknown> | unknown) => {
    const lines: string[] = [];
    const execution = { output: { info: (m: string) => lines.push(m), error: (m: string) => lines.push(`ERROR ${m}`) } };
    try {
      const result = (await fn(execution)) as { data: unknown };
      return { label, lines: lines.map((l) => l.replaceAll(scratch, "<root>")), data: JSON.parse(JSON.stringify(result.data).replaceAll(scratch, "<root>")) };
    } catch (error) {
      return { label, lines, error: (error as Error).message.replaceAll(scratch, "<root>") };
    }
  };
  const ctx = { config } as never;
  const cases = [];
  for (const [kind, query, limit] of [["llm", "qwen coder", 5], ["stt", "", 5], ["image", "flux", 8]] as const) {
    cases.push(await run(`search ${kind} ${query}`, (ex) => runSearch({ kind, query, limit }, ctx, ex as never, { fetch: recording })));
  }
  const add = (reference: string, id?: string) =>
    run(`add ${reference}${id ? ` --id ${id}` : ""}`, (ex) => runAdd({ reference, install: false, ...(id ? { id } : {}) }, ctx, ex as never, { fetch: recording }));
  for (const ref of ["hf:bartowski/Qwen_Qwen3.5-0.8B-GGUF", "hf:ggerganov/whisper.cpp:ggml-base.en-q8_0.bin", "hf:nerijs/pixel-art-xl",
    "hf:black-forest-labs/FLUX.1-schnell", "hf:Qwen/Qwen3-Coder-Next-GGUF:Q8_0", "hf:bartowski/Qwen_Qwen3.5-0.8B-GGUF", "hf:ggml-org/Clef-Flash-GGUF",
    "hf:bad", "hf:openai/whisper-large-v3"]) {
    const result = await add(ref);
    const file = "data" in result && result.data && typeof result.data === "object" && "path" in (result.data as object)
      ? read(((result.data as { path: string }).path).replace("<root>", scratch), "utf8") : null;
    cases.push({ ...result, file });
  }
  cases.push(await add("hf:ggerganov/whisper.cpp:ggml-small-q8_0.bin", "clef-flash-q4_k_m"));
  cases.push(await add("hf:ggerganov/whisper.cpp:ggml-small-q8_0.bin", "my-small"));
  for (const id of ["whisper-base.en-q8_0", "my-small", "no-such-entry", "clef-flash-q4_k_m"]) {
    cases.push(await run(`remove-entry ${id}`, (ex) => runRemoveEntry({ modelId: id }, ctx, ex as never)));
  }
  return cases;
}
