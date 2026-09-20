import { expect, test } from "bun:test";
import { byId, CATALOG, type ModelSpec } from "../../catalog";
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

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const multipartModelId = "qwen3-coder-next-q4_k_m";
const speechModelId = "qwen3-tts-1.7b-base-q4_k_m";
const embeddingModelId = "qwen3-embedding-0.6b-q8_0";
const videoModelId = "wan2.1-t2v-1.3b-q8_0";

function catalogModel(id = modelId) {
  const model = byId(id);
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
  expect(model.ttsRuntime?.referenceVoices).toEqual([
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
  });

  expect(metadata.catalog.capabilities).toEqual({
    kind: "speech",
    outputFormats: ["wav"],
    voice: {
      selection: "catalog-reference",
      requestValues: ["default", "harbor", "willow"],
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
    features: ["speech-to-video", "avi-output"],
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
    expect(list.data[index]?.catalog.features).toEqual(model.features);
    expect(list.data[index]?.catalog.memory.unifiedMemoryEstimateGb).toBe(
      model.videoRuntime.estimatedMemoryDemand.unifiedBytes / 1024 ** 3,
    );
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
    expect(list.data[index]?.catalog.features).toEqual(model.features);
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
