import { describe, expect, test } from "bun:test";
import { listServedModels } from "./served-models";

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
  parallel: 1 as const,
  root: ".",
  llmModelsDir: ".",
};

function byId(data: Awaited<ReturnType<typeof listServedModels>>, id: string) {
  return data.find((model) => model.id === id);
}

describe("listServedModels", () => {
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

  test("uses the current config context after a hot reload", async () => {
    const reloaded = { ...config, ctxSize: 4096, parallel: 2 as const };
    const data = await listServedModels(reloaded, undefined, { memoryGb: 16 });
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.context_length,
    ).toBe(2048);
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
    ).toBe(10922);
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
