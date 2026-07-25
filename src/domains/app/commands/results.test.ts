import { expect, test } from "bun:test";
import { CATALOG } from "../../../catalog";
import { modelOutputSchema } from "./results";

test("model command output validates complete catalog records strictly", () => {
  const model = CATALOG[0];
  expect(modelOutputSchema.safeParse(model).success).toBe(true);
  expect(
    modelOutputSchema.safeParse({ ...model, unexpected: true }).success,
  ).toBe(false);

  const incomplete = { ...model } as Record<string, unknown>;
  delete incomplete.repositoryRevision;
  expect(modelOutputSchema.safeParse(incomplete).success).toBe(false);
});
