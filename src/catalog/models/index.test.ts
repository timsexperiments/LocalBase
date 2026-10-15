import { expect, test } from "bun:test";
import { readdirSync } from "node:fs";
import { join, relative } from "node:path";
import { CATALOG } from "../../catalog";
import { CATALOG_DATA } from ".";

const modelsDirectory = import.meta.dir;

function dataFilesOnDisk(): string[] {
  return readdirSync(modelsDirectory, { recursive: true, encoding: "utf8" })
    .filter((path) => path.endsWith(".json"))
    .map((path) => `./${path.split("\\").join("/")}`)
    .sort();
}

async function indexedImports(): Promise<string[]> {
  const source = await Bun.file(join(modelsDirectory, "index.ts")).text();
  return [...source.matchAll(/^import \w+ from "(\.\/[^"]+\.json)";$/gm)].map(
    (match) => match[1]!,
  );
}

test("the index imports every catalog data file exactly once", async () => {
  const imports = await indexedImports();
  expect(new Set(imports).size).toBe(imports.length);
  expect([...imports].sort()).toEqual(dataFilesOnDisk());
  expect(CATALOG_DATA).toHaveLength(imports.length);
});

test("each data file is named after its model and filed under its kind", async () => {
  const imports = await indexedImports();
  expect(
    CATALOG.map(({ kind, modelId }) => `./${kind}/${modelId}.json`),
  ).toEqual(imports);
  for (const path of imports) {
    expect(
      relative(modelsDirectory, join(modelsDirectory, path)),
    ).not.toStartWith("..");
  }
});
