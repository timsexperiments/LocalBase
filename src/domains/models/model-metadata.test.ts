import { expect, test } from "bun:test";
import { byId, CATALOG, legacyFeatures, type ModelSpec } from "../../catalog";
import { formerCatalogEntry } from "../../catalog/former";
import { defaultConfig } from "../../manager";
import { createRuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import {
  gibibyte,
  type HostMemorySnapshot,
  type MemoryTopology,
} from "../runtime/memory-safety";
import {
  modelMetadataSchema,
  projectModelMetadata,
  projectModelMetadataList,
} from "./model-metadata";
import { resolveModelCapabilities } from "./capability-detection";
import { modelsSchema } from "../../ui/client";

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const multipartModelId = "qwen3-coder-next-q4_k_m";
const speechModelId = "qwen3-tts-1.7b-base-q4_k_m";
const embeddingModelId = "qwen3-embedding-0.6b-q8_0";
const videoModelId = "wan2.1-t2v-1.3b-q8_0";

// Several fixtures are former built-in entries; their pinned data still holds.
function catalogModel(id = modelId) {
  const model = byId(id) ?? formerCatalogEntry(id);
  if (!model) throw new Error(`Expected catalog model ${id}.`);
  return model;
}

function runtimeSnapshots() {
  return {
    llm: createRuntimeLifecycleSnapshot({
      modality: "llm",
      configured: true,
      state: "running",
      modelId,
      runtimeId: "llm:test:1",
      admission: { kind: "unknown" },
      configuredSlots: 4,
      queue: {
        waiting: 0,
        active: 0,
        immediateDispatchAvailable: true,
        capacity: 6,
        maxWaitMs: 60_000,
        accepting: true,
      },
    }),
    stt: createRuntimeLifecycleSnapshot({
      modality: "stt",
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    }),
    tts: createRuntimeLifecycleSnapshot({
      modality: "tts",
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    }),
    image: createRuntimeLifecycleSnapshot({
      modality: "image",
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    }),
    video: createRuntimeLifecycleSnapshot({
      modality: "video",
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    }),
  };
}

function hostMemory(): {
  topology: MemoryTopology;
  snapshot: HostMemorySnapshot;
} {
  return {
    topology: {
      kind: "unified",
      system: { id: "system", capacityBytes: 64 * gibibyte },
    },
    snapshot: {
      capturedAtMs: 1,
      pools: [
        {
          poolId: "system",
          availability: "available",
          availableBytes: 40 * gibibyte,
          pressure: "normal",
        },
      ],
    },
  };
}

test("projects catalog facts separately from observed device state", () => {
  const config = defaultConfig("/tmp/localbase-model-metadata");
  config.selectedLlmModels = [modelId];
  config.selectedSttModels = [];
  config.selectedTtsModels = [];
  config.selectedImageModels = [];
  config.selectedVideoModels = [];
  const metadata = projectModelMetadata(catalogModel(), {
    catalog: [catalogModel()],
    config,
    installations: new Map([[modelId, true]]),
    runtimes: runtimeSnapshots(),
  });

  expect(metadata).toMatchObject({
    object: "localbase.model",
    id: modelId,
    catalog: {
      name: "Qwen2.5-Coder",
      displayName: "Qwen2.5 Coder 1.5B Instruct",
      quantization: "Q4_K_M",
      capabilities: null,
      inputModalities: ["text"],
      outputModalities: ["text"],
      contextWindowTokens: 32_768,
      maxOutputTokens: null,
    },
    device: {
      selected: true,
      installed: true,
      runtime: {
        configured: true,
        state: "running",
        executionSlots: 4,
        activeAdmissions: 0,
        availableExecutionSlots: 4,
        immediateDispatchAvailable: null,
        queuedRequests: 0,
        waitingCapacity: 6,
        availableWaitingCapacity: 6,
      },
    },
  });
  expect(metadata.catalog.artifacts[0]?.sha256).toMatch(/^[a-f0-9]{64}$/);
  expect(metadata.catalog.artifacts[0]?.sizeBytes).toBeGreaterThan(0);
  expect(metadata.catalog.revision).toMatch(/^[a-f0-9]{40}$/);
});

test("includes every declared artifact in multi-file model identity", () => {
  const model = catalogModel(multipartModelId);
  const metadata = projectModelMetadata(model, {
    catalog: [model],
    config: defaultConfig("/tmp/localbase-model-metadata-artifacts"),
    installations: new Map([[multipartModelId, false]]),
    runtimes: runtimeSnapshots(),
  });

  expect(metadata.catalog.artifacts).toHaveLength(4);
  expect(metadata.catalog.artifacts).toEqual(
    expect.arrayContaining([
      expect.objectContaining({ role: "primary" }),
      expect.objectContaining({ role: "supplementary" }),
    ]),
  );
});

test("reports the embedding-only capability and its bounded dimensions", () => {
  const model = catalogModel(embeddingModelId);
  const metadata = projectModelMetadata(model, {
    catalog: [model],
    config: defaultConfig("/tmp/localbase-model-metadata-embedding"),
    installations: new Map([[embeddingModelId, false]]),
    runtimes: runtimeSnapshots(),
  });

  expect(metadata.catalog).toMatchObject({
    capabilities: {
      kind: "embedding",
      dimensions: { minimum: 1024, maximum: 1024 },
    },
    contextWindowTokens: 32_768,
    maxOutputTokens: null,
  });
});

test("reports only the supported cold speech contract", () => {
  const model = catalogModel(speechModelId);
  const config = defaultConfig("/tmp/localbase-model-metadata-speech");
  config.selectedTtsModels = [speechModelId];
  config.activeTtsModel = speechModelId;
  expect(
    model.ttsRuntime?.kind === "llama-tts"
      ? model.ttsRuntime.referenceVoices
      : undefined,
  ).toEqual([
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
  ]);
  const metadata = projectModelMetadata(model, {
    catalog: [model],
    config,
    installations: new Map([[speechModelId, true]]),
    runtimes: runtimeSnapshots(),
    customVoices: [{ id: "voice-0123456789ab", name: "Narrator" }],
  });

  expect(metadata.catalog.capabilities).toEqual({
    kind: "speech",
    outputFormats: ["wav"],
    voice: {
      selection: "catalog-reference",
      requestValues: ["default", "harbor", "willow", "voice-0123456789ab"],
      defaultRequestValue: "default",
      references: [
        {
          name: "harbor",
          license: "CC0-1.0",
          provenanceUrl:
            "https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations",
        },
        {
          name: "willow",
          license: "CC0-1.0",
          provenanceUrl:
            "https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations",
        },
      ],
      custom: [{ requestValue: "voice-0123456789ab", name: "Narrator" }],
    },
    residency: "cold-per-request",
  });
  expect(metadata.catalog.inputModalities).toEqual(["text"]);
  expect(metadata.catalog.outputModalities).toEqual(["audio"]);
  expect(metadata.catalog.contextWindowTokens).toBeNull();
  expect(metadata.catalog.maxOutputTokens).toBeNull();
});

test("projects qualified video limits and mode without exposing runtime internals", () => {
  const video = catalogModel(videoModelId);
  if (!video.videoRuntime) throw new Error("Expected video runtime profile.");
  const speechVideo = {
    ...video,
    modelId: "test-s2v",
    capabilities: [{ id: "speech-to-video", source: "declared" }],
    outputFormat: "avi",
    inputModalities: ["text", "audio", "image"],
    videoRuntime: {
      ...video.videoRuntime,
      mode: "s2v",
      artifacts: {
        ...video.videoRuntime.artifacts,
        decoder: { kind: "vae", artifactFilename: "test-vae.safetensors" },
        audioEncoder: "test-audio.safetensors",
      },
      qualification: {
        ...video.videoRuntime.qualification,
        maxWidth: 832,
        maxHeight: 480,
        maxFrames: 81,
        fps: 24,
        jobDeadlineMs: 900_000,
      },
    },
  } satisfies ModelSpec;
  const catalog = [video, catalogModel("fastwan2.2-ti2v-5b-q6_k"), speechVideo];
  const list = projectModelMetadataList(
    {
      catalog,
      config: defaultConfig("/tmp/localbase-model-metadata-video"),
      installations: new Map(),
      runtimes: runtimeSnapshots(),
    },
    hostMemory(),
  );

  for (const [index, model] of catalog.entries()) {
    if (!model.videoRuntime) throw new Error("Expected video runtime profile.");
    const { qualification, mode } = model.videoRuntime;
    expect(list.data[index]?.catalog.capabilities).toEqual({
      kind: "video",
      mode,
      width: qualification.maxWidth,
      height: qualification.maxHeight,
      frames: qualification.maxFrames,
      fps: qualification.fps,
      jobDeadlineMs: qualification.jobDeadlineMs,
      outputFormats: ["mp4"],
    });
    expect(list.data[index]?.catalog.features).toEqual(legacyFeatures(model));
  }
});

test("uses the strict response schemas for lists and entries", () => {
  const model = catalogModel();
  const input = {
    catalog: CATALOG,
    config: defaultConfig("/tmp/localbase-model-metadata-strict"),
    installations: new Map([[modelId, false]]),
    runtimes: runtimeSnapshots(),
  };
  const list = projectModelMetadataList(input, hostMemory());

  expect(list.data).toHaveLength(CATALOG.length);
  expect(list.host.memory).toEqual({
    kind: "unified",
    system: {
      capacityBytes: 64 * gibibyte,
      availableBytes: 40 * gibibyte,
    },
    accelerators: [],
  });
  for (const [index, model] of CATALOG.entries()) {
    expect(list.data[index]?.catalog.features).toEqual(legacyFeatures(model));
  }
  expect(
    modelMetadataSchema.safeParse({ ...list.data[0], extra: true }).success,
  ).toBe(false);

  const metadata = projectModelMetadata(model, input);
  for (const features of [undefined, "tool-calling", [1]]) {
    expect(
      modelMetadataSchema.safeParse({
        ...metadata,
        catalog: { ...metadata.catalog, features },
      }).success,
    ).toBe(false);
  }

  const video = projectModelMetadata(catalogModel(videoModelId), input);
  const capabilities = video.catalog.capabilities;
  if (capabilities?.kind !== "video")
    throw new Error("Expected video capability.");
  const invalidCapabilities = [
    { ...capabilities, extra: true },
    { ...capabilities, mode: "i2v" },
    { ...capabilities, outputFormats: ["avi"] },
    { ...capabilities, outputFormats: ["mp4", "mp4"] },
    { ...capabilities, dimensions: { minimum: 1, maximum: 2 } },
    { ...capabilities, kind: "speech" },
  ];
  for (const field of ["width", "height", "frames", "fps", "jobDeadlineMs"]) {
    for (const value of [undefined, 0, -1, 1.5, "16"]) {
      invalidCapabilities.push({ ...capabilities, [field]: value });
    }
  }
  for (const invalid of invalidCapabilities) {
    expect(
      modelMetadataSchema.safeParse({
        ...video,
        catalog: { ...video.catalog, capabilities: invalid },
      }).success,
    ).toBe(false);
  }
});

test("the browser accepts metadata for every catalog model", () => {
  const list = projectModelMetadataList(
    {
      catalog: CATALOG,
      config: defaultConfig("/tmp/localbase-model-metadata-browser"),
      installations: new Map(),
      runtimes: runtimeSnapshots(),
    },
    hostMemory(),
  );

  const parsed = modelsSchema.safeParse(list);
  expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
  expect(
    list.data.filter(
      (model) => model.catalog.displayName === model.catalog.name,
    ),
  ).toEqual([]);
});

test("projects resolved capabilities with sources and mismatches", () => {
  const config = defaultConfig("/tmp/localbase-model-metadata");
  // Without its verified-unsupported entry, runtime flags decide.
  const { unsupportedCapabilities: _unsupported, ...model } = catalogModel();
  const input = {
    catalog: [model],
    config,
    installations: new Map([[modelId, true]]),
    runtimes: runtimeSnapshots(),
  };
  const declaredOnly = projectModelMetadata(model, input);
  expect(declaredOnly.catalog.capabilityResolution.mismatches).toEqual([]);
  expect(
    declaredOnly.catalog.capabilityResolution.capabilities.every(
      ({ source }) => source === "declared",
    ),
  ).toBe(true);
  const legacy = declaredOnly.catalog.features;

  const resolved = resolveModelCapabilities(model, {
    detected: { capabilities: { "long-context": false } },
    runtime: {
      capabilities: { "tool-calling": true },
      inputModalities: ["text"],
    },
  });
  const metadata = projectModelMetadata(model, {
    ...input,
    capabilities: new Map([[modelId, resolved]]),
  });
  expect(modelMetadataSchema.parse(metadata)).toEqual(metadata);
  expect(metadata.catalog.capabilityResolution).toEqual({
    capabilities: resolved.capabilities.map(({ id, source }) => ({
      id,
      source,
    })),
    mismatches: [
      { id: "tool-calling", declared: false, runtime: true, effective: true },
    ],
    inputModalities: { values: ["text"], source: "runtime" },
  });
  // Legacy fields keep describing the catalog declaration.
  expect(metadata.catalog.features).toEqual(legacy);
});

test("a verified-unsupported capability outranks template flags (#328)", () => {
  const config = defaultConfig("/tmp/localbase-model-metadata");
  const model = catalogModel();
  const resolved = resolveModelCapabilities(model, {
    detected: { capabilities: { "tool-calling": true } },
    runtime: {
      capabilities: { "tool-calling": true },
      inputModalities: ["text"],
    },
  });
  expect(resolved.capabilities.map(({ id }) => id)).not.toContain(
    "tool-calling",
  );
  const metadata = projectModelMetadata(model, {
    catalog: [model],
    config,
    installations: new Map([[modelId, true]]),
    runtimes: runtimeSnapshots(),
    capabilities: new Map([[modelId, resolved]]),
  });
  expect(modelMetadataSchema.parse(metadata)).toEqual(metadata);
  expect(metadata.catalog.unsupportedCapabilities).toEqual([
    expect.objectContaining({ id: "tool-calling", verified: true }),
  ]);
  expect(metadata.catalog.capabilityResolution.mismatches).toEqual([
    expect.objectContaining({
      id: "tool-calling",
      declared: false,
      detected: true,
      runtime: true,
      effective: false,
    }),
  ]);
  expect(metadata.catalog.trust).toBe("qualified");
  expect(metadata.catalog.catalogTier).toBe("built-in");

  // Only a verified round-trip overrides it.
  const verified = resolveModelCapabilities(model, {
    runtime: {
      capabilities: { "tool-calling": true },
      verified: { "tool-calling": true },
    },
  });
  expect(verified.capabilities).toContainEqual({
    id: "tool-calling",
    source: "runtime",
  });
  expect(verified.mismatches[0]).toMatchObject({
    id: "tool-calling",
    effective: true,
  });
});

test("projects the memory source, observed peak, and fit for this machine", () => {
  const model = catalogModel();
  const sha256 = model.artifacts[0]!.sha256!;
  const peak = {
    modelId,
    artifactSha256: sha256,
    launchShape: "ctx=8192;slots=1;kv=q8_0/q8_0",
    peakBytes: 3 * gibibyte,
    method: "footprint" as const,
    observedAt: "2026-10-15T00:00:00.000Z",
  };
  const input = {
    catalog: [model],
    config: defaultConfig("/tmp/localbase-model-metadata-memory"),
    installations: new Map([[modelId, true]]),
    runtimes: runtimeSnapshots(),
  };

  const estimated = projectModelMetadata(model, input).catalog.memory;
  expect(estimated.source).toBe("estimate");
  expect(estimated.observed).toBeNull();
  expect(estimated.fit).toBeUndefined();

  const observed = projectModelMetadata(model, {
    ...input,
    memory: new Map([[modelId, { observed: [peak] }]]),
    systemMemoryGb: 64,
  }).catalog.memory;
  expect(observed).toMatchObject({
    minimumVramEstimateGb: 4,
    source: "observed",
    demandBytes: 3 * gibibyte,
    launchShape: peak.launchShape,
    reference: { source: "estimate" },
    observed: { peakBytes: 3 * gibibyte, method: "footprint" },
    fit: { verdict: "fits", systemMemoryGb: 64, headroomGb: 60 },
  });
});

test("names the feed behind a community entry without its query", async () => {
  const { applyCatalogLayers } = await import("../../catalog/sources");
  const base = catalogModel();
  const communityId = "community-metadata-test";
  try {
    applyCatalogLayers([
      {
        tier: "remote",
        entries: [
          {
            path: "https://feeds.example.com/feed.json?token=secret#models[0]",
            data: { ...structuredClone(base), modelId: communityId },
          },
        ],
      },
    ]);
    const model = catalogModel(communityId);
    const metadata = projectModelMetadata(model, {
      catalog: [model],
      config: defaultConfig("/tmp/localbase-model-metadata-feed"),
      installations: new Map([[communityId, false]]),
      runtimes: runtimeSnapshots(),
    });
    expect(metadata.catalog.trust).toBe("community");
    expect(metadata.catalog.catalogTier).toBe("remote");
    expect(metadata.catalog.catalogFeed).toBe(
      "https://feeds.example.com/feed.json",
    );
    expect(
      projectModelMetadata(base, {
        catalog: [base],
        config: defaultConfig("/tmp/localbase-model-metadata-feed"),
        installations: new Map([[base.modelId, false]]),
        runtimes: runtimeSnapshots(),
      }).catalog.catalogFeed,
    ).toBeNull();
  } finally {
    applyCatalogLayers([]);
  }
});

test("projects a feed-measured peak and parses on the client", async () => {
  const { setFeedAnnotations, huggingFaceRepoOf } =
    await import("../../catalog/annotations");
  const { referenceLaunchShape } = await import("./memory-fit");
  const model = catalogModel();
  const launchShape = referenceLaunchShape(model);
  try {
    setFeedAnnotations([
      {
        repo: huggingFaceRepoOf(model.source)!,
        revision: model.repositoryRevision,
        file: model.artifacts[0]!.sourcePath,
        memoryPeaks: [
          {
            variant: "Q4_K_M",
            launchShape,
            peakBytes: 3 * gibibyte,
            backendAcceleratorBytes: 2 * gibibyte,
            hostBufferBytes: 1024,
            peakRssBytes: 3 * gibibyte,
            measuredAt: "2026-10-14",
            hardware: "Feed machine",
          },
        ],
      },
    ]);
    const metadata = projectModelMetadata(model, {
      catalog: [model],
      config: defaultConfig("/tmp/localbase-model-metadata-feed"),
      installations: new Map([[modelId, true]]),
      runtimes: runtimeSnapshots(),
    });
    expect(metadata.catalog.memory).toMatchObject({
      source: "feed",
      launchShape,
      reference: { source: "feed", launchShape },
      feedPeak: {
        peakBytes: 3 * gibibyte,
        launchShape,
        variant: "Q4_K_M",
        hardware: "Feed machine",
        measuredAt: "2026-10-14",
      },
    });
    expect(modelMetadataSchema.parse(metadata)).toBeTruthy();
  } finally {
    setFeedAnnotations([]);
  }
});

test("reports an image LoRA's strength range only for models added with one", () => {
  const base = CATALOG.find(
    (model) => model.kind === "image" && model.imageLaunchOptions,
  )!;
  const plain = projectModelMetadata(base, {
    catalog: [base],
    config: defaultConfig("/tmp/localbase-model-metadata-image"),
    installations: new Map([[base.modelId, false]]),
    runtimes: runtimeSnapshots(),
  });
  expect(plain.catalog.capabilities).toBeNull();

  const withLora = {
    ...base,
    modelId: "pixel-art-lora",
    imageLaunchOptions: {
      ...base.imageLaunchOptions,
      lora: {
        artifactFilename: "pixel-art-lora.safetensors",
        defaultStrength: 0.8,
        trigger: "pixel art",
      },
    },
  } as ModelSpec;
  const metadata = projectModelMetadata(withLora, {
    catalog: [withLora],
    config: defaultConfig("/tmp/localbase-model-metadata-image"),
    installations: new Map([[withLora.modelId, false]]),
    runtimes: runtimeSnapshots(),
  });
  expect(metadata.catalog.capabilities).toEqual({
    kind: "image",
    lora: {
      defaultStrength: 0.8,
      minimum: 0,
      maximum: 2,
      trigger: "pixel art",
    },
  });
});
