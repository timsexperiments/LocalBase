import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { byId as catalogModelById } from "../../catalog";
import { resolveModelCapabilities } from "./capability-detection";
import { listServedModels } from "./served-models";
import {
  keepFormerCatalogEntries,
  restoreBuiltInCatalog,
} from "../../test/former-catalog";

// The fixtures are former built-in entries an upgraded install kept.
beforeAll(keepFormerCatalogEntries);
afterAll(restoreBuiltInCatalog);

const config = {
  activeLlmModel: "qwen2.5-coder-1.5b-instruct-q4_k_m",
  selectedLlmModels: [
    "qwen2.5-coder-1.5b-instruct-q4_k_m",
    "qwen3-embedding-0.6b-q8_0",
  ],
  activeSttModel: "whisper-tiny-q8_0",
  selectedSttModels: ["whisper-tiny-q8_0"],
  activeTtsModel: "qwen3-tts-1.7b-base-q4_k_m",
  selectedTtsModels: ["qwen3-tts-1.7b-base-q4_k_m"],
  activeImageModel: "flux1-schnell-q4_0",
  selectedImageModels: ["flux1-schnell-q4_0"],
  activeVideoModel: "wan2.1-t2v-1.3b-q8_0",
  selectedVideoModels: ["wan2.1-t2v-1.3b-q8_0"],
  ctxSize: 16384,
  ctxMode: "auto" as const,
  parallel: 1 as const,
  root: ".",
  llmModelsDir: ".",
};

function byId(data: Awaited<ReturnType<typeof listServedModels>>, id: string) {
  return data.find((model) => model.id === id);
}

describe("listServedModels", () => {
  test("reports a decision model's output modality as decisions", async () => {
    const data = await listServedModels({
      ...config,
      selectedLlmModels: [...config.selectedLlmModels, "clef-flash-q4_k_m"],
    });
    expect(byId(data, "clef-flash-q4_k_m")?.architecture).toEqual({
      input_modalities: ["text", "image"],
      output_modalities: ["decisions"],
    });
  });

  test("lists every kind once with modalities in stable order", async () => {
    const data = await listServedModels(config);
    expect(data.map((model) => model.id)).toEqual([
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
      "qwen3-embedding-0.6b-q8_0",
      "whisper-tiny-q8_0",
      "qwen3-tts-1.7b-base-q4_k_m",
      "flux1-schnell-q4_0",
      "wan2.1-t2v-1.3b-q8_0",
    ]);
    expect(byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")).toMatchObject({
      object: "model",
      owned_by: "local-base",
      architecture: { input_modalities: ["text"], output_modalities: ["text"] },
    });
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.context_length,
    ).toBeGreaterThan(0);
    expect(byId(data, "qwen3-embedding-0.6b-q8_0")?.architecture).toEqual({
      input_modalities: ["text"],
      output_modalities: ["embeddings"],
    });
    expect(byId(data, "whisper-tiny-q8_0")?.architecture).toEqual({
      input_modalities: ["audio"],
      output_modalities: ["text"],
    });
    expect(byId(data, "qwen3-tts-1.7b-base-q4_k_m")?.architecture).toEqual({
      input_modalities: ["text"],
      output_modalities: ["audio"],
    });
    expect(
      byId(data, "flux1-schnell-q4_0")?.architecture.output_modalities,
    ).toContain("image");
    expect(
      byId(data, "wan2.1-t2v-1.3b-q8_0")?.architecture.output_modalities,
    ).toContain("video");
    expect(byId(data, "whisper-tiny-q8_0")?.name).toBeTruthy();
  });

  test("adds this machine's memory figure and its source when asked", async () => {
    const plain = await listServedModels(config);
    expect(plain.every((model) => model.memory === undefined)).toBe(true);

    const llmId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const sha = "c".repeat(64);
    const data = await listServedModels(config, undefined, {
      memoryGb: 64,
      memoryInputsForModel: async (spec) =>
        spec.modelId === llmId
          ? {
              observed: [
                {
                  modelId: llmId,
                  artifactSha256: sha,
                  launchShape: "ctx=8192;slots=4;kv=q8_0/q8_0",
                  peakBytes: 1_504_706_560,
                  method: "footprint",
                  observedAt: "2026-10-15T00:00:00.000Z",
                },
              ],
            }
          : {},
    });
    expect(byId(data, llmId)?.memory).toEqual({
      min_vram_gb: 2,
      source: "observed",
      launch_shape: "ctx=8192;slots=4;kv=q8_0/q8_0",
      observed_peak_bytes: 1_504_706_560,
      fit: "fits",
      system_memory_gb: 64,
    });
    expect(byId(data, "whisper-tiny-q8_0")?.memory).toMatchObject({
      source: expect.stringMatching(/^(estimate|measured)$/),
      observed_peak_bytes: null,
      fit: "fits",
    });
  });

  test("adds effective capabilities with their source when asked", async () => {
    const plain = await listServedModels(config);
    expect(plain.every((model) => model.capabilities === undefined)).toBe(true);

    const data = await listServedModels(config, undefined, {
      capabilitiesForModel: async (spec) =>
        resolveModelCapabilities(spec, {
          runtime:
            spec.modelId === "whisper-tiny-q8_0"
              ? { capabilities: { translation: true } }
              : null,
        }).capabilities,
    });
    expect(byId(data, "whisper-tiny-q8_0")?.capabilities).toEqual([
      { id: "speech-to-text", source: "declared" },
      { id: "multilingual", source: "declared" },
      { id: "translation", source: "runtime" },
    ]);
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.capabilities,
    ).toContainEqual({ id: "code-generation", source: "declared" });
    // Additive: the OpenAI fields stay as they were.
    expect(byId(data, "whisper-tiny-q8_0")?.architecture).toEqual({
      input_modalities: ["audio"],
      output_modalities: ["text"],
    });
  });

  test("drops capabilities for a model whose resolver fails", async () => {
    const data = await listServedModels(config, undefined, {
      capabilitiesForModel: async () => {
        throw new Error("unreadable");
      },
    });
    expect(data.length).toBeGreaterThan(0);
    expect(data.every((model) => model.capabilities === undefined)).toBe(true);
  });

  test("skips unconfigured modalities and empty ids", async () => {
    const data = await listServedModels({
      ...config,
      activeSttModel: "whisper-tiny-q8_0",
      selectedSttModels: [],
      activeTtsModel: "",
      selectedTtsModels: [],
      selectedImageModels: [],
      activeVideoModel: "",
      selectedVideoModels: [],
    });
    expect(data.map((model) => model.id)).toEqual([
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
      "qwen3-embedding-0.6b-q8_0",
    ]);
  });

  test("includes active models for explicitly enabled modalities without selections", async () => {
    const data = await listServedModels(
      {
        ...config,
        selectedSttModels: [],
        selectedTtsModels: [],
        selectedImageModels: [],
        selectedVideoModels: [],
      },
      undefined,
      {
        enabled: { stt: true, tts: true, image: true, video: true },
      },
    );

    expect(data.map(({ id }) => id)).toEqual([
      config.activeLlmModel,
      "qwen3-embedding-0.6b-q8_0",
      config.activeSttModel,
      config.activeTtsModel,
      config.activeImageModel,
      config.activeVideoModel,
    ]);
  });

  test("omits models the caller lacks inference permission for", async () => {
    const data = await listServedModels(config, [
      "models:read",
      "inference:chat",
      "inference:transcription",
    ]);
    expect(data.map((model) => model.id)).toEqual([
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
      "whisper-tiny-q8_0",
    ]);
  });

  test("restricts the projection to one ID with the list's visibility rules", async () => {
    const listed = byId(await listServedModels(config), "whisper-tiny-q8_0");
    if (!listed) throw new Error("Expected the STT model to be listed.");
    expect(
      await listServedModels(config, undefined, {
        modelId: "whisper-tiny-q8_0",
      }),
    ).toEqual([listed]);
    expect(
      await listServedModels(config, undefined, { modelId: "not-served" }),
    ).toEqual([]);
    expect(
      await listServedModels(config, ["models:read", "inference:chat"], {
        modelId: "whisper-tiny-q8_0",
      }),
    ).toEqual([]);
  });

  test("applies serve modality disables and reduced per-slot context", async () => {
    const data = await listServedModels(
      { ...config, ctxSize: 4096, parallel: 2 },
      undefined,
      {
        enabled: { image: false },
        memoryGb: 16,
      },
    );
    expect(data.some(({ id }) => id === "flux1-schnell-q4_0")).toBe(false);
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.context_length,
    ).toBeLessThanOrEqual(2048);
  });

  test("reports llama.cpp padded contexts for non-divisible and divisible slots", async () => {
    const threeSlots = await listServedModels(
      { ...config, ctxSize: 8192, parallel: 3 },
      undefined,
      { memoryGb: 16 },
    );
    const twoSlots = await listServedModels(
      { ...config, ctxSize: 8192, parallel: 2 },
      undefined,
      { memoryGb: 16 },
    );

    expect(byId(threeSlots, config.activeLlmModel)?.context_length).toBe(2816);
    expect(byId(twoSlots, config.activeLlmModel)?.context_length).toBe(4096);
  });

  test("caps reported per-slot context at GGUF training context", async () => {
    const data = await listServedModels(
      { ...config, ctxSize: 8192, parallel: 3 },
      undefined,
      {
        memoryGb: 16,
        kvGeometryForModel: async () => ({
          architecture: "qwen2",
          blockCount: 1,
          fullKvHeads: 1,
          swaKvHeads: 0,
          slidingWindow: null,
          keyLength: 1,
          valueLength: 1,
          swaKeyLength: 1,
          swaValueLength: 1,
          q8Compatible: true,
          recurrentBytesPerSlot: 0,
          contextLength: 2048,
        }),
      },
    );

    expect(byId(data, config.activeLlmModel)?.context_length).toBe(2048);
  });

  test("caps recurrent models at training context without KV geometry", async () => {
    const trainingContextLengthForModel = async () => 2048;
    const threeSlots = await listServedModels(
      { ...config, ctxSize: 8192, parallel: 3 },
      undefined,
      {
        memoryGb: 16,
        kvGeometryForModel: async () => null,
        trainingContextLengthForModel,
      },
    );
    const automaticSlots = await listServedModels(
      { ...config, ctxSize: 8192, parallel: "auto" },
      undefined,
      {
        memoryGb: 16,
        kvGeometryForModel: async () => null,
        trainingContextLengthForModel,
      },
    );

    expect(byId(threeSlots, config.activeLlmModel)?.context_length).toBe(2048);
    expect(
      byId(automaticSlots, config.activeLlmModel)?.context_length,
    ).toBeLessThanOrEqual(2048);
  });

  test("uses the current config context after a hot reload", async () => {
    const reloaded = { ...config, ctxSize: 4096, parallel: 2 as const };
    const data = await listServedModels(reloaded, undefined, { memoryGb: 16 });
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.context_length,
    ).toBe(2048);
  });

  test("calculates context for models without catalog context metadata", async () => {
    const modelId = "qwen2.5-coder-7b-instruct-q4_k_m";
    expect(catalogModelById(modelId)?.contextWindowTokens).toBeNull();
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: modelId,
        selectedLlmModels: [modelId],
        ctxSize: 8192,
        parallel: 2,
      },
      undefined,
      { memoryGb: 16 },
    );
    expect(byId(data, modelId)?.context_length).toBe(4096);
  });

  test("keeps listing models when one launch plan cannot be resolved", async () => {
    const activeModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const selectedModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: activeModel,
        selectedLlmModels: [activeModel, selectedModel],
        ctxSize: 8192,
        parallel: 4,
      },
      undefined,
      { memoryGb: 8 },
    );

    expect(byId(data, activeModel)?.context_length).toBe(2048);
    expect(byId(data, selectedModel)).toBeDefined();
    expect(byId(data, selectedModel)?.context_length).toBeUndefined();
  });

  test("uses the pinned active runtime context for a model-file override", async () => {
    const activeModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const selectedModel = "qwen2.5-coder-14b-instruct-q4_k_m";
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: activeModel,
        selectedLlmModels: [activeModel, selectedModel],
        ctxSize: 32768,
        parallel: 2,
      },
      undefined,
      {
        memoryGb: 16,
        llmModelFile: `${activeModel}.gguf`,
      },
    );

    expect(byId(data, selectedModel)?.context_length).toBe(16384);
  });

  test("uses the reloaded active model launch plan after activation with a file override", async () => {
    const pinnedModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const persistedActiveModel = "qwen2.5-coder-14b-instruct-q4_k_m";
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: persistedActiveModel,
        selectedLlmModels: [pinnedModel, persistedActiveModel],
        ctxSize: 32768,
        parallel: 1,
      },
      undefined,
      {
        memoryGb: 16,
        llmModelFile: `${pinnedModel}.gguf`,
      },
    );

    expect(byId(data, persistedActiveModel)?.context_length).toBe(8192);
    expect(byId(data, pinnedModel)?.context_length).toBe(8192);
  });

  test("uses the current replacement supervisor context for a file override", async () => {
    const activeModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const selectedModel = "qwen2.5-coder-14b-instruct-q4_k_m";
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: selectedModel,
        selectedLlmModels: [activeModel, selectedModel],
        ctxSize: 32768,
        parallel: 1,
      },
      undefined,
      {
        memoryGb: 16,
        llmModelFile: `${activeModel}.gguf`,
        pinnedContextLength: 8192,
      },
    );

    expect(byId(data, selectedModel)?.context_length).toBe(8192);
    expect(byId(data, activeModel)?.context_length).toBe(8192);
  });

  test("uses the captured idle supervisor profile after activation with a file override", async () => {
    const pinnedModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const selectedModel = "qwen2.5-coder-14b-instruct-q4_k_m";
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: selectedModel,
        selectedLlmModels: [pinnedModel, selectedModel],
        ctxSize: 32768,
        parallel: 2,
      },
      undefined,
      {
        memoryGb: 16,
        llmModelFile: `${pinnedModel}.gguf`,
        llmProfile: {
          modelId: pinnedModel,
          modelFile: `${pinnedModel}.gguf`,
          configCtxSize: 32768,
          ctxMode: "auto" as const,
          parallel: 2,
        },
      },
    );

    expect(byId(data, selectedModel)?.context_length).toBe(16384);
    expect(byId(data, pinnedModel)?.context_length).toBe(16384);
  });

  test("applies a captured supervisor profile only to its model without a file override", async () => {
    const activeModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const selectedModel = "qwen2.5-coder-14b-instruct-q4_k_m";
    const data = await listServedModels(
      {
        ...config,
        activeLlmModel: activeModel,
        selectedLlmModels: [activeModel, selectedModel],
        ctxSize: 32768,
        parallel: 2,
      },
      undefined,
      {
        memoryGb: 16,
        llmProfile: {
          modelId: activeModel,
          configCtxSize: 32768,
          ctxMode: "auto" as const,
          parallel: 2,
        },
      },
    );

    expect(byId(data, activeModel)?.context_length).toBe(16384);
    expect(byId(data, selectedModel)?.context_length).toBe(4096);
  });

  test("uses GGUF KV geometry when allocating automatic slots", async () => {
    const geometry = {
      architecture: "qwen2",
      blockCount: 28,
      fullKvHeads: 2,
      swaKvHeads: 0,
      slidingWindow: null,
      keyLength: 128,
      valueLength: 128,
      swaKeyLength: 128,
      swaValueLength: 128,
      q8Compatible: true,
      recurrentBytesPerSlot: 0,
      contextLength: 32768,
    } as const;
    const data = await listServedModels(
      { ...config, ctxSize: 32768, parallel: "auto" },
      undefined,
      {
        memoryGb: 6,
        ctxSizeOverride: 32768,
        kvGeometryForModel: async () => geometry,
      },
    );
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.context_length,
    ).toBe(8192);
  });

  test("exposes the context budget behind context_length", async () => {
    const data = await listServedModels(
      { ...config, ctxSize: 4096, parallel: 2 },
      undefined,
      { memoryGb: 16 },
    );
    const llm = byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m");
    expect(llm?.context_length).toBe(2048);
    expect(llm?.context_budget).toMatchObject({
      mode: "auto",
      configured_ceiling: 4096,
      process_override: null,
      model_context_window: 32768,
      launch_context: 4096,
      parallel_slots: 2,
      effective_context: 2048,
    });
    expect(llm?.context_budget?.reasons).toHaveLength(1);
    expect(llm?.context_budget?.reasons[0]).toContain("2 parallel slots");
  });

  test("explicit context mode serves the configured ceiling", async () => {
    const auto = await listServedModels(
      { ...config, ctxSize: 32768 },
      undefined,
      { memoryGb: 4 },
    );
    const explicit = await listServedModels(
      { ...config, ctxSize: 32768, ctxMode: "explicit" },
      undefined,
      { memoryGb: 4 },
    );
    const autoLlm = byId(auto, "qwen2.5-coder-1.5b-instruct-q4_k_m");
    const explicitLlm = byId(explicit, "qwen2.5-coder-1.5b-instruct-q4_k_m");
    expect(autoLlm?.context_length).toBeLessThan(32768);
    expect(autoLlm?.context_budget?.mode).toBe("auto");
    expect(autoLlm?.context_budget?.reasons[0]).toContain(
      "--ctx-mode explicit",
    );
    expect(explicitLlm?.context_length).toBe(32768);
    expect(explicitLlm?.context_budget?.mode).toBe("explicit");
    expect(explicitLlm?.context_budget?.launch_context).toBe(32768);
  });

  test("lists a modality configured after gateway startup", async () => {
    const reloaded = {
      ...config,
      activeImageModel: "flux1-schnell-q4_0",
      selectedImageModels: ["flux1-schnell-q4_0"],
    };
    const data = await listServedModels(reloaded, undefined, {
      enabled: { image: true },
    });
    expect(data.some(({ id }) => id === "flux1-schnell-q4_0")).toBe(true);
  });
});
