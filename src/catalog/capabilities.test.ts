import { expect, test } from "bun:test";
import {
  byId,
  legacyFeatures,
  modelSpecSchema,
  type ModelSpec,
} from "../catalog";
import { capabilitySchema, hasCapability } from "./capabilities";

function catalogEntry(modelId: string): ModelSpec {
  const model = byId(modelId);
  if (!model) throw new Error(`Missing catalog fixture ${modelId}.`);
  return model;
}

function issues(input: unknown): string[] {
  const result = modelSpecSchema.safeParse(input);
  return result.success
    ? []
    : result.error.issues.map(({ message }) => message);
}

test("bare capability ids parse as catalog declarations", () => {
  const model = catalogEntry("qwen3.5-0.8b-q4_k_m");
  const parsed = modelSpecSchema.parse({
    ...model,
    capabilities: ["text-generation", "tool-calling"],
  });
  expect(parsed.capabilities).toEqual([
    { id: "text-generation", source: "declared" },
    { id: "tool-calling", source: "declared" },
  ]);
  expect(hasCapability(parsed, "tool-calling")).toBe(true);
  expect(hasCapability(parsed, "image-to-image")).toBe(false);
});

test("unknown, duplicate and legacy capability entries are rejected", () => {
  const model = catalogEntry("qwen3.5-0.8b-q4_k_m");
  expect(capabilitySchema.safeParse("lightweight").success).toBe(false);
  expect(issues({ ...model, capabilities: ["wav-output"] })).not.toEqual([]);
  expect(
    issues({ ...model, capabilities: ["text-generation", "text-generation"] }),
  ).not.toEqual([]);
  expect(
    issues({
      ...model,
      capabilities: [{ id: "text-generation", source: "runtime" }],
    }),
  ).not.toEqual([]);
  expect(issues({ ...model, features: ["text-generation"] })).not.toEqual([]);
});

test("typed output and runtime fields only fit matching kinds", () => {
  const llm = catalogEntry("qwen3.5-0.8b-q4_k_m");
  const tts = catalogEntry("kokoro-82m-v1.0-q8");
  const video = catalogEntry("wan2.2-s2v-14b-fp8");
  const catalogOnly = catalogEntry("hunyuanvideo-1.5-t2v-480p-q4_k_m");
  expect(issues(tts)).toEqual([]);
  expect(issues(video)).toEqual([]);
  expect(issues({ ...llm, outputFormat: "wav" })).not.toEqual([]);
  expect(issues({ ...tts, outputFormat: "avi" })).not.toEqual([]);
  expect(issues({ ...llm, experimentalRuntime: true })).not.toEqual([]);
  expect(issues({ ...llm, diffusionWeightsOnly: true })).not.toEqual([]);
  expect(issues(catalogOnly)).toEqual([]);
  expect(
    issues({
      ...video,
      videoCatalogOnly: undefined,
      diffusionWeightsOnly: true,
    }),
  ).not.toEqual([]);
});

test("legacy features keep the pre-capability tag lists", () => {
  expect(legacyFeatures(catalogEntry("kokoro-82m-v1.0-q8"))).toEqual(
    expect.arrayContaining([
      "text-to-speech",
      "wav-output",
      "cold-per-request",
      "bundled-native-voices",
    ]),
  );
  expect(legacyFeatures(catalogEntry("qwen3-tts-1.7b-base-q4_k_m"))).toEqual(
    expect.arrayContaining(["wav-output", "catalog-reference-voices"]),
  );
  expect(legacyFeatures(catalogEntry("wan2.2-s2v-14b-fp8"))).toEqual([
    "speech-to-video",
    "avi-output",
    "experimental",
  ]);
  expect(
    legacyFeatures(catalogEntry("hunyuanvideo-1.5-t2v-480p-q4_k_m")),
  ).toEqual(["catalog-only", "diffusion-weights-only"]);
});
