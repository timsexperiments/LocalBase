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
  checkHubToken,
  HubError,
  imageLoraBaseFor,
  likelyKind,
  entryChoices,
  isGated,
  isDecisionRepository,
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
  ).toThrow(/looks like embeddings/);
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
  const code = (respond: Parameters<typeof fakeFetch>[0]) =>
    fail(respond).catch((error: HubError) => error.code);
  expect(await code(() => ({ status: 401 }))).toBe("hub_auth_required");
  expect(await code(() => ({ status: 403 }))).toBe("hub_auth_required");
  expect(await code(() => ({ status: 429 }))).toBe("hub_rate_limited");
  expect(await code(() => ({ status: 503 }))).toBeUndefined();
  expect(await code(() => ({ status: 404 }))).toBeUndefined();
  await expect(fail(() => ({ status: 429 }))).rejects.toThrow(
    /rate limited.*save a Hugging Face token/,
  );
});

test("checks a token with whoami and never caches or echoes it", async () => {
  const token = "hf_" + "x".repeat(34);
  const { fetch, calls } = fakeFetch(() => ({
    body: { name: "someone", auth: { accessToken: { role: "read" } } },
  }));
  const valid = await checkHubToken(token, fetch);
  expect(valid).toEqual({ state: "valid", name: "someone", role: "read" });
  expect(calls[0]!.url).toBe("https://huggingface.co/api/whoami-v2");
  expect(calls[0]!.headers.authorization).toBe(`Bearer ${token}`);
  expect(JSON.stringify(valid)).not.toContain(token);
  expect(
    await checkHubToken(token, fakeFetch(() => ({ status: 401 })).fetch),
  ).toEqual({ state: "invalid" });
  expect(
    await checkHubToken(token, fakeFetch(() => ({ status: 500 })).fetch),
  ).toMatchObject({ state: "unreachable" });
  expect(
    await checkHubToken(token, fakeFetch(() => new Error("offline")).fetch),
  ).toMatchObject({ state: "unreachable" });
  expect(
    await checkHubToken(token, fakeFetch(() => ({ body: {} })).fetch),
  ).toEqual({ state: "valid", name: null, role: null });
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
        sha: SHA.toUpperCase(),
        downloads: 275_200,
        likes: 3,
        pipeline_tag: "text-generation",
        tags: ["gguf", "license:apache-2.0"],
      },
      {
        id: "TinyOrg/Embed-GGUF",
        sha: "not-a-commit",
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
  const { "expand[]": _expand, ...rest } = Object.fromEntries(url.searchParams);
  expect(rest).toEqual({
    filter: "gguf",
    sort: "downloads",
    direction: "-1",
    limit: "5",
    search: "qwen 0.5b",
  });
  expect(url.searchParams.getAll("expand[]")).toEqual([
    "downloads",
    "likes",
    "pipeline_tag",
    "tags",
    "gated",
    "library_name",
    "sha",
  ]);
  expect(cached).toBe(false);
  expect(hits[0]).toMatchObject({
    repo: "TinyOrg/Tiny-GGUF",
    license: "apache-2.0",
    likely: { kind: "llm" },
    sha: SHA,
  });
  expect(hits[1]!.likely.kind).toBeNull();
  expect(hits[1]!.sha).toBeUndefined();
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
    /^MODEL\s+DOWNLOADS\s+LICENSE\s+PIPELINE\s+GATED\s+LIKELY KIND$/,
  );
  expect(lines[1]).toMatch(
    /^TinyOrg\/Tiny-GGUF\s+275\.2k\s+mit\s+text-generation\s+-\s+llm$/,
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

function whisperInfo(overrides: Partial<Record<string, unknown>> = {}) {
  return modelInfo({
    id: "ggerganov/whisper.cpp",
    pipeline_tag: "automatic-speech-recognition",
    tags: ["license:mit"],
    cardData: { license: "mit" },
    gguf: undefined,
    siblings: [
      lfs("ggml-base.bin", 148_000_000, "a"),
      lfs("ggml-base-q8_0.bin", 82_000_000, "b"),
      lfs("ggml-base.en-q8_0.bin", 82_000_000, "c"),
      lfs("ggml-small-q8_0.bin", 264_000_000, "d"),
      lfs("ggml-base-encoder.mlmodelc.zip", 38_000_000, "e"),
      lfs("ggml-silero-v5.1.2.bin", 900_000, "f"),
    ],
    ...overrides,
  });
}

function imageInfo(overrides: Partial<Record<string, unknown>> = {}) {
  return modelInfo({
    id: "TinyArt/tiny-diffusion-v1",
    pipeline_tag: "text-to-image",
    library_name: "diffusers",
    tags: ["diffusers", "license:creativeml-openrail-m"],
    cardData: { license: "creativeml-openrail-m" },
    gguf: undefined,
    siblings: [
      lfs("tiny-diffusion-v1.safetensors", 4_270_000_000, "a"),
      lfs("tiny-diffusion-v1-fp16.safetensors", 2_130_000_000, "b"),
      lfs("tiny-diffusion-v1.ckpt", 4_270_000_000, "c"),
      lfs("vae/diffusion_pytorch_model.safetensors", 335_000_000, "d"),
      lfs("unet/diffusion_pytorch_model.safetensors", 3_440_000_000, "e"),
      lfs("tiny-vae-ft-mse.safetensors", 800_000_000, "f"),
    ],
    ...overrides,
  });
}

test("classifies speech and image repositories", () => {
  expect(
    likelyKind("automatic-speech-recognition", [], {
      repo: "ggerganov/whisper.cpp",
    }).kind,
  ).toBe("stt");
  expect(
    likelyKind("automatic-speech-recognition", [], {
      repo: "openai/whisper-large-v3",
      library: "transformers",
    }),
  ).toMatchObject({ kind: null, label: expect.stringMatching(/ggml/) });
  expect(
    likelyKind("automatic-speech-recognition", [], {
      repo: "nvidia/parakeet-tdt",
    }).kind,
  ).toBeNull();
  expect(
    likelyKind("text-to-image", [], {
      repo: "stable-diffusion-v1-5/stable-diffusion-v1-5",
      library: "diffusers",
    }).kind,
  ).toBe("image");
  expect(
    likelyKind(undefined, [], {
      repo: "Org/some-checkpoint",
      library: "diffusion-single-file",
    }).kind,
  ).toBe("image");
  for (const [repo, label] of [
    ["black-forest-labs/FLUX.1-dev", "image (FLUX.1 pipeline)"],
    ["Tongyi-MAI/Z-Image-Turbo", "image (Z-Image-Turbo pipeline)"],
    ["Org/FLUX.2-klein-9B-GGUF", "image (FLUX.2 klein 9B pipeline)"],
  ] as const) {
    expect(likelyKind("text-to-image", [], { repo })).toMatchObject({
      kind: "image",
      label,
    });
  }
  for (const [repo, reason] of [
    ["Qwen/Qwen-Image", /Qwen2\.5-VL/],
    ["stabilityai/stable-diffusion-3.5-large", /SD3|CLIP-G/],
    ["Wan-AI/Wan2.1-T2V-1.3B", /video/],
    ["black-forest-labs/FLUX.1-Kontext-dev", /edit/i],
    ["black-forest-labs/FLUX.2-dev", /Mistral/],
  ] as const) {
    const likely = likelyKind("text-to-image", [], { repo });
    expect(likely.kind).toBeNull();
    expect(likely.label).toMatch(reason);
  }
  // Fine-tunes inherit the pipeline from base_model tags.
  expect(
    likelyKind(
      "text-to-image",
      ["base_model:finetune:black-forest-labs/FLUX.1-dev"],
      {
        repo: "Artist/painterly-v2",
      },
    ).pipeline,
  ).toBe("flux1");
  // Image-to-image and untagged GGUF repositories of a known pipeline count.
  expect(
    likelyKind("image-to-image", [], { repo: "unsloth/FLUX.2-klein-4B-GGUF" })
      .pipeline,
  ).toBe("flux2-klein-4b");
  expect(
    likelyKind(undefined, ["gguf"], {
      repo: "leejet/FLUX.1-schnell-gguf",
      library: "gguf",
    }).kind,
  ).toBe("image");
  expect(
    likelyKind("text-to-image", ["lora"], { repo: "Org/style-lora" }).kind,
  ).toBeNull();
  expect(likelyKind("text-to-speech", []).kind).toBeNull();
  expect(isGated("auto")).toBe(true);
  expect(isGated(false)).toBe(false);
  expect(isGated(undefined)).toBe(false);
  expect(modelIdFor("ggml-base.en-q8_0.bin", "stt")).toBe(
    "whisper-base.en-q8_0",
  );
  expect(modelIdFor("sd_xl_base_1.0.safetensors", "image")).toBe(
    "sd_xl_base_1.0",
  );
});

test("generates whisper.cpp entries from ggml files", async () => {
  const info = await parseInfo(whisperInfo());
  const choices = entryChoices(info);
  expect(choices.kind).toBe("stt");
  expect(choices.choices.map(({ path }) => path)).toEqual([
    "ggml-base.bin",
    "ggml-base-q8_0.bin",
    "ggml-base.en-q8_0.bin",
    "ggml-small-q8_0.bin",
  ]);
  expect(choices.default?.path).toBe("ggml-base-q8_0.bin");

  const entry = generateEntry(
    info,
    parseHfReference("hf:ggerganov/whisper.cpp:ggml-base.en-q8_0.bin"),
  );
  const spec = modelSpecSchema.parse(entry.spec);
  expect(spec).toMatchObject({
    modelId: "whisper-base.en-q8_0",
    kind: "stt",
    family: "Whisper",
    size: "base",
    quant: "Q8_0",
    inputModalities: ["audio"],
    outputModalities: ["text"],
    repositoryRevision: SHA,
    commercialStatus: "open",
  });
  expect(spec.capabilities.map(({ id }) => id)).toEqual(["speech-to-text"]);
  expect(spec.artifacts[0]).toMatchObject({
    sourcePath: "ggml-base.en-q8_0.bin",
    sha256: "c".repeat(64),
  });
  const small = modelSpecSchema.parse(
    generateEntry(
      info,
      parseHfReference("hf:ggerganov/whisper.cpp:whisper-small-q8_0"),
    ).spec,
  );
  expect(small.capabilities.map(({ id }) => id)).toEqual([
    "speech-to-text",
    "multilingual",
  ]);
  expect(small.size).toBe("small");

  const transformers = await parseInfo(
    whisperInfo({ id: "openai/whisper-tiny", library_name: "transformers" }),
  );
  expect(() =>
    generateEntry(transformers, parseHfReference("hf:openai/whisper-tiny")),
  ).toThrow(/whisper.cpp needs ggml files/);
  const empty = await parseInfo(
    whisperInfo({ siblings: [lfs("model.safetensors", 10, "a")] }),
  );
  expect(() =>
    generateEntry(empty, parseHfReference("hf:ggerganov/whisper.cpp")),
  ).toThrow(/no whisper.cpp ggml/);
});

test("generates single-file Stable Diffusion entries", async () => {
  const info = await parseInfo(imageInfo());
  const choices = entryChoices(info);
  expect(choices.kind).toBe("image");
  // Diffusers folders, VAEs and anything below 500 MB are excluded.
  expect(choices.choices.map(({ path }) => path).sort()).toEqual([
    "tiny-diffusion-v1-fp16.safetensors",
    "tiny-diffusion-v1.ckpt",
    "tiny-diffusion-v1.safetensors",
  ]);
  expect(choices.default?.path).toBe("tiny-diffusion-v1-fp16.safetensors");

  const entry = generateEntry(
    info,
    parseHfReference("hf:TinyArt/tiny-diffusion-v1"),
  );
  const spec = modelSpecSchema.parse(entry.spec);
  expect(spec).toMatchObject({
    modelId: "tiny-diffusion-v1-fp16",
    kind: "image",
    quant: "F16",
    size: "2.1GB",
    storageGb: 2.13,
    inputModalities: ["text"],
    outputModalities: ["image"],
    imageLaunchOptions: { diffusionFlashAttention: false },
    contextWindowTokens: null,
  });
  expect(spec.artifacts).toHaveLength(1);
  expect(spec.capabilities.map(({ id }) => id)).toEqual([
    "text-to-image",
    "image-to-image",
  ]);

  const gguf = await parseInfo(
    imageInfo({
      siblings: [
        lfs("tiny-diffusion-Q4_0.gguf", 1_100_000_000, "a"),
        lfs("tiny-diffusion-Q8_0.gguf", 1_800_000_000, "b"),
      ],
    }),
  );
  expect(entryChoices(gguf).default?.path).toBe("tiny-diffusion-Q8_0.gguf");
  const diffusersOnly = await parseInfo(
    imageInfo({
      siblings: [
        lfs("unet/diffusion_pytorch_model.safetensors", 3_440_000_000, "e"),
      ],
    }),
  );
  expect(() =>
    generateEntry(diffusersOnly, parseHfReference("hf:TinyArt/x")),
  ).toThrow(/single-file Stable Diffusion checkpoint/);
});

test("searches speech and image repositories with the token", async () => {
  const { fetch, calls } = fakeFetch(() => ({
    body: [
      {
        id: "ggerganov/whisper.cpp",
        downloads: 100,
        pipeline_tag: "automatic-speech-recognition",
        tags: ["license:mit"],
      },
      {
        id: "openai/whisper-large-v3",
        downloads: 50,
        pipeline_tag: "automatic-speech-recognition",
        library_name: "transformers",
        gated: "auto",
        tags: [],
      },
    ],
  }));
  const root = scratchRoot();
  const { hits } = await searchHub(
    { root, fetch, kind: "stt", token: "hf_test_token" },
    "",
  );
  const url = new URL(calls[0]!.url);
  expect(url.searchParams.get("pipeline_tag")).toBe(
    "automatic-speech-recognition",
  );
  expect(url.searchParams.get("search")).toBe("whisper");
  expect(url.searchParams.get("filter")).toBeNull();
  expect(
    calls[0]!.headers.Authorization ?? calls[0]!.headers.authorization,
  ).toBe("Bearer hf_test_token");
  expect(hits.map((hit) => [hit.likely.kind, hit.gated])).toEqual([
    ["stt", false],
    [null, true],
  ]);

  await searchHub({ root, fetch, kind: "image" }, "anime gguf");
  const image = new URL(calls[1]!.url);
  expect(image.searchParams.get("pipeline_tag")).toBe("text-to-image");
  // Only language-model searches drop the word "gguf".
  expect(image.searchParams.get("search")).toBe("anime gguf");
});

test("models search passes the saved token and shows gated repositories", async () => {
  const lines: string[] = [];
  const { fetch, calls } = fakeFetch(() => ({
    body: [
      {
        id: "TinyOrg/Gated-GGUF",
        downloads: 10,
        pipeline_tag: "text-generation",
        gated: "manual",
        tags: ["gguf"],
      },
    ],
  }));
  const ctx = context(scratchRoot());
  (ctx.config as { hfToken: string }).hfToken = "hf_saved_token";
  const result = await runSearch(
    { query: "gated", limit: 5 },
    ctx,
    execution(lines),
    { fetch },
  );
  expect(
    calls[0]!.headers.Authorization ?? calls[0]!.headers.authorization,
  ).toBe("Bearer hf_saved_token");
  expect(lines[1]).toMatch(/\syes\s+llm$/);
  expect(lines.join("\n")).toContain("Gated repositories need");
  expect(lines.join("\n")).not.toContain("hf_saved_token");
  expect(result.data).toMatchObject({
    kind: "llm",
    results: [{ gated: true, kind: "llm" }],
  });
});

test("generates multi-file pipeline entries from the measured profiles", async () => {
  const dev = await parseInfo(
    imageInfo({
      id: "black-forest-labs/FLUX.1-dev",
      library_name: "diffusers",
      siblings: [
        { rfilename: "model_index.json", size: 500 },
        lfs("flux1-dev.safetensors", 23_800_000_000, "a"),
        lfs("ae.safetensors", 335_000_000, "b"),
        lfs(
          "transformer/diffusion_pytorch_model-00001-of-00003.safetensors",
          9_900_000_000,
          "c",
        ),
        lfs(
          "text_encoder_2/model-00001-of-00002.safetensors",
          4_990_000_000,
          "d",
        ),
      ],
    }),
  );
  const choices = entryChoices(dev);
  expect(choices.pipeline).toBe("flux1");
  expect(choices.choices.map(({ path }) => path)).toEqual([
    "flux1-dev.safetensors",
  ]);
  const entry = generateEntry(
    dev,
    parseHfReference("hf:black-forest-labs/FLUX.1-dev"),
  );
  const spec = modelSpecSchema.parse(entry.spec);
  const template = byId("flux1-schnell-q4_0")!;
  expect(spec).toMatchObject({
    kind: "image",
    family: "FLUX.1",
    size: "12B",
    imageLaunchOptions: { diffusionFlashAttention: true },
    imageRuntime: {
      kind: "diffusion-flux1",
      artifacts: {
        diffusionModel: "flux1-dev.safetensors",
        vae: "flux1-ae.safetensors",
        t5xxl: "flux1-t5xxl_fp16.safetensors",
      },
      generation: { sampler: "euler", steps: 20, cfgScale: 1 },
    },
  });
  expect(spec.artifacts.map(({ filename }) => filename)).toEqual(
    template.artifacts.map(({ filename }, index) =>
      index === 0 ? "flux1-dev.safetensors" : filename,
    ),
  );
  // Every supplementary file is pinned to its own repository revision.
  for (const artifact of spec.artifacts.slice(1)) {
    expect(artifact.source?.revision).toMatch(/^[a-f0-9]{40}$/);
  }
  expect(spec.artifacts[0]).toMatchObject({
    sha256: "a".repeat(64),
    sourcePath: "flux1-dev.safetensors",
  });
  expect(spec.storageGb).toBeGreaterThan(23.8 + 10);

  const klein = await parseInfo(
    imageInfo({
      id: "unsloth/FLUX.2-klein-4B-GGUF",
      pipeline_tag: "image-to-image",
      library_name: "gguf",
      siblings: [
        lfs("flux-2-klein-4b-Q8_0.gguf", 4_300_000_000, "a"),
        lfs("flux-2-klein-4b-Q4_0.gguf", 2_500_000_000, "b"),
        lfs("flux-2-klein-4b-Q2_K.gguf", 1_600_000_000, "c"),
      ],
    }),
  );
  const kleinEntry = generateEntry(
    klein,
    parseHfReference("hf:unsloth/FLUX.2-klein-4B-GGUF"),
  );
  const kleinSpec = modelSpecSchema.parse(kleinEntry.spec);
  expect(kleinSpec).toMatchObject({
    family: "FLUX.2-klein",
    size: "4B",
    quant: "Q4_0",
    imageRuntime: {
      kind: "diffusion-qwen3",
      artifacts: {
        diffusionModel: "flux-2-klein-4b-q4_0-flux-2-klein-4b-Q4_0.gguf",
      },
      generation: { steps: 4 },
    },
  });
  expect(kleinSpec.capabilities.map(({ id }) => id)).toEqual(["text-to-image"]);

  // The same bytes as the built-in's file are shared, not downloaded twice.
  const builtIn = byId("flux2-klein-4b-q4_0")!.artifacts[0]!;
  const sameBytes = await parseInfo(
    imageInfo({
      id: "leejet/FLUX.2-klein-4B-GGUF",
      pipeline_tag: undefined,
      library_name: "gguf",
      tags: ["gguf"],
      siblings: [
        {
          rfilename: builtIn.sourcePath,
          size: builtIn.expectedSizeBytes,
          lfs: { sha256: builtIn.sha256, size: builtIn.expectedSizeBytes },
        },
      ],
    }),
  );
  expect(
    generateEntry(
      sameBytes,
      parseHfReference("hf:leejet/FLUX.2-klein-4B-GGUF"),
      { modelId: "klein-copy" },
    ).spec.artifacts,
  ).toContainEqual(expect.objectContaining({ filename: builtIn.filename }));

  const missing = await parseInfo(
    imageInfo({
      id: "Org/FLUX.1-dev-lora-pack",
      siblings: [lfs("tiny.safetensors", 100_000_000, "a")],
    }),
  );
  expect(() => entryChoices(missing)).toThrow(/no FLUX\.1 diffusion model/);
});

test("classifies image LoRAs by their declared base model", () => {
  const kleinTags = [
    "lora",
    "base_model:black-forest-labs/FLUX.2-klein-4B",
    "base_model:adapter:black-forest-labs/FLUX.2-klein-4B",
  ];
  expect(
    likelyKind("text-to-image", kleinTags, { repo: "Org/pixel-art-lora" }),
  ).toMatchObject({
    kind: "image",
    label: "image LoRA (FLUX.2 klein 4B)",
    lora: "flux2-klein-4b",
  });
  // A LoRA repository often has no pipeline tag at all.
  expect(
    likelyKind(undefined, kleinTags, { repo: "Org/pixel-art-lora" }).lora,
  ).toBe("flux2-klein-4b");
  expect(
    imageLoraBaseFor("Org/style", [
      "base_model:adapter:stabilityai/stable-diffusion-xl-base-1.0",
    ]),
  ).toEqual({ base: "sdxl" });
  expect(
    imageLoraBaseFor("Org/style", [
      "base_model:adapter:stable-diffusion-v1-5/stable-diffusion-v1-5",
    ]),
  ).toEqual({ base: "sd1.5" });
  expect(imageLoraBaseFor("Org/z-image-turbo-ink-lora", [])).toEqual({
    base: "z-image-turbo",
  });
  // Declared tags decide; the repository name is not consulted beside them.
  expect(
    imageLoraBaseFor("Org/sdxl-look-lora", ["base_model:Org/unknown-model"]),
  ).toBeNull();
  for (const [base, reason] of [
    ["black-forest-labs/FLUX.1-dev", /FLUX\.1 dev/],
    ["black-forest-labs/FLUX.2-klein-base-4B", /klein base/],
    ["black-forest-labs/FLUX.2-dev", /FLUX\.2 dev/],
    ["stabilityai/sdxl-turbo", /SDXL variant/],
  ] as const) {
    const likely = likelyKind("text-to-image", ["lora", `base_model:${base}`], {
      repo: "Org/style-lora",
    });
    expect(likely.kind).toBeNull();
    expect(likely.label).toMatch(reason);
  }
});

test("pins an image LoRA beside every file of its base entry", async () => {
  const info = await parseInfo(
    imageInfo({
      id: "Limbicnation/pixel-art-lora",
      pipeline_tag: "text-to-image",
      library_name: "diffusers",
      tags: [
        "diffusers",
        "lora",
        "base_model:black-forest-labs/FLUX.2-klein-4B",
        "base_model:adapter:black-forest-labs/FLUX.2-klein-4B",
        "license:apache-2.0",
      ],
      cardData: { license: "apache-2.0", instance_prompt: "pixel art sprite" },
      siblings: [
        { rfilename: "README.md", size: 10 },
        lfs("pytorch_lora_weights.safetensors", 325_280_664, "a"),
        lfs("pytorch_lora_weights.comfyui.safetensors", 325_000_000, "b"),
        lfs("samples/sprite.png", 1_000_000, "c"),
      ],
    }),
  );
  const choices = entryChoices(info);
  expect(choices.lora).toBe("flux2-klein-4b");
  expect(choices.pipeline).toBeNull();
  expect(choices.choices.map(({ path }) => path).sort()).toEqual([
    "pytorch_lora_weights.comfyui.safetensors",
    "pytorch_lora_weights.safetensors",
  ]);
  expect(choices.default?.path).toBe("pytorch_lora_weights.safetensors");

  const entry = generateEntry(
    info,
    parseHfReference("hf:Limbicnation/pixel-art-lora"),
  );
  const spec = modelSpecSchema.parse(entry.spec);
  const template = byId("flux2-klein-4b-q4_0")!;
  expect(spec.modelId).toBe("pixel-art-lora");
  expect(spec.imageRuntime).toEqual(template.imageRuntime);
  expect(spec.imageLaunchOptions?.lora).toEqual({
    artifactFilename: "pixel-art-lora-pytorch_lora_weights.safetensors",
    defaultStrength: 1,
    trigger: "pixel art sprite",
  });
  expect(spec.artifacts.map(({ filename }) => filename)).toEqual([
    ...template.artifacts.map(({ filename }) => filename),
    "pixel-art-lora-pytorch_lora_weights.safetensors",
  ]);
  expect(spec.artifacts.at(-1)).toMatchObject({
    role: "supplementary",
    sha256: "a".repeat(64),
    sourcePath: "pytorch_lora_weights.safetensors",
  });
  // The base files keep the template's own pinned sources, so they are shared.
  for (const artifact of spec.artifacts.slice(0, -1))
    expect(artifact.source?.revision).toMatch(/^[a-f0-9]{40}$/);
  expect(spec.storageGb).toBeGreaterThan(template.storageGb);

  const explicit = generateEntry(
    info,
    parseHfReference(
      "hf:Limbicnation/pixel-art-lora:pytorch_lora_weights.comfyui.safetensors",
    ),
  );
  expect(
    modelSpecSchema.parse(explicit.spec).imageLaunchOptions?.lora
      ?.artifactFilename,
  ).toBe("pixel-art-lora-pytorch_lora_weights.comfyui.safetensors");

  const empty = await parseInfo(
    imageInfo({
      id: "Org/empty-lora",
      tags: ["lora", "base_model:black-forest-labs/FLUX.2-klein-4B"],
      siblings: [lfs("weights/lora.safetensors", 1_000, "a")],
    }),
  );
  expect(() => entryChoices(empty)).toThrow(/no LoRA \.safetensors file/);
});

test("explains diffusers folder layouts sd.cpp cannot load", async () => {
  const diffusers = await parseInfo(
    imageInfo({
      siblings: [
        { rfilename: "model_index.json", size: 500 },
        lfs("unet/diffusion_pytorch_model.safetensors", 3_440_000_000, "e"),
      ],
    }),
  );
  expect(() => entryChoices(diffusers)).toThrow(/diffusers folder layout/);
});

test("launches v-prediction checkpoints with --prediction v", async () => {
  const byName = await parseInfo(
    imageInfo({
      id: "Laxhar/noobai-XL-Vpred-1.0",
      siblings: [lfs("NoobAI-XL-Vpred-v1.0.safetensors", 6_900_000_000, "a")],
    }),
  );
  const named = modelSpecSchema.parse(
    generateEntry(byName, parseHfReference("hf:Laxhar/noobai-XL-Vpred-1.0"))
      .spec,
  );
  expect(named.imageLaunchOptions).toEqual({
    diffusionFlashAttention: false,
    prediction: "v",
  });
  expect(named.notes).toMatch(/--prediction v/);

  // An SD 2.1 repository found by its scheduler config.
  const body = imageInfo({
    id: "Org/sd-two-one",
    siblings: [
      { rfilename: "scheduler/scheduler_config.json", size: 400 },
      lfs("v2-1.safetensors", 5_200_000_000, "a"),
    ],
  });
  const { fetch, calls } = fakeFetch((url) =>
    url.endsWith("scheduler/scheduler_config.json")
      ? { body: { prediction_type: "v_prediction" } }
      : { body },
  );
  const info = await fetchModelInfo(
    { root: scratchRoot(false), fetch },
    "Org/sd-two-one",
  );
  expect(info.schedulerPrediction).toBe("v");
  expect(calls.at(-1)!.url).toBe(
    `https://huggingface.co/Org/sd-two-one/resolve/${SHA}/scheduler/scheduler_config.json`,
  );
  const spec = modelSpecSchema.parse(
    generateEntry(info, parseHfReference("hf:Org/sd-two-one")).spec,
  );
  expect(spec.imageLaunchOptions?.prediction).toBe("v");

  // Epsilon checkpoints stay unchanged, and a failed config read is ignored.
  const eps = await fetchModelInfo(
    {
      root: scratchRoot(false),
      fetch: fakeFetch((url) =>
        url.endsWith("scheduler_config.json") ? { status: 404 } : { body },
      ).fetch,
    },
    "Org/sd-two-one",
  );
  expect(eps.schedulerPrediction).toBeUndefined();
  expect(
    modelSpecSchema.parse(
      generateEntry(eps, parseHfReference("hf:Org/sd-two-one")).spec,
    ).imageLaunchOptions,
  ).toEqual({ diffusionFlashAttention: false });
});

test("image searches add launchable image-to-image and GGUF pipelines", async () => {
  const pages: Record<string, unknown[]> = {
    "text-to-image": [
      {
        id: "stabilityai/sdxl-turbo",
        downloads: 500,
        pipeline_tag: "text-to-image",
        library_name: "diffusers",
        tags: [],
      },
    ],
    "image-to-image": [
      {
        id: "unsloth/FLUX.2-klein-4B-GGUF",
        downloads: 900,
        pipeline_tag: "image-to-image",
        library_name: "gguf",
        tags: ["gguf"],
      },
      {
        id: "Org/upscaler",
        downloads: 2000,
        pipeline_tag: "image-to-image",
        tags: [],
      },
    ],
    gguf: [
      {
        id: "leejet/FLUX.1-schnell-gguf",
        downloads: 100,
        tags: ["gguf"],
        library_name: "gguf",
      },
      {
        id: "TinyOrg/Flux-Chat-7B-GGUF",
        downloads: 5000,
        pipeline_tag: "text-generation",
        tags: ["gguf"],
      },
      {
        id: "unsloth/FLUX.2-klein-4B-GGUF",
        downloads: 900,
        pipeline_tag: "image-to-image",
        library_name: "gguf",
        tags: ["gguf"],
      },
    ],
  };
  const { fetch, calls } = fakeFetch((url) => {
    const params = new URL(url).searchParams;
    return { body: pages[params.get("pipeline_tag") ?? params.get("filter")!] };
  });
  const { hits } = await searchHub(
    { root: scratchRoot(), fetch, kind: "image" },
    "flux",
  );
  expect(calls).toHaveLength(3);
  expect(hits.map(({ repo }) => repo)).toEqual([
    "unsloth/FLUX.2-klein-4B-GGUF",
    "stabilityai/sdxl-turbo",
    "leejet/FLUX.1-schnell-gguf",
  ]);

  // An empty query skips the unfiltered GGUF search; a failing extra page
  // does not fail the search.
  const failing = fakeFetch((url) =>
    new URL(url).searchParams.get("pipeline_tag") === "image-to-image"
      ? { status: 500 }
      : { body: pages["text-to-image"] },
  );
  const quiet = await searchHub(
    { root: scratchRoot(), fetch: failing.fetch, kind: "image" },
    "",
  );
  expect(failing.calls).toHaveLength(2);
  expect(quiet.hits.map(({ repo }) => repo)).toEqual([
    "stabilityai/sdxl-turbo",
  ]);
});

test("detects decision models from Hub tags and GGUF architecture", async () => {
  expect(isDecisionRepository(["gguf", "SystemOne"])).toBe(true);
  expect(isDecisionRepository(["decision-model"])).toBe(true);
  expect(isDecisionRepository(["gguf"], "clef")).toBe(true);
  expect(isDecisionRepository(["gguf"], "qwen35")).toBe(false);
  expect(likelyKind("image-text-to-text", ["gguf", "decision-model"])).toEqual({
    kind: "llm",
    label: "decision",
  });
  expect(likelyKind("text-to-image", ["decision-model"]).label).not.toBe(
    "decision",
  );

  const info = await parseInfo(
    modelInfo({
      id: "ggml-org/Clef-Flash-GGUF",
      pipeline_tag: "image-text-to-text",
      tags: ["gguf", "clef", "systemone", "decision-model"],
      gguf: {
        architecture: "clef",
        context_length: 262144,
        chat_template: "{% if tools %}{{ tools }}{% endif %}<think>",
      },
      siblings: [
        lfs("Clef-Flash-Q4_K_M.gguf", 6_486_448_288, "a"),
        lfs("mmproj-Clef-Flash-Q8_0.gguf", 624_229_728, "b"),
      ],
    }),
  );
  const entry = generateEntry(
    info,
    parseHfReference("hf:ggml-org/Clef-Flash-GGUF"),
  );
  expect(entry.detected).toEqual(["decision"]);
  const spec = modelSpecSchema.parse(entry.spec);
  expect(spec.llmRuntime).toEqual({ capability: "decision-only" });
  expect(spec.capabilities.map(({ id }) => id)).toEqual(["decision"]);
  expect(spec.inputModalities).toEqual(["text", "image"]);
});
