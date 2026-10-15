import { describe, expect, test } from "bun:test";
import {
  catalogQueryFilters,
  catalogQueryParams,
  metadataCatalogRow,
  parseCatalogQuery,
  queryCatalog,
  type CatalogRow,
} from "./catalog-query";

const row = (overrides: Partial<CatalogRow> & { id: string }): CatalogRow => ({
  name: overrides.id,
  kind: "llm",
  capabilities: ["text-generation"],
  fit: "fits",
  installed: false,
  license: "open",
  storageGb: 1,
  memoryGb: 1,
  contextTokens: null,
  ...overrides,
});

const rows = [
  row({
    id: "qwen-coder",
    name: "Qwen Coder",
    text: ["Fast local coding model"],
    capabilities: ["text-generation", "code-generation", "tool-calling"],
    fit: "tight",
    installed: true,
    storageGb: 5,
    memoryGb: 9,
    contextTokens: 32768,
  }),
  row({
    id: "flux-schnell",
    name: "FLUX schnell",
    kind: "image",
    capabilities: ["text-to-image", "image-to-image"],
    fit: "wont-fit",
    license: "conditional",
    storageGb: 12,
    memoryGb: 24,
  }),
  row({
    id: "gemma",
    name: "Gemma",
    capabilities: ["text-generation", "tool-calling", "long-context"],
    fit: null,
    license: null,
    storageGb: 3,
    memoryGb: 6,
    contextTokens: 131072,
  }),
  row({
    id: "whisper",
    name: "Whisper",
    kind: "stt",
    capabilities: ["speech-to-text"],
    installed: true,
    storageGb: 0.5,
    memoryGb: 2,
  }),
];

const ids = (query: Parameters<typeof queryCatalog>[2]) =>
  queryCatalog(rows, (value) => value, query).map(({ id }) => id);

describe("catalog query filters", () => {
  test("an empty query keeps every model in input order", () => {
    expect(ids({})).toEqual(["qwen-coder", "flux-schnell", "gemma", "whisper"]);
  });

  test("filters by kind", () => {
    expect(ids({ kinds: ["image", "stt"] })).toEqual([
      "flux-schnell",
      "whisper",
    ]);
  });

  test("requires every requested capability", () => {
    expect(ids({ capabilities: ["tool-calling"] })).toEqual([
      "qwen-coder",
      "gemma",
    ]);
    expect(ids({ capabilities: ["tool-calling", "long-context"] })).toEqual([
      "gemma",
    ]);
  });

  test("filters by fit verdict and hides models without one", () => {
    expect(ids({ fits: ["fits", "tight"] })).toEqual(["qwen-coder", "whisper"]);
    expect(ids({ fits: ["wont-fit"] })).toEqual(["flux-schnell"]);
  });

  test("filters by installation in both directions", () => {
    expect(ids({ installed: true })).toEqual(["qwen-coder", "whisper"]);
    expect(ids({ installed: false })).toEqual(["flux-schnell", "gemma"]);
  });

  test("filters by license", () => {
    expect(ids({ licenses: ["conditional"] })).toEqual(["flux-schnell"]);
  });

  test("search matches id, name, and description terms case-insensitively", () => {
    expect(ids({ search: "FLUX" })).toEqual(["flux-schnell"]);
    expect(ids({ search: "coding local" })).toEqual(["qwen-coder"]);
    expect(ids({ search: "whisper-" })).toEqual([]);
  });

  test("combines filters", () => {
    expect(
      ids({ kinds: ["llm"], capabilities: ["tool-calling"], installed: true }),
    ).toEqual(["qwen-coder"]);
  });

  test("reports whether any filter is set", () => {
    expect(catalogQueryFilters({ sort: "name", reverse: true })).toBe(false);
    expect(catalogQueryFilters({ search: "  " })).toBe(false);
    expect(catalogQueryFilters({ installed: false })).toBe(true);
  });
});

describe("catalog query sorting", () => {
  test("sorts by name", () => {
    expect(ids({ sort: "name" })).toEqual([
      "flux-schnell",
      "gemma",
      "qwen-coder",
      "whisper",
    ]);
  });

  test("sorts by download size and memory, smallest first", () => {
    expect(ids({ sort: "size" })).toEqual([
      "whisper",
      "gemma",
      "qwen-coder",
      "flux-schnell",
    ]);
    expect(ids({ sort: "memory", reverse: true })).toEqual([
      "flux-schnell",
      "qwen-coder",
      "gemma",
      "whisper",
    ]);
  });

  test("sorts by fit, best first, with unknown verdicts last", () => {
    expect(ids({ sort: "fit" })).toEqual([
      "whisper",
      "qwen-coder",
      "flux-schnell",
      "gemma",
    ]);
    expect(ids({ sort: "fit", reverse: true })).toEqual([
      "flux-schnell",
      "qwen-coder",
      "whisper",
      "gemma",
    ]);
  });

  test("sorts by context, longest first, with unknown lengths last", () => {
    expect(ids({ sort: "context" })).toEqual([
      "gemma",
      "qwen-coder",
      "flux-schnell",
      "whisper",
    ]);
  });

  test("reverse without a sort reverses the input order", () => {
    expect(ids({ reverse: true })).toEqual([
      "whisper",
      "gemma",
      "flux-schnell",
      "qwen-coder",
    ]);
  });

  test("does not mutate the input", () => {
    const input = [...rows];
    queryCatalog(input, (value) => value, { sort: "name" });
    expect(input).toEqual(rows);
  });
});

describe("catalog query URL parameters", () => {
  test("round-trips a query", () => {
    const query = {
      search: "coder",
      kinds: ["llm"],
      capabilities: ["tool-calling", "reasoning"] as const,
      fits: ["fits", "tight"] as const,
      installed: false,
      licenses: ["open"] as const,
      sort: "memory" as const,
      reverse: true,
    };
    const params = catalogQueryParams({
      ...query,
      capabilities: [...query.capabilities],
      fits: [...query.fits],
      licenses: [...query.licenses],
    });
    expect(params.toString()).toBe(
      "q=coder&kind=llm&capability=tool-calling&capability=reasoning&fit=fits%2Ctight&installed=false&license=open&sort=memory&reverse=true",
    );
    expect(parseCatalogQuery(params)).toEqual({
      ...query,
      capabilities: [...query.capabilities],
      fits: [...query.fits],
      licenses: [...query.licenses],
    });
  });

  test("defaults write no parameters", () => {
    expect(catalogQueryParams({}).toString()).toBe("");
    expect(parseCatalogQuery(new URLSearchParams())).toEqual({});
  });

  test("drops unknown values instead of failing", () => {
    expect(
      parseCatalogQuery(
        new URLSearchParams(
          "kind=llm,bogus&capability=flying&capability=reasoning&fit=maybe&installed=yes&sort=rank&reverse=1",
        ),
        ["llm", "image"],
      ),
    ).toEqual({ kinds: ["llm"], capabilities: ["reasoning"] });
  });
});

describe("metadata rows", () => {
  const metadata = {
    id: "flux-schnell",
    catalog: {
      name: "FLUX",
      displayName: "FLUX schnell",
      kind: "image",
      quantization: "q4_0",
      summary: "Fast distilled image model",
      commercialStatus: "conditional" as const,
      contextWindowTokens: null,
      features: ["text-to-image"],
      capabilityResolution: {
        capabilities: [{ id: "text-to-image" }, { id: "image-to-image" }],
      },
      memory: {
        minimumVramEstimateGb: 12,
        storageEstimateGb: 7,
        fit: { verdict: "tight" as const },
      },
    },
    device: { installed: true },
  };

  test("maps effective capabilities, fit, and the summary", () => {
    expect(metadataCatalogRow(metadata)).toEqual({
      id: "flux-schnell",
      name: "FLUX schnell",
      text: ["FLUX", "q4_0", "Fast distilled image model"],
      kind: "image",
      capabilities: ["text-to-image", "image-to-image"],
      fit: "tight",
      installed: true,
      license: "conditional",
      storageGb: 7,
      memoryGb: 12,
      contextTokens: null,
    });
  });

  test("falls back to declared features for older servers", () => {
    const { capabilityResolution, ...catalog } = metadata.catalog;
    void capabilityResolution;
    const row = metadataCatalogRow({
      ...metadata,
      catalog: {
        ...catalog,
        memory: { minimumVramEstimateGb: 12, storageEstimateGb: 7 },
        commercialStatus: undefined,
      },
    });
    expect(row).toMatchObject({
      capabilities: ["text-to-image"],
      fit: null,
      license: null,
    });
  });
});
