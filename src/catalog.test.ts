import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  CATALOG,
  artifactDownloadUrl,
  byId,
  calculateMaxSafeContextSize,
  catalogSchema,
  hasCapability,
  legacyFeatures,
  resolveCatalogInstallation,
  type ModelSpec,
} from "./catalog";
import {
  catalogModelDemand,
  estimateCatalogModelMemory,
  minimumMemoryGb,
  totalArtifactBytes,
} from "./domains/runtime/memory-estimate";
import {
  computedMinVramGb,
  modelMemoryFigure,
  recommendedForVram,
} from "./domains/models/memory-fit";

import { qualifiedVideoInput } from "./domains/runtime/video/gateway-contract";

import { resolveImageLaunchPlan } from "./domains/runtime/launch-plan";
import { buildSdImageServerArgs } from "./domains/runtime/launcher";

const checksum = "a".repeat(64);

function capabilityIds(entry: ModelSpec | undefined): string[] {
  return entry?.capabilities.map(({ id }) => id) ?? [];
}

test("image-to-video feature matches image-conditioned runtime modes", () => {
  for (const entry of CATALOG.filter(
    (model) => model.kind === "video" && model.videoRuntime,
  )) {
    const supportsImages = ["ti2v", "i2v"].includes(entry.videoRuntime!.mode);
    expect(hasCapability(entry, "image-to-video")).toBe(supportsImages);
    if (supportsImages) expect(entry.inputModalities).toContain("image");
  }
});

const verifiedContextWindows = new Map<string, number>([
  ["qwen2.5-coder-1.5b-instruct-q4_k_m", 32_768],
  ["qwen3-embedding-0.6b-q8_0", 32_768],
  ["qwen3-coder-next-q4_k_m", 262_144],
  ["gpt-oss-20b-q4_k_m", 131_072],
  ["qwen3.5-27b-q4_k_m", 262_144],
  ["qwen3.5-0.8b-q4_k_m", 262_144],
  ["qwen3.5-0.8b-q8_0", 262_144],
  ["qwen3.5-2b-q4_k_m", 262_144],
  ["qwen3.5-2b-q8_0", 262_144],
  ["qwen3.5-4b-q4_k_m", 262_144],
  ["qwen3.5-9b-q4_k_m", 262_144],
  ["qwen3-embedding-4b-q4_k_m", 32_768],
  ["qwen3-embedding-4b-q8_0", 32_768],
  ["qwen3-embedding-8b-q4_k_m", 32_768],
  ["qwen3-embedding-8b-q8_0", 32_768],
  ["mistral-small-3.2-24b-instruct-q4_k_m", 131_072],
]);

function model(artifacts: unknown[]) {
  return {
    modelId: "test-model",
    kind: "llm",
    provider: "Test",
    family: "Test",
    version: "1",
    size: "1B",
    quant: "Q4_K_M",
    storageGb: 1,
    source: "https://huggingface.co/test/model",
    repositoryRevision: "c".repeat(40),
    artifacts,
    inputModalities: ["text"],
    outputModalities: ["text"],
    capabilities: ["text-generation"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "Test model.",
  };
}

describe("catalog artifact validation", () => {
  test("image installation requires complete supplementary artifacts, not only diffusion", async () => {
    const image = byId("flux2-klein-4b-q4_0");
    if (image?.imageRuntime?.kind !== "diffusion-qwen3") {
      throw new Error("Missing Qwen image fixture");
    }
    const fixture = {
      ...image,
      imageRuntime: image.imageRuntime,
      artifacts: image.artifacts.map((artifact) => ({
        ...artifact,
        expectedSizeBytes: 4,
      })),
    };
    const directory = mkdtempSync(
      join(tmpdir(), "localbase-image-installation-"),
    );
    try {
      await Bun.write(
        join(directory, fixture.imageRuntime.artifacts.diffusionModel),
        "test",
      );
      expect(
        (await resolveCatalogInstallation(fixture, directory)).complete,
      ).toBe(false);
      await Bun.write(
        join(directory, fixture.imageRuntime.artifacts.vae),
        "test",
      );
      await Bun.write(
        join(directory, fixture.imageRuntime.artifacts.textEncoder),
        "bad",
      );
      expect(
        (await resolveCatalogInstallation(fixture, directory)).complete,
      ).toBe(false);
      await Bun.write(
        join(directory, fixture.imageRuntime.artifacts.textEncoder),
        "test",
      );
      expect(
        (await resolveCatalogInstallation(fixture, directory)).complete,
      ).toBe(true);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("image profiles require distinct declared artifacts with matching roles", () => {
    const image = byId("flux2-klein-4b-q4_0");
    if (image?.imageRuntime?.kind !== "diffusion-qwen3") {
      throw new Error("Missing Qwen image fixture");
    }
    expect(catalogSchema.safeParse([image]).success).toBe(true);
    for (const invalid of [
      { ...image, kind: "llm" },
      {
        ...image,
        imageRuntime: {
          ...image.imageRuntime,
          artifacts: {
            ...image.imageRuntime.artifacts,
            vae: "missing.safetensors",
          },
        },
      },
      {
        ...image,
        imageRuntime: {
          ...image.imageRuntime,
          artifacts: {
            ...image.imageRuntime.artifacts,
            vae: image.imageRuntime.artifacts.textEncoder,
          },
        },
      },
      {
        ...image,
        imageRuntime: {
          ...image.imageRuntime,
          artifacts: {
            ...image.imageRuntime.artifacts,
            diffusionModel: image.imageRuntime.artifacts.textEncoder,
          },
        },
      },
      {
        ...image,
        imageRuntime: {
          ...image.imageRuntime,
          generation: { ...image.imageRuntime.generation, steps: 0 },
        },
      },
    ]) {
      expect(catalogSchema.safeParse([invalid]).success).toBe(false);
    }
  });

  test("image models alone declare strict sd-server launch options", () => {
    const image = byId("dreamshaper-v8");
    const llm = CATALOG.find((model) => model.kind === "llm");
    if (!image || !llm) throw new Error("Missing catalog fixtures");
    const { imageLaunchOptions: _omitted, ...withoutOptions } = image;
    for (const invalid of [
      withoutOptions,
      { ...image, imageLaunchOptions: {} },
      {
        ...image,
        imageLaunchOptions: { diffusionFlashAttention: true, extra: 1 },
      },
      { ...llm, imageLaunchOptions: { diffusionFlashAttention: false } },
    ]) {
      expect(catalogSchema.safeParse([invalid]).success).toBe(false);
    }
  });

  test("an image LoRA names a supplementary artifact the runtime leaves unmapped", () => {
    const klein = byId("flux2-klein-4b-q4_0");
    if (!klein?.imageRuntime || !klein.imageLaunchOptions)
      throw new Error("Missing catalog fixtures");
    const loraArtifact = {
      ...klein.artifacts[0]!,
      role: "supplementary" as const,
      filename: "style-lora.safetensors",
    };
    const withLora = (
      lora: Record<string, unknown>,
      artifacts = [...klein.artifacts, loraArtifact],
    ) => ({
      ...klein,
      modelId: "style-lora",
      artifacts,
      imageLaunchOptions: { ...klein.imageLaunchOptions, lora },
    });
    const valid = {
      artifactFilename: "style-lora.safetensors",
      defaultStrength: 1,
      trigger: "pixel art",
    };
    expect(catalogSchema.safeParse([withLora(valid)]).success).toBe(true);
    for (const invalid of [
      withLora({ ...valid, artifactFilename: "missing.safetensors" }),
      withLora({
        ...valid,
        artifactFilename: klein.imageRuntime.artifacts.diffusionModel,
      }),
      withLora({ ...valid, defaultStrength: 2.5 }),
      withLora({ ...valid, defaultStrength: -1 }),
      withLora({ ...valid, extra: true }),
      withLora(valid, [
        ...klein.artifacts,
        { ...loraArtifact, role: "primary" as const },
      ]),
    ]) {
      expect(catalogSchema.safeParse([invalid]).success).toBe(false);
    }
  });

  test("pins the default decision model and its projector", () => {
    const clef = byId("clef-flash-q4_k_m");
    expect(clef).toMatchObject({
      kind: "llm",
      provider: "Cloudflare",
      source: "https://huggingface.co/ggml-org/Clef-Flash-GGUF",
      repositoryRevision: "6b4fe4c59098479652d94f0fe11a40facbebd1ec",
      llmRuntime: { capability: "decision-only" },
      inputModalities: ["text", "image"],
      contextWindowTokens: 262_144,
      artifacts: [
        {
          filename: "Clef-Flash-Q4_K_M.gguf",
          expectedSizeBytes: 6486448288,
          sha256:
            "3626116623ca8d89641952471fac9ce12af84fb3517520029a2ac6f5146868a2",
          role: "primary",
        },
        {
          filename: "mmproj-Clef-Flash-Q8_0.gguf",
          expectedSizeBytes: 624229728,
          sha256:
            "9dd8bc279f57d33b0697d4d0fe900fe751a24c9e3d30ab90969d4714ffc0722d",
          role: "supplementary",
        },
      ],
    });
    expect(hasCapability(clef!, "decision")).toBe(true);
    // A decision model never stands in for a chat recommendation.
    expect(recommendedForVram(128)).not.toContainEqual(clef);
  });

  test("pins the embedding-only Qwen runtime contract", () => {
    expect(byId("qwen3-embedding-0.6b-q8_0")).toMatchObject({
      kind: "llm",
      contextWindowTokens: 32_768,
      llmRuntime: {
        capability: "embedding-only",
        pooling: "last",
        dimensions: { minimum: 1024, maximum: 1024 },
      },
      artifacts: [
        {
          expectedSizeBytes: 639150592,
          sha256:
            "06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439",
        },
      ],
    });
    for (const [modelId, dimensions] of [
      ["qwen3-embedding-0.6b-q8_0", 1024],
      ["qwen3-embedding-4b-q4_k_m", 2560],
      ["qwen3-embedding-4b-q8_0", 2560],
      ["qwen3-embedding-8b-q4_k_m", 4096],
      ["qwen3-embedding-8b-q8_0", 4096],
    ] satisfies [string, number][]) {
      const embedding = byId(modelId);
      if (!embedding) throw new Error(`Missing embedding entry: ${modelId}`);
      expect(embedding.llmRuntime).toEqual({
        capability: "embedding-only",
        pooling: "last",
        dimensions: { minimum: dimensions, maximum: dimensions },
      });
      expect(calculateMaxSafeContextSize(embedding, 64)).toBe(32_768);
      expect(recommendedForVram(128)).not.toContainEqual(embedding);
    }
  });

  test("keeps supported-architecture additions within the packaged capabilities", () => {
    const ids = [
      "qwen3.5-0.8b-q4_k_m",
      "qwen3.5-0.8b-q8_0",
      "qwen3.5-2b-q4_k_m",
      "qwen3.5-2b-q8_0",
      "qwen3.5-4b-q4_k_m",
      "qwen3.5-9b-q4_k_m",
      "qwen3-embedding-4b-q4_k_m",
      "qwen3-embedding-4b-q8_0",
      "qwen3-embedding-8b-q4_k_m",
      "qwen3-embedding-8b-q8_0",
      "whisper-tiny-q8_0",
      "whisper-base-en-q8_0",
      "whisper-small-q8_0",
      "whisper-medium-q8_0",
      "realvis-xl-v5.0",
    ];
    const toolCallingVerified = new Set([
      "qwen3.5-4b-q4_k_m",
      "qwen3.5-9b-q4_k_m",
    ]);
    for (const id of ids) {
      const entry = byId(id);
      if (!entry) throw new Error(`Missing catalog entry: ${id}`);
      expect(entry.artifacts).toHaveLength(1);
      const [artifact] = entry.artifacts;
      if (!artifact) throw new Error(`Missing artifact: ${id}`);
      expect(artifactDownloadUrl(entry, artifact)).toContain(
        `/resolve/${entry.repositoryRevision}/`,
      );
      for (const capability of [
        "vision",
        "tool-calling",
        "image-to-image",
        "translation",
      ]) {
        if (capability === "tool-calling" && toolCallingVerified.has(id)) {
          expect(capabilityIds(entry)).toContain(capability);
          continue;
        }
        expect(capabilityIds(entry)).not.toContain(capability);
      }
      expect(entry.inputModalities).toEqual(
        entry.kind === "stt" ? ["audio"] : ["text"],
      );
    }
    expect(capabilityIds(byId("whisper-base-en-q8_0"))).not.toContain(
      "multilingual",
    );
    for (const size of ["tiny", "base", "small", "medium"]) {
      expect(capabilityIds(byId(`whisper-${size}-q8_0`))).toContain(
        "multilingual",
      );
    }
    expect(capabilityIds(byId("whisper-large-v3-turbo"))).toContain(
      "multilingual",
    );
    expect(capabilityIds(byId("whisper-tiny-en-q8_0"))).not.toContain(
      "multilingual",
    );
    expect(byId("realvis-xl-v5.0")).toMatchObject({
      commercialStatus: "conditional",
      capabilities: [{ id: "text-to-image", source: "declared" }],
      outputModalities: ["image"],
    });
  });

  test("requires video profiles to name declared artifacts and bounded measurements", () => {
    const video = {
      ...model([
        {
          sourcePath: "diffusion.gguf",
          filename: "diffusion.gguf",
          expectedSizeBytes: 10,
          sha256: checksum,
          role: "primary",
        },
        {
          sourcePath: "encoder.gguf",
          filename: "encoder.gguf",
          expectedSizeBytes: 8,
          sha256: "b".repeat(64),
          role: "supplementary",
        },
        {
          sourcePath: "vae.safetensors",
          filename: "vae.safetensors",
          expectedSizeBytes: 6,
          sha256: "c".repeat(64),
          role: "supplementary",
        },
      ]),
      kind: "video",
      inputModalities: ["text"],
      outputModalities: ["video"],
      videoRuntime: {
        mode: "t2v",
        artifacts: {
          diffusionModel: "diffusion.gguf",
          textEncoder: "encoder.gguf",
          decoder: { kind: "vae", artifactFilename: "vae.safetensors" },
        },
        qualification: {
          jobDeadlineMs: 10 * 60 * 1_000,
          maxWidth: 320,
          maxHeight: 320,
          maxFrames: 33,
          fps: 16,
          generation: {
            sampler: "euler",
            scheduler: "discrete",
            steps: 20,
            cfgScale: 6,
            flowShift: 3,
            seed: 42,
          },
          launchOptions: {
            cpuOffload: true,
            diffusionFlashAttention: true,
            vaeConvDirect: false,
          },
        },
        supportedTargets: [
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
    };
    expect(catalogSchema.safeParse([video]).success).toBe(true);
    for (const jobDeadlineMs of [undefined, 0, -1, 1.5, Infinity, 1_800_001]) {
      expect(
        catalogSchema.safeParse([
          {
            ...video,
            videoRuntime: {
              ...video.videoRuntime,
              qualification: {
                ...video.videoRuntime.qualification,
                jobDeadlineMs,
              },
            },
          },
        ]).success,
      ).toBe(false);
    }
    expect(
      catalogSchema.safeParse([
        {
          ...video,
          videoRuntime: {
            ...video.videoRuntime,
            qualification: {
              ...video.videoRuntime.qualification,
              jobDeadlineMs: 1_800_000,
            },
          },
        },
      ]).success,
    ).toBe(true);
    expect(
      catalogSchema.safeParse([
        {
          ...video,
          videoRuntime: {
            ...video.videoRuntime,
            artifacts: {
              ...video.videoRuntime.artifacts,
              decoder: { kind: "tae", artifactFilename: "vae.safetensors" },
            },
            qualification: {
              ...video.videoRuntime.qualification,
              generation: {
                ...video.videoRuntime.qualification.generation,
                scheduler: "lcm",
              },
            },
          },
        },
      ]).success,
    ).toBe(true);
    expect(
      catalogSchema.safeParse([
        {
          ...video,
          videoRuntime: {
            ...video.videoRuntime,
            artifacts: {
              ...video.videoRuntime.artifacts,
              decoder: { kind: "vae", artifactFilename: "missing.vae" },
            },
          },
        },
      ]).success,
    ).toBe(false);
  });

  test("requires an S2V profile to map its declared audio encoder", () => {
    const artifacts = [
      {
        sourcePath: "diffusion.safetensors",
        filename: "diffusion.safetensors",
        expectedSizeBytes: 10,
        sha256: checksum,
        role: "primary",
      },
      ...["text.safetensors", "vae.safetensors", "wav2vec2.safetensors"].map(
        (filename, index) => ({
          sourcePath: filename,
          filename,
          expectedSizeBytes: 9 - index,
          sha256: String.fromCharCode(98 + index).repeat(64),
          role: "supplementary",
        }),
      ),
    ];
    const speechVideo = {
      ...model(artifacts),
      kind: "video",
      inputModalities: ["text", "audio", "image"],
      outputModalities: ["video"],
      videoRuntime: {
        mode: "s2v",
        artifacts: {
          diffusionModel: "diffusion.safetensors",
          textEncoder: "text.safetensors",
          decoder: { kind: "vae", artifactFilename: "vae.safetensors" },
          audioEncoder: "wav2vec2.safetensors",
        },
        qualification: {
          jobDeadlineMs: 10 * 60 * 1_000,
          maxWidth: 832,
          maxHeight: 480,
          maxFrames: 81,
          fps: 16,
          generation: {
            sampler: "euler",
            scheduler: "discrete",
            steps: 20,
            cfgScale: 6,
            flowShift: 3,
            seed: 42,
          },
          launchOptions: {
            cpuOffload: true,
            diffusionFlashAttention: true,
            vaeConvDirect: false,
          },
        },
        supportedTargets: [
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
    };

    expect(catalogSchema.safeParse([speechVideo]).success).toBe(true);
    expect(
      catalogSchema.safeParse([
        {
          ...speechVideo,
          videoRuntime: {
            ...speechVideo.videoRuntime,
            artifacts: {
              ...speechVideo.videoRuntime.artifacts,
              decoder: { kind: "tae", artifactFilename: "vae.safetensors" },
            },
          },
        },
      ]).success,
    ).toBe(false);
    expect(
      catalogSchema.safeParse([
        {
          ...speechVideo,
          videoRuntime: {
            ...speechVideo.videoRuntime,
            artifacts: {
              ...speechVideo.videoRuntime.artifacts,
              audioEncoder: "missing.safetensors",
            },
          },
        },
      ]).success,
    ).toBe(false);
  });

  test("requires TTS runtime files to name declared supplementary artifacts", () => {
    const tts = {
      ...model([
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          expectedSizeBytes: 10,
          sha256: checksum,
          role: "primary",
        },
        {
          sourcePath: "projector.gguf",
          filename: "projector.gguf",
          expectedSizeBytes: 8,
          sha256: "b".repeat(64),
          role: "supplementary",
        },
        {
          sourcePath: "harbor.wav",
          filename: "harbor.wav",
          expectedSizeBytes: 6,
          sha256: "c".repeat(64),
          role: "supplementary",
        },
        {
          sourcePath: "willow.wav",
          filename: "willow.wav",
          expectedSizeBytes: 4,
          sha256: "d".repeat(64),
          role: "supplementary",
        },
      ]),
      kind: "tts",
      inputModalities: ["text"],
      outputModalities: ["audio"],
      ttsRuntime: {
        kind: "llama-tts",
        projectorArtifactFilename: "projector.gguf",
        referenceVoices: [
          {
            name: "harbor",
            artifactFilename: "harbor.wav",
            license: "CC0-1.0",
            provenanceUrl: "https://example.test/voices",
          },
          {
            name: "willow",
            artifactFilename: "willow.wav",
            license: "CC0-1.0",
            provenanceUrl: "https://example.test/voices",
          },
        ],
      },
    };
    expect(catalogSchema.safeParse([tts]).success).toBe(true);
    expect(
      catalogSchema.safeParse([
        {
          ...tts,
          ttsRuntime: {
            ...tts.ttsRuntime,
            projectorArtifactFilename: "model.gguf",
          },
        },
      ]).success,
    ).toBe(false);
    expect(
      catalogSchema.safeParse([
        {
          ...tts,
          ttsRuntime: {
            ...tts.ttsRuntime,
            referenceVoices: [
              ...tts.ttsRuntime.referenceVoices,
              {
                name: "harbor",
                artifactFilename: "missing.wav",
                license: "CC0-1.0",
                provenanceUrl: "https://example.test/voices",
              },
            ],
          },
        },
      ]).success,
    ).toBe(false);
  });

  test("requires Kokoro voices and local artifacts to be declared", () => {
    const kokoro = {
      ...model([
        {
          sourcePath: "onnx/model_quantized.onnx",
          filename: "model_quantized.onnx",
          expectedSizeBytes: 10,
          sha256: checksum,
          role: "primary",
        },
        ...["config.json", "tokenizer.json", "tokenizer_config.json"].map(
          (filename) => ({
            sourcePath: filename,
            filename,
            expectedSizeBytes: 1,
            sha256: "b".repeat(64),
            role: "supplementary" as const,
          }),
        ),
      ]),
      kind: "tts",
      inputModalities: ["text"],
      outputModalities: ["audio"],
      ttsRuntime: {
        kind: "kokoro-onnx",
        artifacts: {
          model: "model_quantized.onnx",
          config: "config.json",
          tokenizer: "tokenizer.json",
          tokenizerConfig: "tokenizer_config.json",
        },
        voices: [{ name: "af_heart" }, { name: "af_bella" }],
        defaultVoice: "af_heart",
      },
    };
    expect(catalogSchema.safeParse([kokoro]).success).toBe(true);
    expect(
      catalogSchema.safeParse([
        {
          ...kokoro,
          ttsRuntime: { ...kokoro.ttsRuntime, defaultVoice: "af_bella" },
        },
      ]).success,
    ).toBe(true);
    expect(
      catalogSchema.safeParse([
        {
          ...kokoro,
          ttsRuntime: { ...kokoro.ttsRuntime, defaultVoice: "af_alloy" },
        },
      ]).success,
    ).toBe(false);
  });

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

    expect(catalogSchema.safeParse([singleFile, sharded]).success).toBe(true);
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
          filename: "../model.gguf",
          expectedSizeBytes: 1,
          sha256: checksum,
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
      expect(catalogSchema.safeParse([model(artifacts)]).success).toBe(false);
    }
  });

  test("rejects empty or incompatible modality contracts", () => {
    const artifacts = [
      {
        sourcePath: "model.gguf",
        filename: "model.gguf",
        expectedSizeBytes: 10,
        sha256: checksum,
        role: "primary",
      },
    ];

    expect(
      catalogSchema.safeParse([{ ...model(artifacts), inputModalities: [] }])
        .success,
    ).toBe(false);
    expect(
      catalogSchema.safeParse([
        { ...model(artifacts), outputModalities: ["unknown"] },
      ]).success,
    ).toBe(false);
  });

  test("requires complete immutable sources for artifact overrides", () => {
    const invalidSources = [
      { repositoryUrl: "https://huggingface.co/test/encoder" },
      {
        repositoryUrl: "https://huggingface.co/test/encoder",
        revision: "main",
      },
      { revision: "d".repeat(40) },
      ...[
        "https://github.com/madebyollin/taehv/blob/main/safetensors/taew2_2.safetensors",
        "https://github.com/madebyollin/taehv/tree/main",
        "https://github.com/madebyollin",
        "https://github.com/madebyollin/taehv?ref=main",
        "https://github.com/madebyollin/taehv#main",
        "https://github.com/madebyollin/taehv.git",
        "http://github.com/madebyollin/taehv",
        "https://user:secret@github.com/madebyollin/taehv",
        "https://github.com/madebyollin/../taehv",
      ].map((repositoryUrl) => ({ repositoryUrl, revision: "d".repeat(40) })),
    ];

    for (const source of invalidSources) {
      expect(
        catalogSchema.safeParse([
          model([
            {
              sourcePath: "model.gguf",
              filename: "model.gguf",
              expectedSizeBytes: 10,
              sha256: checksum,
              role: "primary",
              source,
            },
          ]),
        ]).success,
      ).toBe(false);
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

  test("records verified packaged modalities and context limits", () => {
    for (const [modelId, contextWindowTokens] of verifiedContextWindows) {
      expect(byId(modelId)).toMatchObject({
        inputModalities: ["text"],
        outputModalities: ["text"],
        contextWindowTokens,
      });
    }

    const gemma = byId("gemma-3-12b-it-q4_k_m");
    expect(gemma?.contextWindowTokens).toBeNull();
    expect(gemma).toMatchObject({
      inputModalities: ["text"],
      outputModalities: ["text"],
    });
    expect(capabilityIds(gemma)).not.toContain("vision");
    expect(gemma?.artifacts).toEqual([
      expect.objectContaining({ role: "primary" }),
    ]);
  });

  test("does not advertise tool calling for the unqualified Qwen 1.5B artifact", () => {
    const qwen = byId("qwen2.5-coder-1.5b-instruct-q4_k_m");

    expect(capabilityIds(qwen)).not.toContain("tool-calling");
  });

  test("pins the complete Qwen3 TTS base artifact set", () => {
    const tts = CATALOG.find(
      ({ modelId }) => modelId === "qwen3-tts-1.7b-base-q4_k_m",
    );
    expect(tts?.kind).toBe("tts");
    expect(tts?.repositoryRevision).toBe(
      "ca27d74bc954b73dadab5b71ca265d87fc861a7c",
    );
    expect(
      tts?.artifacts.map(
        ({ filename, expectedSizeBytes, sha256, role, source }) => ({
          filename,
          expectedSizeBytes,
          sha256,
          role,
          source: source ?? null,
        }),
      ),
    ).toEqual([
      {
        filename: "Qwen3-TTS-12Hz-1.7B-Base-Q4_K_M.gguf",
        expectedSizeBytes: 1_035_965_280,
        sha256:
          "8d18c94acb2addd042f97da63c98be144eafa76d0d9495177eab65130cf85129",
        role: "primary",
        source: null,
      },
      {
        filename: "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf",
        expectedSizeBytes: 446_422_912,
        sha256:
          "6fd65188839bcd6ecc91b277ad471e22a0edfada4699a0fe82f1165c18cfcce2",
        role: "supplementary",
        source: null,
      },
      {
        filename: "qwen3-tts-harbor.wav",
        expectedSizeBytes: 480_044,
        sha256:
          "4bd75d0ef0ad3f4e82ac075eab2a132651d2463f83bec210edeeccaf69294886",
        role: "supplementary",
        source: {
          repositoryUrl: "https://huggingface.co/kyutai/tts-voices",
          revision: "323332d33f997de8394f24a193e1a76df720e01a",
        },
      },
      {
        filename: "qwen3-tts-willow.wav",
        expectedSizeBytes: 480_044,
        sha256:
          "8edd516de8c2171b67757cacb29e1effd3e6a8b78f5d6b035069273fadefac2b",
        role: "supplementary",
        source: {
          repositoryUrl: "https://huggingface.co/kyutai/tts-voices",
          revision: "323332d33f997de8394f24a193e1a76df720e01a",
        },
      },
    ]);
    expect(tts?.ttsRuntime).toEqual({
      kind: "llama-tts",
      projectorArtifactFilename: "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf",
      referenceVoices: [
        {
          name: "harbor",
          artifactFilename: "qwen3-tts-harbor.wav",
          license: "CC0-1.0",
          provenanceUrl:
            "https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations",
        },
        {
          name: "willow",
          artifactFilename: "qwen3-tts-willow.wav",
          license: "CC0-1.0",
          provenanceUrl:
            "https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations",
        },
      ],
    });
  });

  test("pins the experimental Wan Q8 video bundle and bounded launch profile", () => {
    expect(byId("wan2.1-t2v-1.3b-q8_0")).toMatchObject({
      kind: "video",
      repositoryRevision: "5a512b15fc35d1b67a074cfe55a591be9e9ef9b5",
      artifacts: [
        {
          expectedSizeBytes: 1_535_768_800,
          sha256:
            "30a44f695b4275a915810120360d6fd26152ec303c2226b5152ec33a93c380e4",
          role: "primary",
        },
        {
          expectedSizeBytes: 6_043_068_256,
          sha256:
            "2521d4de0bf9e1cc6549866463ceae85e4ec3239bc6063f7488810be39033bbc",
          role: "supplementary",
        },
        {
          expectedSizeBytes: 253_815_318,
          sha256:
            "2fc39d31359a4b0a64f55876d8ff7fa8d780956ae2cb13463b0223e15148976b",
          role: "supplementary",
        },
      ],
      videoRuntime: {
        mode: "t2v",
        qualification: {
          jobDeadlineMs: 10 * 60 * 1_000,
          maxWidth: 320,
          maxHeight: 320,
          maxFrames: 33,
          fps: 16,
          generation: {
            sampler: "euler",
            scheduler: "discrete",
            steps: 20,
            cfgScale: 6,
            flowShift: 3,
            seed: 42,
          },
          launchOptions: {
            cpuOffload: true,
            diffusionFlashAttention: true,
            vaeConvDirect: false,
          },
        },
        supportedTargets: [
          {
            platform: "darwin",
            architecture: "arm64",
            accelerator: "apple-unified",
          },
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
    });
  });

  test("uses model source by default and a pinned override for supplementary artifacts", () => {
    const parsed = catalogSchema.parse([
      model([
        {
          sourcePath: "model.gguf",
          filename: "model.gguf",
          expectedSizeBytes: 10,
          sha256: checksum,
          role: "primary",
        },
        {
          sourcePath: "encoder/model.safetensors",
          filename: "model.safetensors",
          expectedSizeBytes: 8,
          sha256: "b".repeat(64),
          role: "supplementary",
          source: {
            repositoryUrl: "https://huggingface.co/test/encoder",
            revision: "d".repeat(40),
          },
        },
      ]),
    ])[0]!;

    expect(artifactDownloadUrl(parsed, parsed.artifacts[0]!)).toBe(
      `${parsed.source}/resolve/${parsed.repositoryRevision}/model.gguf`,
    );
    expect(artifactDownloadUrl(parsed, parsed.artifacts[1]!)).toBe(
      "https://huggingface.co/test/encoder/resolve/" +
        `${"d".repeat(40)}/encoder/model.safetensors`,
    );
  });

  test("pins the S2V speech encoder and bounds its full-VAE Linux profile", () => {
    const speechVideo = byId("wan2.2-s2v-14b-fp8");
    expect(speechVideo).toMatchObject({
      qualificationState: "experimental",
      repositoryRevision: "c4f60d30c55a624e35427060fdd217579a6c1d77",
      inputModalities: ["text", "image", "audio"],
      outputModalities: ["video"],
      capabilities: [{ id: "speech-to-video", source: "declared" }],
      outputFormat: "avi",
      experimentalRuntime: true,
      catch:
        "Apache-2.0 licenses for the pinned artifacts. Output audio is the supplied driving track, not generated speech.",
      videoRuntime: {
        mode: "s2v",
        artifacts: {
          decoder: { kind: "vae", artifactFilename: "wan_2.1_vae.safetensors" },
          audioEncoder: "wav2vec2_large_english_fp16.safetensors",
        },
        qualification: {
          maxWidth: 480,
          maxHeight: 640,
          maxFrames: 33,
          fps: 16,
          jobDeadlineMs: 30 * 60 * 1_000,
          generation: {
            sampler: "euler",
            scheduler: "discrete",
            steps: 20,
            cfgScale: 6,
            flowShift: 3,
            seed: 42,
          },
          launchOptions: {
            cpuOffload: true,
            diffusionFlashAttention: true,
            vaeConvDirect: false,
          },
        },
        supportedTargets: [
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
    });
    expect(
      speechVideo?.artifacts.find(({ role }) => role === "primary"),
    ).toMatchObject({
      sourcePath:
        "split_files/diffusion_models/wan2.2_s2v_14B_fp8_scaled.safetensors",
      expectedSizeBytes: 16_394_832_474,
      sha256:
        "140e75af5534ac3d91e710d9df756f7032addd64b341ba2c1c70e3e6da9aa216",
    });
    expect(
      speechVideo?.artifacts.find(
        ({ filename }) =>
          filename === "wav2vec2_large_english_fp16.safetensors",
      ),
    ).toMatchObject({
      sourcePath:
        "split_files/audio_encoders/wav2vec2_large_english_fp16.safetensors",
      expectedSizeBytes: 630_997_322,
      sha256:
        "f0017a43ea57ef6b3d4866be607844bbd8cada6d30966f7d70044ed0d63d3f9e",
    });
    for (const filename of [
      "umt5-xxl-encoder-Q8_0.gguf",
      "wan_2.1_vae.safetensors",
    ]) {
      expect(
        speechVideo?.artifacts.find(
          (artifact) => artifact.filename === filename,
        ),
      ).toEqual(
        byId("wan2.1-t2v-1.3b-q8_0")?.artifacts.find(
          (artifact) => artifact.filename === filename,
        ),
      );
    }
  });

  test("pins the FastWan decoder, shared encoder, and measured Linux profile", () => {
    const fastWan = byId("fastwan2.2-ti2v-5b-q6_k");
    expect(fastWan).toMatchObject({
      repositoryRevision: "3e8fe5537b1200654868aa24ea8d0f4012fb3a1e",
      videoRuntime: {
        mode: "ti2v",
        artifacts: {
          decoder: { kind: "tae", artifactFilename: "taew2_2.safetensors" },
        },
        qualification: {
          jobDeadlineMs: 10 * 60 * 1_000,
          maxWidth: 480,
          maxHeight: 832,
          maxFrames: 81,
          fps: 16,
          generation: {
            sampler: "euler",
            scheduler: "lcm",
            steps: 3,
            cfgScale: 1,
            flowShift: 3,
            seed: 42,
          },
          launchOptions: {
            cpuOffload: true,
            diffusionFlashAttention: true,
            vaeConvDirect: true,
          },
        },
        supportedTargets: [
          {
            platform: "darwin",
            architecture: "arm64",
            accelerator: "apple-unified",
          },
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
    });
    expect(capabilityIds(fastWan)).toContain("image-to-video");
    expect(
      fastWan?.artifacts.find(({ role }) => role === "primary"),
    ).toMatchObject({
      expectedSizeBytes: 4_210_247_200,
      sha256:
        "416a87e30f2328dbefd7666ac90b395ead74f443748ff31c83483ac4ac6121cc",
    });
    expect(
      fastWan?.artifacts.find(
        ({ filename }) => filename === "umt5-xxl-encoder-Q8_0.gguf",
      ),
    ).toEqual(
      byId("wan2.1-t2v-1.3b-q8_0")?.artifacts.find(
        ({ filename }) => filename === "umt5-xxl-encoder-Q8_0.gguf",
      ),
    );
    expect(
      fastWan?.artifacts.find(
        ({ filename }) => filename === "taew2_2.safetensors",
      ),
    ).toMatchObject({
      expectedSizeBytes: 22_848_048,
      sha256:
        "b84609b2a133d48434bd9636bfcb44bf05168dc436e2d3cecf26256faa1f5325",
      source: {
        repositoryUrl: "https://github.com/madebyollin/taehv",
        revision: "fa579a9a726b0a55951998d73e309bfdf0abd342",
      },
    });
  });

  test("resolves pinned Hugging Face and GitHub artifacts", () => {
    const artifact = {
      sourcePath: "safetensors/taew2_2.safetensors",
      filename: "taew2_2.safetensors",
      expectedSizeBytes: 22_848_048,
      sha256:
        "b84609b2a133d48434bd9636bfcb44bf05168dc436e2d3cecf26256faa1f5325",
      role: "primary",
    };
    const source = {
      repositoryUrl: "https://github.com/madebyollin/taehv/",
      revision: "fa579a9a726b0a55951998d73e309bfdf0abd342",
    };
    for (const candidate of [
      model([{ ...artifact, source }]),
      {
        ...model([artifact]),
        source: source.repositoryUrl,
        repositoryRevision: source.revision,
      },
    ]) {
      const parsed = catalogSchema.parse([candidate])[0]!;
      expect(artifactDownloadUrl(parsed, parsed.artifacts[0]!)).toBe(
        "https://raw.githubusercontent.com/madebyollin/taehv/fa579a9a726b0a55951998d73e309bfdf0abd342/safetensors/taew2_2.safetensors",
      );
    }
    const encoder = catalogSchema.parse([
      model([
        {
          ...artifact,
          sourcePath: "umt5-xxl-encoder-Q8_0.gguf",
          source: {
            repositoryUrl:
              "https://huggingface.co/city96/umt5-xxl-encoder-gguf",
            revision: "b535255bee98c2b0a59ea7c0ae2dcd0c6657b3b7",
          },
        },
      ]),
    ])[0]!;
    expect(artifactDownloadUrl(encoder, encoder.artifacts[0]!)).toBe(
      "https://huggingface.co/city96/umt5-xxl-encoder-gguf/resolve/b535255bee98c2b0a59ea7c0ae2dcd0c6657b3b7/umt5-xxl-encoder-Q8_0.gguf",
    );
    for (const sourcePath of [
      "../main/file",
      "safetensors/../../main/file",
      "./file",
    ]) {
      expect(
        catalogSchema.safeParse([model([{ ...artifact, source, sourcePath }])])
          .success,
      ).toBe(false);
    }
  });
});

describe("catalog-only video candidates", () => {
  test.each([
    "wan2.2-ti2v-5b-q6_k",
    "wan2.2-t2v-a14b-q4_k_m",
    "ltx-2.3-22b-distilled-1.1-q4_k_m",
    "hunyuanvideo-1.5-t2v-480p-q4_k_m",
  ])("lists %s without admitting native generation", (id) => {
    const entry = byId(id);
    if (!entry) throw new Error(`Missing catalog candidate: ${id}`);
    expect(entry.videoCatalogOnly).toBe(true);
    expect(entry.videoRuntime).toBeUndefined();
    expect(entry.capabilities).toEqual([]);
    expect(entry.diffusionWeightsOnly).toBe(true);
    expect(legacyFeatures(entry)).toEqual([
      "catalog-only",
      "diffusion-weights-only",
    ]);
    expect(entry.storageGb).toBeCloseTo(
      entry.artifacts.reduce(
        (bytes, artifact) => bytes + (artifact.expectedSizeBytes ?? 0),
        0,
      ) / 1e9,
      2,
    );
    expect(
      qualifiedVideoInput(
        {
          model: id,
          prompt: "A bird in flight",
          width: 480,
          height: 832,
          frames: 81,
          fps: 16,
          input: { kind: "text" },
        },
        entry,
      ),
    ).toBeUndefined();
    expect(
      catalogSchema.safeParse([{ ...entry, videoCatalogOnly: undefined }])
        .success,
    ).toBe(false);
    expect(catalogSchema.safeParse([{ ...entry, kind: "image" }]).success).toBe(
      false,
    );
    const runnable = byId("wan2.1-t2v-1.3b-q8_0");
    if (!runnable?.videoRuntime)
      throw new Error("Missing runnable video fixture");
    expect(
      catalogSchema.safeParse([{ ...runnable, videoCatalogOnly: true }])
        .success,
    ).toBe(false);
    for (const artifact of entry.artifacts) {
      expect(artifactDownloadUrl(entry, artifact)).toContain(
        `/resolve/${entry.repositoryRevision}/`,
      );
    }
  });

  test("includes both Wan experts and distinguishes conditional licenses", () => {
    expect(
      byId("wan2.2-t2v-a14b-q4_k_m")?.artifacts.map(({ filename }) => filename),
    ).toEqual([
      "wan2.2_t2v_high_noise_14B_Q4_K_M.gguf",
      "wan2.2_t2v_low_noise_14B_Q4_K_M.gguf",
    ]);
    for (const id of ["wan2.2-ti2v-5b-q6_k", "wan2.2-t2v-a14b-q4_k_m"]) {
      expect(byId(id)?.commercialStatus).toBe("open");
    }
    for (const id of [
      "ltx-2.3-22b-distilled-1.1-q4_k_m",
      "hunyuanvideo-1.5-t2v-480p-q4_k_m",
    ]) {
      expect(byId(id)?.commercialStatus).toBe("conditional");
    }
  });
});

describe("FLUX Klein image additions", () => {
  test.each([
    {
      id: "flux2-klein-4b-q8_0",
      bytes: 7134122044,
      deviceBytes: 14478475264,
      hostBytes: 8412053504,
      confidence: "measured",
      encoder: "Qwen3-4B-Q4_K_M.gguf",
      commercialStatus: "open",
    },
    {
      id: "flux2-klein-9b-q4_0",
      bytes: 10980203836,
      deviceBytes: 18496396604,
      // No FA-on measurement (artifacts exceed the 8 GB qualification cap), so
      // the derived estimate applies; it stays above the FA-off peak.
      hostBytes: 11517074748,
      confidence: "derived",
      encoder: "Qwen3-8B-Q4_K_M.gguf",
      commercialStatus: "conditional",
    },
  ])(
    "resolves $id into a complete image launch plan",
    ({
      id,
      bytes,
      deviceBytes,
      hostBytes,
      confidence,
      encoder,
      commercialStatus,
    }) => {
      const entry = byId(id);
      if (!entry?.imageRuntime) throw new Error(`Missing image profile: ${id}`);
      const artifactBytes = entry.artifacts.reduce(
        (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
        0,
      );
      expect(artifactBytes).toBe(bytes);
      expect(entry.commercialStatus).toBe(commercialStatus);
      expect(capabilityIds(entry)).toEqual(["text-to-image"]);
      const plan = resolveImageLaunchPlan({
        runtimeId: `image:${id}`,
        root: "/localbase",
        modelsDirectory: "/models/image",
        modelId: id,
        modelFile: entry.imageRuntime.artifacts.diffusionModel,
        host: "127.0.0.1",
        port: 8083,
        model: entry,
        artifactBytes,
        imageRuntime: entry.imageRuntime,
      });
      expect(buildSdImageServerArgs(plan)).toEqual([
        "--diffusion-model",
        join("/models/image", entry.imageRuntime.artifacts.diffusionModel),
        "--vae",
        "/models/image/flux2-vae.safetensors",
        "--llm",
        join("/models/image", encoder),
        "--sampling-method",
        "euler",
        "--steps",
        "4",
        "--cfg-scale",
        "1",
        "--diffusion-fa",
        "--listen-ip",
        "127.0.0.1",
        "--listen-port",
        "8083",
      ]);
      expect(plan.memoryDemand).toEqual({
        unifiedBytes: deviceBytes,
        hostBytes,
        acceleratorBytes: deviceBytes,
        confidence,
      });
    },
  );
});

describe("Cloudflare open-weight image additions", () => {
  test.each([
    {
      id: "flux1-schnell-q4_0",
      bytes: 17253896444,
      runtimeKind: "diffusion-flux1",
      commercialStatus: "open",
    },
    {
      id: "dreamshaper-8-lcm",
      bytes: 2133804992,
      runtimeKind: "checkpoint",
      commercialStatus: "open",
    },
  ])(
    "pins a complete and runnable $id manifest",
    ({ id, bytes, runtimeKind, commercialStatus }) => {
      const entry = byId(id);
      expect(entry).toBeDefined();
      expect(
        entry?.artifacts.reduce(
          (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
          0,
        ),
      ).toBe(bytes);
      expect(entry?.imageRuntime?.kind).toBe(runtimeKind);
      expect(entry?.commercialStatus).toBe(commercialStatus);
      expect(capabilityIds(entry)).toEqual(["text-to-image"]);
    },
  );
});

describe("derived memory estimates", () => {
  test.each(CATALOG.map((model) => [model.modelId, model] as const))(
    "%s estimate and measured demand cover its artifacts",
    (_id, model) => {
      const demands = [
        estimateCatalogModelMemory(model).demand,
        catalogModelDemand(model),
      ];
      const artifacts = totalArtifactBytes(model);

      const hostOnly = new Set<string>(
        model.imageRuntime?.kind === "diffusion-flux1"
          ? [
              model.imageRuntime.artifacts.clipL,
              model.imageRuntime.artifacts.t5xxl,
            ]
          : [],
      );
      const resident = model.artifacts.filter(
        (artifact) => !hostOnly.has(artifact.filename),
      );
      const offload =
        model.videoRuntime?.qualification.launchOptions.cpuOffload ?? false;
      // Kokoro runs on the CPU ONNX provider and holds no accelerator memory.
      const cpuOnly = model.ttsRuntime?.kind === "kokoro-onnx";
      const required = cpuOnly
        ? 0
        : offload
          ? Math.max(
              ...resident.map((artifact) => artifact.expectedSizeBytes ?? 0),
            )
          : resident.reduce(
              (sum, artifact) => sum + (artifact.expectedSizeBytes ?? 0),
              0,
            );
      for (const demand of demands) {
        expect(demand.unifiedBytes).toBeGreaterThanOrEqual(artifacts);
        expect(demand.hostBytes).toBeGreaterThanOrEqual(artifacts);
        expect(demand.acceleratorBytes).toBeGreaterThanOrEqual(required);
      }
    },
  );

  test.each(CATALOG.map((model) => [model.modelId, model] as const))(
    "%s computed minimum covers its required artifacts",
    (_id, model) => {
      const minimum = computedMinVramGb(model);
      expect(Number.isInteger(minimum)).toBe(true);
      expect(minimum).toBeGreaterThanOrEqual(1);
      const figure = modelMemoryFigure(model);
      expect(minimum).toBe(minimumMemoryGb(figure.demandBytes));
      expect(minimum * 1024 ** 3).toBeGreaterThan(figure.demandBytes);
      expect(figure.source).not.toBe("observed");
    },
  );
});
