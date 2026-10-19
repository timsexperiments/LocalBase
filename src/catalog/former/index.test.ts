import { expect, test } from "bun:test";
import { readdirSync } from "node:fs";
import { join, relative } from "node:path";
import { BUILT_IN_CATALOG } from "../../catalog";
import { FORMER_CATALOG, FORMER_CATALOG_DATA, formerCatalogEntry } from ".";

const formerDirectory = import.meta.dir;

function dataFilesOnDisk(): string[] {
  return readdirSync(formerDirectory, { recursive: true, encoding: "utf8" })
    .filter((path) => path.endsWith(".json"))
    .map((path) => `./${path.split("\\").join("/")}`)
    .sort();
}

async function indexedImports(): Promise<string[]> {
  const source = await Bun.file(join(formerDirectory, "index.ts")).text();
  return [...source.matchAll(/^import \w+ from "(\.\/[^"]+\.json)";$/gm)].map(
    (match) => match[1]!,
  );
}

test("the former index imports every former data file exactly once", async () => {
  const imports = await indexedImports();
  expect(new Set(imports).size).toBe(imports.length);
  expect([...imports].sort()).toEqual(dataFilesOnDisk());
  expect(FORMER_CATALOG_DATA).toHaveLength(imports.length);
});

test("each former file is named after its model and filed under its kind", async () => {
  const imports = await indexedImports();
  expect(
    FORMER_CATALOG.map(({ kind, modelId }) => `./${kind}/${modelId}.json`),
  ).toEqual(imports);
  for (const path of imports) {
    expect(
      relative(formerDirectory, join(formerDirectory, path)),
    ).not.toStartWith("..");
  }
});

test("a former entry never shares an id with a built-in entry", () => {
  const builtIn = new Set(BUILT_IN_CATALOG.map(({ modelId }) => modelId));
  for (const { modelId } of FORMER_CATALOG) {
    expect(builtIn.has(modelId)).toBe(false);
    expect(formerCatalogEntry(modelId)?.modelId).toBe(modelId);
  }
  expect(formerCatalogEntry("qwen3.5-9b-q4_k_m")).toBeUndefined();
});
