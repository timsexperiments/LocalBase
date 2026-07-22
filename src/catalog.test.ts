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
    repositoryRevision: "c".repeat(40),
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
      {
        sourcePath: "model.gguf",
        filename: "model.gguf",
        expectedSizeBytes: 10,
        sha256: checksum,
        role: "primary",
      },
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

  test("requires immutable release metadata for every catalog artifact", () => {
    for (const catalogModel of CATALOG) {
      expect(catalogModel.repositoryRevision).toMatch(/^[a-f0-9]{40}$/);

      for (const artifact of catalogModel.artifacts) {
        expect(artifact.expectedSizeBytes).toBeGreaterThan(0);
        expect(artifact.sha256).toMatch(/^[a-f0-9]{64}$/);
      }
    }
  });

  test("resolves primary artifact URLs from immutable release locks", () => {
    for (const catalogModel of CATALOG) {
      const primary = primaryArtifact(catalogModel);

      expect(modelDownloadUrl(catalogModel)).toBe(
        `${catalogModel.source}/resolve/${catalogModel.repositoryRevision}/${primary.sourcePath}`,
      );
    }
  });
});
