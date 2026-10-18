import { expect, test } from "bun:test";
import {
  lowQualityQuant,
  catalogVariantGroups,
  recommendedChoice,
  qualifiedCatalogMatch,
} from "./model-choice";
import type { Model } from "./client";

function choice(
  quant: string,
  verdict: "fits" | "tight" | "wont-fit" | null = "fits",
) {
  return {
    path: `model-${quant}.gguf`,
    quant,
    fit: verdict ? { verdict } : null,
  };
}

test("prefers balanced Q4 files with headroom over alphabetical and smaller files", () => {
  const files = [
    choice("IQ1_M"),
    choice("IQ2_XXS"),
    choice("Q4_0"),
    choice("Q8_0"),
    choice("Q4_K_M"),
  ];
  expect(recommendedChoice(files)?.quant).toBe("Q4_K_M");
  expect(files[0]?.quant).toBe("IQ1_M");
  const supportingFile = {
    ...choice("Q4_K_M"),
    path: "MTP/mtp-model-Q4_K_M.gguf",
  };
  expect(recommendedChoice([supportingFile, choice("Q8_0")])?.quant).toBe(
    "Q8_0",
  );
  expect(recommendedChoice([supportingFile])).toBeUndefined();
  expect(
    recommendedChoice([choice("UD-Q4_K_XL"), choice("Q4_K_M", "wont-fit")])
      ?.quant,
  ).toBe("UD-Q4_K_XL");
  expect(
    recommendedChoice([choice("Q4_K_M", "tight"), choice("Q5_K_M")])?.quant,
  ).toBe("Q5_K_M");
  expect(
    recommendedChoice([choice("Q4_K_M", null), choice("Q4_K_XL")])?.quant,
  ).toBe("Q4_K_XL");
});

test("never defaults to low-bit or non-fitting downloads, but preserves unknown fit", () => {
  for (const quant of ["IQ1_M", "UD-IQ2_XXS", "Q2_K", "TQ1_0"])
    expect(lowQualityQuant(quant)).toBe(true);
  expect(lowQualityQuant("IQ4_XS")).toBe(false);
  expect(
    recommendedChoice([
      choice("IQ1_M"),
      choice("IQ2_S"),
      choice("Q4_K_M", "wont-fit"),
    ]),
  ).toBeUndefined();
  expect(recommendedChoice([])).toBeUndefined();
  expect(recommendedChoice([choice("Q4_K_M", null)])?.fit).toBeNull();
});

// Matching needs only catalog identity, not installation/runtime metadata.
type Identity = {
  id: string;
  catalog: Pick<
    Model["catalog"],
    "name" | "displayName" | "source" | "qualificationState"
  >;
};
const catalog = (overrides: Partial<Identity["catalog"]> = {}): Identity => ({
  id: "qwen3.8-27b-q4_k_m",
  catalog: {
    name: "qwen3.8",
    displayName: "Qwen3.8 27B",
    qualificationState: "qualified",
    source: "https://huggingface.co/bartowski/Qwen3.8-27B-GGUF",
    ...overrides,
  },
});

test("matches qualified catalog identities without swallowing derivatives or versions", () => {
  const models = [catalog()];
  expect(qualifiedCatalogMatch("unsloth/Qwen3.8-27B-GGUF", models)?.id).toBe(
    "qwen3.8-27b-q4_k_m",
  );
  expect(qualifiedCatalogMatch("bartowski/Qwen3.8-27B-GGUF", models)?.id).toBe(
    "qwen3.8-27b-q4_k_m",
  );
  expect(
    qualifiedCatalogMatch("other/Qwen3.8-27B-Uncensored-GGUF", models),
  ).toBeUndefined();
  expect(
    qualifiedCatalogMatch("other/Qwen3.5-27B-GGUF", models),
  ).toBeUndefined();
  expect(
    qualifiedCatalogMatch("unsloth/Qwen3.8-27B-GGUF", [
      catalog({ qualificationState: "experimental" }),
    ]),
  ).toBeUndefined();
});

test("unknown quantization is never a recommendation", () => {
  expect(
    recommendedChoice([{ ...choice("unknown"), quant: null }]),
  ).toBeUndefined();
});

const variant = (
  id: string,
  quantization: string,
  overrides: Partial<Model["catalog"]> = {},
): Model => ({
  id,
  catalog: {
    name: "qwen",
    displayName: "Qwen3.5 0.8B",
    kind: "llm",
    qualificationState: "qualified",
    source: "https://huggingface.co/org/qwen",
    revision: "same-revision",
    quantization,
    memory: { minimumVramEstimateGb: 3, storageEstimateGb: 0.58 },
    features: [],
    inputModalities: ["text"],
    outputModalities: ["text"],
    contextWindowTokens: 8192,
    capabilities: null,
    ...overrides,
  },
  device: { installed: false, selected: false, runtime: null },
});
test("catalog groups quant variants while keeping distinct identities and capabilities separate", () => {
  const q4 = variant("q4", "Q4_K_M");
  const q8 = variant("q8", "Q8_0");
  expect(
    catalogVariantGroups([q4, q8]).map((group) =>
      group.map((model) => model.id),
    ),
  ).toEqual([["q4", "q8"]]);
  for (const changes of [
    { source: "https://huggingface.co/other/qwen" },
    { revision: "different" },
    { displayName: "Qwen3.5 2B" },
    { inputModalities: ["text", "image"] },
    { contextWindowTokens: 4096 },
  ]) {
    expect(
      catalogVariantGroups([q4, variant("other", "Q8_0", changes)]),
    ).toHaveLength(2);
  }
  expect(
    catalogVariantGroups([q4, variant("same-quant", "Q4_K_M")]),
  ).toHaveLength(2);
  expect(
    catalogVariantGroups([
      variant("missing-q4", "Q4_K_M", { revision: undefined }),
      variant("missing-q8", "Q8_0", { revision: undefined }),
    ]),
  ).toHaveLength(2);
});
