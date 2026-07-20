import { describe, expect, test } from "bun:test";
import {
  CATALOG,
  CatalogSchema,
  modelDownloadUrl,
  primaryArtifact,
} from "./catalog";

const checksum = "a".repeat(64);

function model(artifacts: unknown[]) {
  return {
    modelId: "test-model",
    kind: "llm",
    provider: "Test",
    family: "Test",
    version: "1",
    size: "1B",
    quant: "Q4_K_M",
    minVramGb: 1,
    storageGb: 1,
    source: "https://huggingface.co/test/model",
    repositoryRevision: "revision",
    artifacts,
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["test"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "Test model.",
  };
}

describe("catalog artifact validation", () => {
  test("accepts single-file and sharded artifact sets", () => {
    const singleFile = model([
      { sourcePath: "model.gguf", filename: "model.gguf", role: "primary" },
    ]);
    const sharded = model([
      {
        sourcePath: "model-00001-of-00002.gguf",
        filename: "model-00001-of-00002.gguf",
        expectedSizeBytes: 10,
        sha256: checksum,
        role: "primary",
      },
      {
        sourcePath: "model-00002-of-00002.gguf",
        filename: "model-00002-of-00002.gguf",
        expectedSizeBytes: 8,
        sha256: "b".repeat(64),
        role: "supplementary",
      },
    ]);

    expect(CatalogSchema.safeParse([singleFile, sharded]).success).toBe(true);
  });

  test("rejects invalid artifact boundaries", () => {
    const invalidArtifacts = [
      [],
      [
        { sourcePath: "one.gguf", filename: "same.gguf", role: "primary" },
        {
          sourcePath: "two.gguf",
          filename: "same.gguf",
          role: "supplementary",
        },
      ],
      [
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          role: "supplementary",
        },
      ],
      [
        { sourcePath: "one.gguf", filename: "one.gguf", role: "primary" },
        { sourcePath: "two.gguf", filename: "two.gguf", role: "primary" },
      ],
      [
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          expectedSizeBytes: 1,
          sha256: "not-a-checksum",
          role: "primary",
        },
      ],
      [
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          expectedSizeBytes: 0,
          sha256: checksum,
          role: "primary",
        },
      ],
      [
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          expectedSizeBytes: 1,
          role: "primary",
        },
      ],
      [
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          sha256: checksum,
          role: "primary",
        },
      ],
    ];

    for (const artifacts of invalidArtifacts) {
      expect(CatalogSchema.safeParse([model(artifacts)]).success).toBe(false);
    }
  });

  test("pins Qwen3-Coder-Next's complete sharded artifact set", () => {
    const qwen = CATALOG.find(
      (model) => model.modelId === "qwen3-coder-next-q4_k_m",
    );

    expect(qwen).toMatchObject({
      repositoryRevision: "b82fb7382639d97b38fa7672e526c760c2fb358e",
      minVramGb: 64,
    });
    expect(qwen?.artifacts).toEqual([
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
        expectedSizeBytes: 15524827040,
        sha256:
          "6bcfc9f9c37901eeb92172e2ab871224dab36a453d263bcb2547f737409534da",
        role: "primary",
      },
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00002-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00002-of-00004.gguf",
        expectedSizeBytes: 14872168352,
        sha256:
          "817def0691ee9d08bf3dc4444be7aed29c9e52091e8fa9d97901ce7e7f6f01d3",
        role: "supplementary",
      },
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00003-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00003-of-00004.gguf",
        expectedSizeBytes: 14503294496,
        sha256:
          "23aa634d47dca9b4ca3ea249384e6f01951b24c83cdc076f37f6f43d6c99883f",
        role: "supplementary",
      },
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00004-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00004-of-00004.gguf",
        expectedSizeBytes: 3510702144,
        sha256:
          "249c768cc5f130dc731567d6edcbdacc48e14dec9e02c5dbe2b2185d2c5bdb2b",
        role: "supplementary",
      },
    ]);
  });

  test("resolves legacy and pinned primary artifact URLs", () => {
    const legacy = CATALOG.find(
      (model) => model.modelId === "qwen2.5-coder-1.5b-instruct-q4_k_m",
    );
    const qwen = CATALOG.find(
      (model) => model.modelId === "qwen3-coder-next-q4_k_m",
    );

    expect(legacy && modelDownloadUrl(legacy)).toBe(
      "https://huggingface.co/Qwen/Qwen2.5-Coder-1.5B-Instruct-GGUF/resolve/main/qwen2.5-coder-1.5b-instruct-q4_k_m.gguf",
    );
    expect(qwen && primaryArtifact(qwen).filename).toBe(
      "Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
    );
    expect(qwen && modelDownloadUrl(qwen)).toBe(
      "https://huggingface.co/Qwen/Qwen3-Coder-Next-GGUF/resolve/b82fb7382639d97b38fa7672e526c760c2fb358e/Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
    );
  });
});
