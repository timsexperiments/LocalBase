import { afterEach, expect, test } from "bun:test";
import {
  mkdtempSync,
  readdirSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BUILT_IN_CATALOG, byId } from "../../catalog";
import {
  setFeedAnnotations,
  type FeedAnnotation,
} from "../../catalog/annotations";
import { applyCatalogLayers, catalogProvenance } from "../../catalog/sources";
import {
  addFromHub,
  CatalogConflictError,
  discoverModels,
  discoveryRequestSchema,
  catalogNotesFor,
  hubTokenFrom,
  previewRepository,
  runDiscoveryRequest,
  withFeedMarks,
  type DiscoveryDependencies,
} from "./discovery";
import type { HubFetch } from "./huggingface";

const SHA = "9217f5db79a29953eb74d5343926648285ec7e67";
const TOKEN = "hf_discovery_test_value";
const roots: string[] = [];

function scratchRoot(): string {
  const root = realpathSync(mkdtempSync(join(tmpdir(), "lb-discovery-")));
  writeFileSync(join(root, ".localbase-root.json"), "{}");
  roots.push(root);
  return root;
}

afterEach(() => {
  applyCatalogLayers([]);
  setFeedAnnotations([]);
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

function lfs(rfilename: string, size: number, fill: string) {
  return { rfilename, size, lfs: { sha256: fill.repeat(64), size } };
}

const llmRepo = {
  id: "TinyOrg/Tiny-0.5B-Instruct-GGUF",
  sha: SHA,
  pipeline_tag: "text-generation",
  tags: ["gguf", "license:apache-2.0"],
  cardData: { license: "apache-2.0" },
  gguf: { architecture: "qwen2", context_length: 8192 },
  siblings: [
    lfs("tiny-0.5b-instruct-q4_k_m.gguf", 400_000_000, "a"),
    lfs("tiny-0.5b-instruct-q8_0.gguf", 600_000_000, "b"),
  ],
};

const whisperRepo = {
  id: "ggerganov/whisper.cpp",
  sha: SHA,
  pipeline_tag: "automatic-speech-recognition",
  tags: ["license:mit"],
  cardData: { license: "mit" },
  siblings: [
    lfs("ggml-base-q8_0.bin", 82_000_000, "b"),
    lfs("ggml-small.bin", 466_000_000, "d"),
  ],
};

const searchResults = [
  {
    id: llmRepo.id,
    downloads: 1200,
    likes: 4,
    pipeline_tag: "text-generation",
    tags: ["gguf", "license:apache-2.0"],
    gated: false,
  },
  {
    id: "GatedOrg/Gated-GGUF",
    downloads: 900,
    pipeline_tag: "text-generation",
    tags: ["gguf"],
    gated: "manual",
  },
  {
    id: "EmbedOrg/tiny-embed-GGUF",
    downloads: 10,
    pipeline_tag: "feature-extraction",
    tags: ["gguf"],
  },
];

type Call = { url: string; headers: Record<string, string> };

function hub(): { fetch: HubFetch; calls: Call[] } {
  const calls: Call[] = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({
        url,
        headers: (init?.headers ?? {}) as Record<string, string>,
      });
      const path = new URL(url).pathname;
      const respond = (status: number, body: unknown = {}) => ({
        ok: status === 200,
        status,
        json: async () => body,
      });
      if (path === "/api/models") return respond(200, searchResults);
      if (path === `/api/models/${llmRepo.id}`) return respond(200, llmRepo);
      if (path === `/api/models/${whisperRepo.id}`)
        return respond(200, whisperRepo);
      if (path === "/api/models/GatedOrg/Gated-GGUF") return respond(403);
      return respond(404);
    },
  };
}

test("uses the saved token before HF_TOKEN", () => {
  const previous = process.env.HF_TOKEN;
  process.env.HF_TOKEN = "from-env";
  try {
    expect(hubTokenFrom({ hfToken: "saved" })).toBe("saved");
    expect(hubTokenFrom({ hfToken: "" })).toBe("from-env");
    delete process.env.HF_TOKEN;
    expect(hubTokenFrom({ hfToken: "" })).toBeUndefined();
  } finally {
    if (previous === undefined) delete process.env.HF_TOKEN;
    else process.env.HF_TOKEN = previous;
  }
});

test("previews addable search results with memory fit and gated state", async () => {
  const { fetch, calls } = hub();
  const { hits } = await discoverModels(
    {
      root: scratchRoot(),
      fetch,
      token: TOKEN,
      kind: "llm",
      systemVramGb: 16,
    },
    "tiny",
  );
  expect(hits.map((hit) => hit.repo)).toEqual([
    llmRepo.id,
    "GatedOrg/Gated-GGUF",
    "EmbedOrg/tiny-embed-GGUF",
  ]);
  const [tiny, gated, embed] = hits;
  expect(tiny!.url).toBe(`https://huggingface.co/${llmRepo.id}`);
  expect(tiny!.preview?.kind).toBe("llm");
  expect(tiny!.preview?.defaultPath).toBe("tiny-0.5b-instruct-q4_k_m.gguf");
  expect(tiny!.preview?.choices.map((choice) => choice.modelId)).toEqual([
    "tiny-0.5b-instruct-q4_k_m",
    "tiny-0.5b-instruct-q8_0",
  ]);
  expect(tiny!.preview?.choices[0]!.fit?.verdict).toBe("fits");
  expect(tiny!.preview?.choices[0]!.sizeBytes).toBe(400_000_000);
  expect(gated!.gated).toBe(true);
  expect(gated!.preview).toBeNull();
  expect(gated!.reason).toMatch(/refused access \(403\)/);
  expect(gated!.reasonCode).toBe("hub_auth_required");
  expect(tiny!.reasonCode).toBeUndefined();
  expect(embed!.preview).toBeNull();
  expect(embed!.reason).toMatch(/embeddings/);
  // Unsupported results are never fetched; the token goes only to huggingface.co.
  expect(calls.some((call) => call.url.includes("tiny-embed"))).toBe(false);
  for (const call of calls) {
    expect(new URL(call.url).origin).toBe("https://huggingface.co");
    expect(call.headers.authorization).toBe(`Bearer ${TOKEN}`);
  }
});

test("skips built-in ids when picking the default file", async () => {
  const { fetch } = hub();
  const preview = await previewRepository(
    { root: scratchRoot(), fetch, systemVramGb: 16 },
    whisperRepo.id,
  );
  expect(preview.kind).toBe("stt");
  const base = preview.choices.find(
    (choice) => choice.path === "ggml-base-q8_0.bin",
  );
  expect(base?.existing).toBe("built-in");
  expect(base?.modelId).toBe("whisper-base-q8_0");
  expect(preview.defaultPath).not.toBe("ggml-base-q8_0.bin");
});

test("adds a pinned user entry and refuses ids another tier owns", async () => {
  const root = scratchRoot();
  const { fetch } = hub();
  const added = await addFromHub(
    { root, fetch },
    { repo: llmRepo.id, selector: "q8_0" },
  );
  expect(added.model.modelId).toBe("tiny-0.5b-instruct-q8_0");
  expect(added.replaced).toBe(false);
  expect(added.entry.revision).toBe(SHA);
  expect(added.entry.weights[0]!.sha256).toBe("b".repeat(64));
  expect(readdirSync(join(root, "catalog.d"))).toHaveLength(1);
  expect(catalogProvenance("tiny-0.5b-instruct-q8_0").origin).toBe("user");

  const again = await addFromHub(
    { root, fetch },
    { repo: llmRepo.id, selector: "q8_0" },
  );
  expect(again.replaced).toBe(true);

  await expect(
    addFromHub(
      { root, fetch },
      { repo: whisperRepo.id, selector: "ggml-base-q8_0.bin" },
    ),
  ).rejects.toBeInstanceOf(CatalogConflictError);
  await expect(
    addFromHub(
      { root, fetch },
      { repo: llmRepo.id, modelId: "whisper-base-q8_0" },
    ),
  ).rejects.toThrow(/already a built-in catalog model/);
  expect(byId("whisper-base-q8_0")?.kind).toBe("stt");
});

function deps(
  root: string,
  fetch: HubFetch,
  installs: string[],
): DiscoveryDependencies {
  return {
    root,
    fetch,
    token: TOKEN,
    systemVramGb: 16,
    refresh: async () => {},
    install: async (modelId) => {
      installs.push(modelId);
      return { action: "install", state: "running" };
    },
  };
}

test("runs gateway discovery requests without exposing the token", async () => {
  const root = scratchRoot();
  const { fetch } = hub();
  const installs: string[] = [];
  const run = (body: unknown) =>
    runDiscoveryRequest(
      deps(root, fetch, installs),
      discoveryRequestSchema.parse(body),
    );

  const search = await run({ action: "search", query: "tiny" });
  expect(search.status).toBe(200);
  expect(JSON.stringify(search.body)).not.toContain(TOKEN);

  const preview = await run({ action: "preview", repo: whisperRepo.id });
  expect(preview.status).toBe(200);

  const added = await run({
    action: "add",
    repo: llmRepo.id,
    selector: "tiny-0.5b-instruct-q4_k_m.gguf",
    install: true,
  });
  expect(added.status).toBe(200);
  expect(added.body).toMatchObject({
    modelId: "tiny-0.5b-instruct-q4_k_m",
    kind: "llm",
    operation: { state: "running" },
  });
  expect(installs).toEqual(["tiny-0.5b-instruct-q4_k_m"]);

  const conflict = await run({
    action: "add",
    repo: whisperRepo.id,
    selector: "ggml-base-q8_0.bin",
  });
  expect(conflict.status).toBe(409);

  const gated = await run({ action: "preview", repo: "GatedOrg/Gated-GGUF" });
  expect(gated.status).toBe(400);
  expect(JSON.stringify(gated.body)).not.toContain(TOKEN);

  const invalid = await run({ action: "preview", repo: "../etc/passwd" });
  expect(invalid.status).toBe(400);

  expect(() =>
    discoveryRequestSchema.parse({ action: "search", kind: "tts" }),
  ).toThrow();
});

test("reports Hugging Face outages as upstream failures", async () => {
  const outcome = await runDiscoveryRequest(
    {
      ...deps(
        scratchRoot(),
        async () => {
          throw new Error("offline");
        },
        [],
      ),
    },
    { action: "search", query: "tiny", kind: "llm" },
  );
  expect(outcome.status).toBe(502);
});

const toolMark = {
  id: "tool-calling",
  supported: false,
  source: "declared",
  verified: true,
  reason: "The chat template has no tool-call format.",
} as const;

function tinyAnnotation(
  overrides: Partial<FeedAnnotation> = {},
): FeedAnnotation {
  return {
    repo: llmRepo.id,
    revision: SHA,
    file: "tiny-0.5b-instruct-q8_0.gguf",
    status: "verified",
    unsupportedCapabilities: [toolMark],
    ...overrides,
  };
}

test("previews carry the feed annotation for each file", async () => {
  setFeedAnnotations([tinyAnnotation()]);
  const { fetch } = hub();
  const { hits } = await discoverModels(
    { root: scratchRoot(), fetch, kind: "llm", systemVramGb: 16 },
    "tiny",
  );
  const tiny = hits.find((hit) => hit.repo === llmRepo.id)!;
  const [q4, q8] = tiny.preview!.choices;
  expect(q4!.annotation).toBeNull();
  expect(q8!.annotation).toMatchObject({
    status: "verified",
    revisionMatch: true,
    unsupportedCapabilities: [{ id: "tool-calling", applied: true }],
  });
  expect(tiny.catalog).toHaveLength(1);
  expect(tiny.catalog[0]).toMatchObject({ revisionMatch: true });
  expect(hits.find((hit) => hit.repo !== llmRepo.id)!.catalog).toEqual([]);
});

test("unpreviewed results compare feed notes with the search sha", async () => {
  setFeedAnnotations([tinyAnnotation()]);
  const withSha = (sha: string) =>
    (async (url: string) => {
      const body = searchResults.map((hit) =>
        hit.id === llmRepo.id ? { ...hit, sha } : hit,
      );
      return {
        ok: new URL(url).pathname === "/api/models",
        status: 200,
        json: async () => body,
      };
    }) as HubFetch;
  const search = async (sha: string) =>
    (
      await discoverModels(
        {
          root: scratchRoot(),
          fetch: withSha(sha),
          kind: "llm",
          systemVramGb: 16,
          previewLimit: 0,
        },
        "tiny",
      )
    ).hits.find((hit) => hit.repo === llmRepo.id)!;
  const current = await search(SHA);
  expect(current.preview).toBeNull();
  expect(current.sha).toBe(SHA);
  expect(current.catalog[0]).toMatchObject({ revisionMatch: true });
  const moved = await search("1".repeat(40));
  expect(moved.catalog[0]).toMatchObject({ revisionMatch: false });
});

test("rate limits and refusals keep a code the UI can act on", async () => {
  const outcome = (status: number) =>
    runDiscoveryRequest(
      deps(
        scratchRoot(),
        async () => ({ ok: false, status, json: async () => ({}) }),
        [],
      ),
      { action: "search", query: "tiny", kind: "llm" },
    );
  expect(await outcome(429)).toMatchObject({
    status: 502,
    body: { error: { code: "hub_rate_limited" } },
  });
  expect(await outcome(401)).toMatchObject({
    status: 400,
    body: { error: { code: "hub_auth_required" } },
  });
  expect(await outcome(500)).toMatchObject({
    status: 502,
    body: { error: { code: "hub_unavailable" } },
  });
});

test("an annotation for another revision is shown but not applied", async () => {
  setFeedAnnotations([tinyAnnotation({ revision: "0".repeat(40) })]);
  const { fetch } = hub();
  const preview = await previewRepository(
    { root: scratchRoot(), fetch, systemVramGb: 16 },
    llmRepo.id,
  );
  const q8 = preview.choices.find(
    (choice) => choice.path === "tiny-0.5b-instruct-q8_0.gguf",
  )!;
  expect(q8.annotation).toMatchObject({
    revisionMatch: false,
    unsupportedCapabilities: [{ applied: false }],
  });

  const root = scratchRoot();
  const added = await addFromHub(
    { root, fetch },
    { repo: llmRepo.id, selector: "q8_0" },
  );
  expect(added.model.unsupportedCapabilities ?? []).toEqual([]);
});

test("adding a file writes the feed's marks into the new entry", async () => {
  setFeedAnnotations([tinyAnnotation()]);
  const { fetch } = hub();
  const root = scratchRoot();
  const added = await addFromHub(
    { root, fetch },
    { repo: llmRepo.id, selector: "q8_0" },
  );
  expect(added.model.unsupportedCapabilities).toEqual([toolMark]);
  expect(added.model.capabilities.some(({ id }) => id === "tool-calling")).toBe(
    false,
  );
});

test("withFeedMarks removes marked ids from the declared capabilities", () => {
  const spec = {
    capabilities: ["text-generation", { id: "tool-calling" }],
  };
  expect(withFeedMarks(spec, [])).toBe(spec);
  expect(withFeedMarks(spec, [toolMark])).toEqual({
    capabilities: ["text-generation"],
    unsupportedCapabilities: [toolMark],
  });
});

const multiFile = BUILT_IN_CATALOG.find(
  (model) =>
    model.artifacts.length > 1 &&
    model.source.startsWith("https://huggingface.co/"),
)!;

function recipeAnnotation(): FeedAnnotation {
  const repo = new URL(multiFile.source).pathname.slice(1);
  return {
    repo,
    revision: multiFile.repositoryRevision.toLowerCase(),
    modelId: multiFile.modelId,
    status: "qualified",
    recipe: {
      modelId: multiFile.modelId,
      totalBytes: 30,
      files: [
        {
          repositoryUrl: multiFile.source,
          revision: multiFile.repositoryRevision.toLowerCase(),
          path: "a.gguf",
          sizeBytes: 10,
          sha256: "a".repeat(64),
          role: "primary",
        },
        {
          repositoryUrl: multiFile.source,
          revision: multiFile.repositoryRevision.toLowerCase(),
          path: "b.gguf",
          sizeBytes: 20,
          sha256: "b".repeat(64),
          role: "supplementary",
        },
      ],
    },
  };
}

test("search results list recipes for catalog entries first", () => {
  const recipe = recipeAnnotation();
  setFeedAnnotations([{ repo: recipe.repo, revision: "1".repeat(40) }, recipe]);
  const notes = catalogNotesFor(recipe.repo);
  expect(notes[0]).toMatchObject({
    modelId: multiFile.modelId,
    displayName: multiFile.displayName,
    installable: true,
    recipe: { fileCount: 2, totalBytes: 30 },
  });
  expect(notes[1]!.installable).toBe(false);
});

test("install-recipe installs a catalog entry the feed has a recipe for", async () => {
  const { fetch } = hub();
  const installs: string[] = [];
  const run = (body: unknown) =>
    runDiscoveryRequest(
      deps(scratchRoot(), fetch, installs),
      discoveryRequestSchema.parse(body),
    );
  const missing = await run({ action: "install-recipe", modelId: "nope" });
  expect(missing.status).toBe(404);
  const noRecipe = await run({
    action: "install-recipe",
    modelId: multiFile.modelId,
  });
  expect(noRecipe.status).toBe(400);
  setFeedAnnotations([recipeAnnotation()]);
  const installed = await run({
    action: "install-recipe",
    modelId: multiFile.modelId,
  });
  expect(installed.status).toBe(200);
  expect(installed.body).toMatchObject({
    modelId: multiFile.modelId,
    operation: { state: "running" },
  });
  expect(installs).toEqual([multiFile.modelId]);
});

test("pipeline previews count the shared encoders in the download size", async () => {
  const fluxRepo = {
    id: "city96/FLUX.1-dev-gguf",
    sha: SHA,
    pipeline_tag: "text-to-image",
    library_name: "gguf",
    tags: ["gguf", "base_model:quantized:black-forest-labs/FLUX.1-dev"],
    cardData: { license: "other" },
    siblings: [
      lfs("flux1-dev-Q4_0.gguf", 6_790_000_000, "a"),
      lfs("flux1-dev-Q8_0.gguf", 12_700_000_000, "b"),
    ],
  };
  const fetch: HubFetch = async (url) => {
    const path = new URL(url).pathname;
    const found = path === `/api/models/${fluxRepo.id}`;
    return {
      ok: found,
      status: found ? 200 : 404,
      json: async () => (found ? fluxRepo : {}),
    };
  };
  const preview = await previewRepository(
    { root: scratchRoot(), fetch, systemVramGb: 64 },
    fluxRepo.id,
  );
  expect(preview.kind).toBe("image");
  expect(preview.defaultPath).toBe("flux1-dev-Q4_0.gguf");
  const q4 = preview.choices.find(
    (choice) => choice.path === "flux1-dev-Q4_0.gguf",
  )!;
  const template = byId("flux1-schnell-q4_0")!;
  const encoders = template.artifacts
    .filter((artifact) => artifact.role === "supplementary")
    .reduce((sum, artifact) => sum + (artifact.expectedSizeBytes ?? 0), 0);
  expect(encoders).toBeGreaterThan(0);
  expect(q4.sizeBytes).toBe(6_790_000_000 + encoders);
});

test("preview presents complete shard sets once with total size and part count", async () => {
  const repo = {
    ...llmRepo,
    siblings: [
      lfs("BF16/model-BF16-00001-of-00002.gguf", 3_000_000_000, "a"),
      lfs("BF16/model-BF16-00002-of-00002.gguf", 2_000_000_000, "b"),
      lfs("Q4/model-Q4_K_M.gguf", 1_000_000_000, "c"),
      lfs("MTP/mtp-model-Q4_0.gguf", 100_000_000, "d"),
    ],
  };
  const preview = await previewRepository(
    {
      root: scratchRoot(),
      systemVramGb: 16,
      fetch: async () => ({ ok: true, status: 200, json: async () => repo }),
    },
    repo.id,
  );
  expect(preview.choices).toHaveLength(2);
  expect(preview.choices[0]).toMatchObject({
    sizeBytes: 5_000_000_000,
    partCount: 2,
    quant: "BF16",
  });
  expect(preview.choices[1]).toMatchObject({
    sizeBytes: 1_000_000_000,
    partCount: 1,
  });
});
