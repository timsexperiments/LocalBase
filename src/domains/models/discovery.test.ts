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
import { byId } from "../../catalog";
import { applyCatalogLayers, catalogProvenance } from "../../catalog/sources";
import {
  addFromHub,
  CatalogConflictError,
  discoverModels,
  discoveryRequestSchema,
  hubTokenFrom,
  previewRepository,
  runDiscoveryRequest,
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
