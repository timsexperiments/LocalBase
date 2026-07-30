import { expect, test } from "bun:test";
import { renderMigrationAssetSource } from "./migration-assets";

test("renders deterministic ordered static migration imports", () => {
  const source = renderMigrationAssetSource(["0000_base", "0001_next"]);
  expect(source).toBe(renderMigrationAssetSource(["0000_base", "0001_next"]));
  expect(
    source
      .split("\n")
      .filter(
        (line) =>
          line.startsWith("import migration") ||
          /^  migration\d+Path,$/.test(line),
      )
      .map((line) => line.trim()),
  ).toEqual([
    'import migration0Path from "../../drizzle/0000_base.sql" with { type: "file" };',
    'import migration1Path from "../../drizzle/0001_next.sql" with { type: "file" };',
    "migration0Path,",
    "migration1Path,",
  ]);
});
