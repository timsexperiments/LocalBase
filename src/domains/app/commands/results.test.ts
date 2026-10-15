import { expect, test } from "bun:test";
import { CATALOG, withLegacyFeatures } from "../../../catalog";
import { evaluateModelFit, modelMemoryJson } from "../../models/memory-fit";
import { modelOutputSchema } from "./results";

test("model command output validates complete catalog records strictly", () => {
  const spec = CATALOG[0]!;
  const model = {
    ...withLegacyFeatures(spec),
    ...modelMemoryJson(evaluateModelFit(spec, 64)),
  };
  expect(modelOutputSchema.safeParse(model).success).toBe(true);
  expect(
    modelOutputSchema.safeParse({ ...model, unexpected: true }).success,
  ).toBe(false);

  const incomplete = { ...model } as Record<string, unknown>;
  delete incomplete.repositoryRevision;
  expect(modelOutputSchema.safeParse(incomplete).success).toBe(false);

  const withoutSource = { ...model } as Record<string, unknown>;
  delete withoutSource.memorySource;
  expect(modelOutputSchema.safeParse(withoutSource).success).toBe(false);
});
