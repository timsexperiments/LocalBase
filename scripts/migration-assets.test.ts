import { describe, expect, test } from "bun:test";
import {
  assertMigrationAgreement,
  migrationAssetSource,
  migrationAssetsPath,
} from "./migration-assets";

describe("migration asset preparation", () => {
  test("requires exact SQL and journal agreement", () => {
    expect(() =>
      assertMigrationAgreement(["0000_base"], ["0000_base"]),
    ).not.toThrow();
    for (const [files, journal] of [
      [["0000_base"], []],
      [[], ["0000_base"]],
      [
        ["0000_base", "0001_next"],
        ["0001_next", "0000_base"],
      ],
      [["0000_base"], ["0000_base", "0000_base"]],
    ]) {
      expect(() => assertMigrationAgreement(files, journal)).toThrow();
    }
  });

  test("keeps generated output in exact deterministic agreement", async () => {
    expect(await Bun.file(migrationAssetsPath).text()).toBe(
      await migrationAssetSource(),
    );
  });
});
