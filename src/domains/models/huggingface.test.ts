import { afterEach, expect, test } from "bun:test";
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../context";
import { byId, modelSpecSchema } from "../../catalog";
import { applyCatalogLayers, catalogProvenance } from "../../catalog/sources";
import { defaultConfig } from "../../manager";
import type { CommandExecution } from "../app/commands/framework";
import { runAdd, runRemoveEntry, runSearch } from "./commands/hf";
import {
  commercialStatusForLicense,
  fetchModelInfo,
  generateEntry,
  HubError,
  likelyKind,
  modelIdFor,
  parseHfReference,
  quantOf,
  searchHub,
  type HubFetch,
  type HubModelInfo,
} from "./huggingface";

const SHA = "9217f5db79a29953eb74d5343926648285ec7e67";
const roots: string[] = [];

/** A LocalBase-shaped scratch root, so the Hub cache may write into it. */
function scratchRoot(initialized = true): string {
  const root = realpathSync(mkdtempSync(join(tmpdir(), "lb-hf-")));
  if (initialized) writeFileSync(join(root, ".localbase-root.json"), "{}");
  roots.push(root);
  return root;
}

afterEach(() => {
  applyCatalogLayers([]);
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

function lfs(rfilename: string, size: number, fill: string) {
  return { rfilename, size, lfs: { sha256: fill.repeat(64), size } };
}

function modelInfo(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    id: "TinyOrg/Tiny-0.5B-Instruct-GGUF",
    sha: SHA,
    pipeline_tag: "text-generation",
    tags: ["gguf", "license:apache-2.0"],
    cardData: { license: "apache-2.0" },
    gguf: {
      architecture: "qwen2",
      context_length: 8192,
      chat_template:
        "{% if tools %}{{ tools | tojson }}{% endif %}{% for message in messages %}{{ message.content }}{% if message.tool_calls %}<tool_call>{% endif %}{% endfor %}",
    },
    siblings: [
      { rfilename: "README.md", size: 10 },
      lfs("tiny-0.5b-instruct-q4_k_m.gguf", 400_000_000, "a"),
      lfs("tiny-0.5b-instruct-q8_0.gguf", 600_000_000, "b"),
      lfs("tiny-0.5b-instruct-fp16.gguf", 1_000_000_000, "c"),
    ],
    ...overrides,
  };
}

type Call = { url: string; headers: Record<string, string> };

function fakeFetch(
  respond: (url: string) => { status?: number; body?: unknown } | Error,
): { fetch: HubFetch; calls: Call[] } {
  const calls: Call[] = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({
        url,
        headers: (init?.headers ?? {}) as Record<string, string>,
      });
      const result = respond(url);
      if (result instanceof Error) throw result;
      const status = result.status ?? 200;
      return {
        ok: status >= 200 && status < 300,
        status,
        json: async () => result.body,
      };
    },
  };
}

function parseInfo(body: unknown): Promise<HubModelInfo> {
  const { fetch } = fakeFetch(() => ({ body }));
  return fetchModelInfo(
    { root: scratchRoot(false), fetch },
    "TinyOrg/Tiny-0.5B-Instruct-GGUF",
  );
}

const execution = (lines: string[]): CommandExecution => ({
  global: { nonInteractive: true, json: false },
  output: {
    info: (message) => lines.push(message),
    error: (message) => lines.push(message),
    lifecycle() {},
  },
});

function context(root: string): AppContext {
  return {
    config: { ...defaultConfig(root), hfToken: "" },
  } as unknown as AppContext;
}

test("parses hf references", () => {
  expect(parseHfReference("hf:Org/Repo-GGUF")).toEqual({
    repo: "Org/Repo-GGUF",
  });
  expect(parseHfReference("hf:Org/Repo:Q8_0")).toEqual({
    repo: "Org/Repo",
    selector: "Q8_0",
  });
  expect(() => parseHfReference("Org/Repo")).toThrow(HubError);
  expect(() => parseHfReference("hf:../etc")).toThrow(HubError);
});

test("classifies quants, ids, licenses and kinds", () => {
  expect(quantOf("x/Model-Q4_K_M.gguf")).toBe("Q4_K_M");
  expect(quantOf("model-iq2_xxs.gguf")).toBe("IQ2_XXS");
  expect(quantOf("model.gguf")).toBeNull();
  expect(modelIdFor("dir/My Model-Q4_K_M-00001-of-00002.gguf")).toBe(
    "my-model-q4_k_m",
  );
  expect(commercialStatusForLicense("apache-2.0")).toBe("open");
  expect(commercialStatusForLicense("cc-by-nc-4.0")).toBe("prohibited");
  expect(commercialStatusForLicense("llama3.2")).toBe("conditional");
  expect(commercialStatusForLicense(null)).toBe("conditional");
  expect(likelyKind("text-generation", [])).toEqual({
    kind: "llm",
    label: "llm",
  });
  expect(likelyKind("image-text-to-text", []).label).toBe("llm (vision)");
  expect(likelyKind("feature-extraction", []).kind).toBeNull();
  expect(likelyKind(undefined, ["gguf"]).kind).toBe("llm");
});

test("generates a pinned entry with the preferred quant", async () => {
  const info = await parseInfo(modelInfo());
  const entry = generateEntry(
    info,
    parseHfReference("hf:TinyOrg/Tiny-0.5B-Instruct-GGUF"),
  );
  expect(entry.modelId).toBe("tiny-0.5b-instruct-q4_k_m");
  expect(entry.revision).toBe(SHA);
  expect(entry.license).toBe("apache-2.0");
  expect(entry.detected).toContain("tool-calling");
  expect(entry.projector).toBeNull();
  const spec = modelSpecSchema.parse(entry.spec);
  expect(spec).toMatchObject({
    kind: "llm",
    repositoryRevision: SHA,
    quant: "Q4_K_M",
    size: "0.5B",
    contextWindowTokens: 8192,
    commercialStatus: "open",
    inputModalities: ["text"],
  });
  expect(spec.artifacts).toEqual([
    {
      sourcePath: "tiny-0.5b-instruct-q4_k_m.gguf",
      filename: "tiny-0.5b-instruct-q4_k_m.gguf",
      expectedSizeBytes: 400_000_000,
      sha256: "a".repeat(64),
      role: "primary",
    },
  ]);
  // Detected capabilities are reported, never written into the catalog.
  expect(spec.capabilities.map(({ id }) => id)).toEqual(["text-generation"]);
});

test("selects by quant or file name and lists choices when ambiguous", async () => {
  const info = await parseInfo(modelInfo());
  const reference = (selector: string) =>
    parseHfReference(`hf:TinyOrg/Tiny-0.5B-Instruct-GGUF:${selector}`);
  expect(generateEntry(info, reference("q8_0")).weights[0]!.path).toBe(
    "tiny-0.5b-instruct-q8_0.gguf",
  );
  expect(
    generateEntry(info, reference("tiny-0.5b-instruct-fp16.gguf")).weights[0]!
      .path,
  ).toBe("tiny-0.5b-instruct-fp16.gguf");
  expect(() => generateEntry(info, reference("Q2_K"))).toThrow(
    /no GGUF file or quant named "Q2_K"[\s\S]*tiny-0\.5b-instruct-q8_0\.gguf \(Q8_0\)/,
  );
  const noPreferred = await parseInfo(
    modelInfo({
      siblings: [lfs("a-q3_k_m.gguf", 10, "a"), lfs("a-q6_k.gguf", 10, "b")],
    }),
  );
  expect(() =>
    generateEntry(
      noPreferred,
      parseHfReference("hf:TinyOrg/Tiny-0.5B-Instruct-GGUF"),
    ),
  ).toThrow(/has 2 GGUF files; pick one/);
});

test("expands shards and pairs a vision projector", async () => {
  const info = await parseInfo(
    modelInfo({
      pipeline_tag: "image-text-to-text",
      siblings: [
        lfs("Q4_K_M/big-q4_k_m-00002-of-00002.gguf", 200, "b"),
        lfs("Q4_K_M/big-q4_k_m-00001-of-00002.gguf", 100, "a"),
        lfs("mmproj-model-bf16.gguf", 30, "d"),
        lfs("mmproj-model-f16.gguf", 20, "c"),
      ],
    }),
  );
  const entry = generateEntry(
    info,
    parseHfReference("hf:TinyOrg/Tiny-0.5B-Instruct-GGUF"),
  );
  expect(entry.weights.map(({ path }) => path)).toEqual([
    "Q4_K_M/big-q4_k_m-00001-of-00002.gguf",
    "Q4_K_M/big-q4_k_m-00002-of-00002.gguf",
  ]);
  expect(entry.projector?.path).toBe("mmproj-model-f16.gguf");
  const spec = modelSpecSchema.parse(entry.spec);
  expect(spec.inputModalities).toEqual(["text", "image"]);
  expect(spec.artifacts.map(({ filename, role }) => [filename, role])).toEqual([
    ["big-q4_k_m-00001-of-00002.gguf", "primary"],
    ["big-q4_k_m-00002-of-00002.gguf", "supplementary"],
    ["big-q4_k_m-mmproj-model-f16.gguf", "supplementary"],
  ]);

  const missingShard = await parseInfo(
    modelInfo({ siblings: [lfs("big-q4_k_m-00001-of-00003.gguf", 1, "a")] }),
  );
  expect(() =>
    generateEntry(
      missingShard,
      parseHfReference("hf:TinyOrg/Tiny-0.5B-Instruct-GGUF"),
    ),
  ).toThrow(/split into 3 parts but 1/);
});

test("refuses repositories LocalBase cannot run", async () => {
  const embeddings = await parseInfo(
    modelInfo({ pipeline_tag: "feature-extraction" }),
  );
  expect(() =>
    generateEntry(
      embeddings,
      parseHfReference("hf:TinyOrg/Tiny-0.5B-Instruct-GGUF"),
    ),
  ).toThrow(/only supports GGUF language models/);
  const safetensors = await parseInfo(
    modelInfo({ siblings: [lfs("model.safetensors", 10, "a")] }),
  );
  expect(() =>
    generateEntry(
      safetensors,
      parseHfReference("hf:TinyOrg/Tiny-0.5B-Instruct-GGUF"),
    ),
  ).toThrow(/no GGUF weights/);
  await expect(parseInfo({ id: "x", sha: "short" })).rejects.toThrow(
    /does not understand/,
  );
});

test("caches responses, reuses them while fresh and falls back when offline", async () => {
  const root = scratchRoot();
  let now = 1_000_000;
  let offline = false;
  const { fetch, calls } = fakeFetch(() =>
    offline ? new Error("getaddrinfo ENOTFOUND") : { body: modelInfo() },
  );
  const options = { root, fetch, now: () => now };
  await fetchModelInfo(options, "TinyOrg/Tiny-0.5B-Instruct-GGUF");
  expect(calls).toHaveLength(1);
  expect(calls[0]!.url).toBe(
    "https://huggingface.co/api/models/TinyOrg/Tiny-0.5B-Instruct-GGUF?blobs=true",
  );
  expect(readdirSync(join(root, "cache", "hf"))).toHaveLength(1);

  now += 60_000;
  await fetchModelInfo(options, "TinyOrg/Tiny-0.5B-Instruct-GGUF");
  expect(calls).toHaveLength(1);

  now += 60 * 60_000;
  offline = true;
  const stale = await fetchModelInfo(
    options,
    "TinyOrg/Tiny-0.5B-Instruct-GGUF",
  );
  expect(stale.sha).toBe(SHA);
  expect(calls).toHaveLength(2);
});

test("explains offline, missing and gated repositories", async () => {
  const root = scratchRoot();
  const fail = (respond: Parameters<typeof fakeFetch>[0]) =>
    fetchModelInfo({ root, fetch: fakeFetch(respond).fetch }, "TinyOrg/Gated");
  await expect(fail(() => new Error("ENOTFOUND"))).rejects.toThrow(
    /Could not reach Hugging Face.*nothing was changed/,
  );
  await expect(fail(() => ({ status: 404 }))).rejects.toThrow(
    /no public repository/,
  );
  await expect(fail(() => ({ status: 403 }))).rejects.toThrow(
    /refused access \(403\).*configure --hf-token/,
  );
  await expect(fail(() => ({ status: 503 }))).rejects.toThrow(/answered 503/);
});

test("sends the token only to huggingface.co and never caches outside a root", async () => {
  const root = scratchRoot(false);
  const { fetch, calls } = fakeFetch(() => ({ body: modelInfo() }));
  await fetchModelInfo(
    { root, fetch, token: "hf_secret" },
    "TinyOrg/Tiny-0.5B-Instruct-GGUF",
  );
  expect(calls[0]!.headers.authorization).toBe("Bearer hf_secret");
  expect(existsSync(join(root, "cache"))).toBe(false);
});

test("searches GGUF repositories by downloads", async () => {
  const { fetch, calls } = fakeFetch(() => ({
    body: [
      {
        id: "TinyOrg/Tiny-GGUF",
        downloads: 275_200,
        likes: 3,
        pipeline_tag: "text-generation",
        tags: ["gguf", "license:apache-2.0"],
      },
      {
        id: "TinyOrg/Embed-GGUF",
        downloads: 5,
        pipeline_tag: "feature-extraction",
        tags: [],
      },
    ],
  }));
  const { hits, cached } = await searchHub(
    { root: scratchRoot(), fetch, limit: 5 },
    "qwen gguf 0.5b",
  );
  const url = new URL(calls[0]!.url);
  expect(url.pathname).toBe("/api/models");
  expect(Object.fromEntries(url.searchParams)).toEqual({
    filter: "gguf",
    sort: "downloads",
    direction: "-1",
    limit: "5",
    search: "qwen 0.5b",
  });
  expect(cached).toBe(false);
  expect(hits[0]).toMatchObject({
    repo: "TinyOrg/Tiny-GGUF",
    license: "apache-2.0",
    likely: { kind: "llm" },
  });
  expect(hits[1]!.likely.kind).toBeNull();
});

test("models search prints a table and the add hint", async () => {
  const lines: string[] = [];
  const { fetch } = fakeFetch(() => ({
    body: [
      {
        id: "TinyOrg/Tiny-GGUF",
        downloads: 275_200,
        pipeline_tag: "text-generation",
        tags: ["license:mit"],
      },
    ],
  }));
  const result = await runSearch(
    { query: "tiny", limit: 20 },
    context(scratchRoot()),
    execution(lines),
    { fetch },
  );
  expect(lines[0]).toMatch(
    /^MODEL\s+DOWNLOADS\s+LICENSE\s+PIPELINE\s+LIKELY KIND$/,
  );
  expect(lines[1]).toMatch(
    /^TinyOrg\/Tiny-GGUF\s+275\.2k\s+mit\s+text-generation\s+llm$/,
  );
  expect(lines.at(-1)).toContain("local-base models add hf:");
  expect(result.data.results[0]).toMatchObject({
    addable: true,
    likelyKind: "llm",
  });
});

test("models add writes a user entry and remove-entry deletes it", async () => {
  const root = scratchRoot();
  const lines: string[] = [];
  const { fetch } = fakeFetch(() => ({ body: modelInfo() }));
  const ctx = context(root);
  const result = await runAdd(
    { reference: "hf:TinyOrg/Tiny-0.5B-Instruct-GGUF:Q8_0", install: false },
    ctx,
    execution(lines),
    { fetch },
  );
  expect(result.data).toMatchObject({
    modelId: "tiny-0.5b-instruct-q8_0",
    revision: SHA,
    replaced: false,
    installedPath: null,
    detectedCapabilities: ["tool-calling"],
  });
  expect(result.data.path).toBe(
    join(root, "catalog.d", "tiny-0.5b-instruct-q8_0.json"),
  );
  expect(catalogProvenance("tiny-0.5b-instruct-q8_0").trust).toBe("user");
  expect(lines.join("\n")).toContain(
    "Install it with: local-base models install tiny-0.5b-instruct-q8_0",
  );

  const again = await runAdd(
    { reference: "hf:TinyOrg/Tiny-0.5B-Instruct-GGUF:Q8_0", install: false },
    ctx,
    execution([]),
    { fetch },
  );
  expect(again.data.replaced).toBe(true);

  const removed = runRemoveEntry(
    { modelId: "tiny-0.5b-instruct-q8_0" },
    ctx,
    execution([]),
  );
  expect(removed.data).toMatchObject({ restoredTier: null, selected: false });
  expect(byId("tiny-0.5b-instruct-q8_0")).toBeUndefined();
  expect(() =>
    runRemoveEntry({ modelId: "tiny-0.5b-instruct-q8_0" }, ctx, execution([])),
  ).toThrow(/No user catalog entry/);
});

test("models add refuses built-in ids and remove-entry refuses selected models", async () => {
  const root = scratchRoot();
  const ctx = context(root);
  const { fetch } = fakeFetch(() => ({ body: modelInfo() }));
  await expect(
    runAdd(
      {
        reference: "hf:TinyOrg/Tiny-0.5B-Instruct-GGUF",
        install: false,
        id: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      },
      ctx,
      execution([]),
      { fetch },
    ),
  ).rejects.toThrow(/already a built-in catalog model/);
  await runAdd(
    { reference: "hf:TinyOrg/Tiny-0.5B-Instruct-GGUF", install: false },
    ctx,
    execution([]),
    { fetch },
  );
  ctx.config.selectedLlmModels = [
    ...ctx.config.selectedLlmModels,
    "tiny-0.5b-instruct-q4_k_m",
  ];
  expect(() =>
    runRemoveEntry(
      { modelId: "tiny-0.5b-instruct-q4_k_m" },
      ctx,
      execution([]),
    ),
  ).toThrow(/is selected. Deselect it first/);
  expect(byId("tiny-0.5b-instruct-q4_k_m")).toBeDefined();
});
