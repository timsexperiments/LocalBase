import { describe, expect, test } from "bun:test";
import { CATALOG, artifactDownloadUrl, byId, catalogSchema } from "./catalog";

const checksum = "a".repeat(64);

const verifiedContextWindows = new Map<string, number>([
  ["qwen2.5-coder-1.5b-instruct-q4_k_m", 32_768],
  ["qwen3-coder-next-q4_k_m", 262_144],
  ["gpt-oss-20b-q4_k_m", 131_072],
  ["qwen3.5-27b-q4_k_m", 262_144],
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
        estimatedMemoryDemand: {
          unifiedBytes: 20,
          hostBytes: 20,
          acceleratorBytes: 10,
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
        estimatedMemoryDemand: {
          unifiedBytes: 30,
          hostBytes: 20,
          acceleratorBytes: 10,
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
    expect(gemma?.features).not.toContain("vision");
    expect(gemma?.artifacts).toEqual([
      expect.objectContaining({ role: "primary" }),
    ]);
  });

  test("does not advertise tool calling for the unqualified Qwen 1.5B artifact", () => {
    const qwen = byId("qwen2.5-coder-1.5b-instruct-q4_k_m");

    expect(qwen?.features).not.toContain("tool-calling");
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
      minVramGb: 12,
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
        estimatedMemoryDemand: {
          unifiedBytes: 24 * 1024 ** 3,
          hostBytes: 16 * 1024 ** 3,
          acceleratorBytes: 9 * 1024 ** 3,
        },
        supportedTargets: [
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

  test("pins the FastWan decoder, shared encoder, and measured Linux profile", () => {
    const fastWan = byId("fastwan2.2-ti2v-5b-q6_k");
    expect(fastWan).toMatchObject({
      repositoryRevision: "3e8fe5537b1200654868aa24ea8d0f4012fb3a1e",
      videoRuntime: {
        mode: "t2v",
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
        estimatedMemoryDemand: {
          unifiedBytes: 24 * 1024 ** 3,
          hostBytes: 16 * 1024 ** 3,
          acceleratorBytes: 8 * 1024 ** 3,
        },
        supportedTargets: [
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
    });
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
