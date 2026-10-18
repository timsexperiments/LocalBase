import { expect, test } from "bun:test";
import {
  lowQualityQuant,
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
