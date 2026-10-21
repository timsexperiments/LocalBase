import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BUILT_IN_CATALOG, byId, CATALOG } from "../catalog";
import {
  applyCatalogLayers,
  catalogDiagnostics,
  catalogProvenance,
  composeCatalog,
  usesReferenceMeasurements,
} from "./sources";
import {
  loadCatalogSources,
  removeUserCatalogEntry,
  userCatalogDirectory,
  writeUserCatalogEntry,
} from "./user-catalog";

const builtInId = "qwen3-embedding-0.6b-q8_0";
const roots: string[] = [];

function scratchRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "lb-user-catalog-"));
  roots.push(root);
  return root;
}

function newSpec(modelId = "my-tiny-model") {
  const base = byId(builtInId)!;
  return {
    ...base,
    modelId,
    displayName: "My tiny model",
    capabilities: ["text-generation"],
    unsupportedCapabilities: undefined,
    artifacts: [
      {
        sourcePath: "tiny.gguf",
        filename: `${modelId}.gguf`,
        expectedSizeBytes: 1000,
        sha256: "a".repeat(64),
        role: "primary",
      },
    ],
  };
}

afterEach(() => {
  applyCatalogLayers([]);
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

test("built-in entries are qualified and use reference measurements", () => {
  expect(catalogProvenance(builtInId)).toMatchObject({
    trust: "qualified",
    tier: "built-in",
  });
  expect(usesReferenceMeasurements({ modelId: builtInId })).toBe(true);
  expect(CATALOG.length).toBe(BUILT_IN_CATALOG.length);
});

test("a new user entry adds a model with user trust", () => {
  const composition = applyCatalogLayers([
    { tier: "user", entries: [{ path: "/u/a.json", data: newSpec() }] },
  ]);
  expect(composition.diagnostics).toEqual([]);
  expect(byId("my-tiny-model")?.displayName).toBe("My tiny model");
  expect(catalogProvenance("my-tiny-model")).toMatchObject({
    trust: "user",
    origin: "user",
    tier: "user",
    path: "/u/a.json",
  });
  expect(usesReferenceMeasurements({ modelId: "my-tiny-model" })).toBe(false);
});

test("a cosmetic patch keeps the entry qualified", () => {
  applyCatalogLayers([
    {
      tier: "user",
      entries: [
        {
          path: "/u/p.json",
          data: { modelId: builtInId, displayName: "Mine" },
        },
      ],
    },
  ]);
  const model = byId(builtInId)!;
  expect(model.displayName).toBe("Mine");
  // Untouched fields keep the built-in values.
  expect(model.contextWindowTokens).toBe(32768);
  expect(catalogProvenance(builtInId)).toMatchObject({
    trust: "qualified",
    origin: "built-in",
    tier: "user",
    patchedFields: ["displayName"],
  });
  expect(usesReferenceMeasurements(model)).toBe(true);
});

test("a launch-shaped patch marks the entry modified and drops measurements", () => {
  applyCatalogLayers([
    {
      tier: "user",
      entries: [
        {
          path: "/u/p.json",
          data: { modelId: builtInId, contextWindowTokens: 16384 },
        },
      ],
    },
  ]);
  expect(byId(builtInId)?.contextWindowTokens).toBe(16384);
  expect(catalogProvenance(builtInId).trust).toBe("modified");
  expect(usesReferenceMeasurements({ modelId: builtInId })).toBe(false);
});

test("a patch that repeats the built-in value changes nothing", () => {
  applyCatalogLayers([
    {
      tier: "user",
      entries: [
        {
          path: "/u/p.json",
          data: { modelId: builtInId, contextWindowTokens: 32768 },
        },
      ],
    },
  ]);
  expect(catalogProvenance(builtInId)).toMatchObject({
    trust: "qualified",
    patchedFields: [],
  });
});

test("user beats remote, remote beats built-in, whatever the layer order", () => {
  const composition = composeCatalog(BUILT_IN_CATALOG, [
    {
      tier: "user",
      entries: [
        { path: "/u/p.json", data: { modelId: builtInId, catch: "user" } },
      ],
    },
    {
      tier: "remote",
      entries: [
        {
          path: "/r/p.json",
          data: { modelId: builtInId, catch: "remote", notes: "remote" },
        },
      ],
    },
  ]);
  const model = composition.models.find((m) => m.modelId === builtInId)!;
  expect(model.catch).toBe("user");
  expect(model.notes).toBe("remote");
  expect(composition.provenance.get(builtInId)).toMatchObject({
    tier: "user",
    origin: "built-in",
    patchedFields: ["catch", "notes"],
  });
});

test("remote additions are community trust and become modified when patched", () => {
  const composition = composeCatalog(BUILT_IN_CATALOG, [
    {
      tier: "remote",
      entries: [{ path: "/r/a.json", data: newSpec("community-model") }],
    },
    {
      tier: "user",
      entries: [
        {
          path: "/u/a.json",
          data: { modelId: "community-model", contextWindowTokens: 4096 },
        },
      ],
    },
  ]);
  expect(composition.provenance.get("community-model")).toMatchObject({
    trust: "modified",
    origin: "remote",
    tier: "user",
  });
});

test("invalid entries are skipped with a diagnostic", () => {
  const composition = applyCatalogLayers([
    {
      tier: "user",
      entries: [
        { path: "/u/1.json", data: [] },
        { path: "/u/2.json", data: { displayName: "no id" } },
        { path: "/u/3.json", data: { modelId: builtInId, bogus: true } },
        { path: "/u/4.json", data: { modelId: "half-spec", kind: "llm" } },
        { path: "/u/5.json", data: { modelId: builtInId, artifacts: [] } },
        { path: "/u/6.json", data: newSpec("dupe") },
        {
          path: "/u/7.json",
          data: { ...newSpec("dupe"), displayName: "Second" },
        },
      ],
    },
  ]);
  const byPath = new Map(
    composition.diagnostics.map((d) => [d.path, d.message]),
  );
  expect(byPath.get("/u/1.json")).toContain("one JSON object");
  expect(byPath.get("/u/2.json")).toContain('"modelId"');
  expect(byPath.get("/u/3.json")).toContain("Unknown field(s): bogus");
  expect(byPath.get("/u/4.json")).toContain("The entry is invalid");
  expect(byPath.get("/u/5.json")).toContain("The patched entry is invalid");
  expect(byPath.get("/u/7.json")).toContain("/u/6.json wins");
  expect(byId("half-spec")).toBeUndefined();
  expect(byId("dupe")?.displayName).toBe("My tiny model");
  expect(byId(builtInId)?.artifacts.length).toBe(1);
  expect(catalogProvenance(builtInId).trust).toBe("qualified");
});

test("catalog.d is read in name order and bad files never throw", () => {
  const root = scratchRoot();
  const directory = userCatalogDirectory(root);
  mkdirSync(directory);
  writeFileSync(join(directory, "a.json"), JSON.stringify(newSpec("first")));
  writeFileSync(join(directory, "b.json"), "{ not json");
  writeFileSync(join(directory, "c.txt"), "ignored");
  mkdirSync(join(directory, "d.json"));
  const composition = loadCatalogSources(root);
  expect(byId("first")).toBeDefined();
  expect(catalogProvenance("first").path).toBe(join(directory, "a.json"));
  const messages = catalogDiagnostics().map((d) => `${d.path}: ${d.message}`);
  expect(composition.diagnostics.length).toBe(2);
  expect(
    messages.some((m) => m.includes("b.json: Skipped: invalid JSON")),
  ).toBe(true);
  expect(
    messages.some((m) => m.includes("d.json: Skipped: not a regular file")),
  ).toBe(true);
});

test("a missing catalog.d is an empty layer", () => {
  const composition = loadCatalogSources(join(scratchRoot(), "absent"));
  expect(composition.diagnostics).toEqual([]);
  expect(CATALOG.length).toBe(BUILT_IN_CATALOG.length);
});

test("write and remove a user entry, restoring the built-in", () => {
  const root = scratchRoot();
  const { path } = writeUserCatalogEntry(root, newSpec());
  expect(path).toBe(join(userCatalogDirectory(root), "my-tiny-model.json"));
  expect(byId("my-tiny-model")).toBeDefined();
  expect(removeUserCatalogEntry(root, "my-tiny-model")).toBe(path);
  expect(byId("my-tiny-model")).toBeUndefined();
  expect(removeUserCatalogEntry(root, "my-tiny-model")).toBeNull();

  const directory = userCatalogDirectory(root);
  writeFileSync(
    join(directory, "override.json"),
    JSON.stringify({ modelId: builtInId, contextWindowTokens: 8192 }),
  );
  loadCatalogSources(root);
  expect(catalogProvenance(builtInId).trust).toBe("modified");
  expect(removeUserCatalogEntry(root, builtInId)).toBe(
    join(directory, "override.json"),
  );
  expect(catalogProvenance(builtInId).trust).toBe("qualified");
  expect(byId(builtInId)?.contextWindowTokens).toBe(32768);
});

test("writeUserCatalogEntry rejects an invalid spec without writing", () => {
  const root = scratchRoot();
  expect(() => writeUserCatalogEntry(root, { modelId: "x" })).toThrow();
  expect(removeUserCatalogEntry(root, "x")).toBeNull();
});
