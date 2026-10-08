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
};

function byId(data: ReturnType<typeof listServedModels>, id: string) {
  return data.find((model) => model.id === id);
}

describe("listServedModels", () => {
  test("lists every kind once with modalities in stable order", () => {
    const data = listServedModels(config);
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

  test("skips unconfigured modalities and empty ids", () => {
    const data = listServedModels({
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

  test("omits models the caller lacks inference permission for", () => {
    const data = listServedModels(config, [
      "models:read",
      "inference:chat",
      "inference:transcription",
    ]);
    expect(data.map((model) => model.id)).toEqual([
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
      "whisper-tiny-q8_0",
    ]);
  });

  test("applies serve modality disables and reduced per-slot context", () => {
    const data = listServedModels(config, undefined, {
      enabled: { image: false },
      ctxSize: 4096,
      parallel: 2,
      memoryGb: 16,
    });
    expect(data.some(({ id }) => id === "flux1-schnell-q4_0")).toBe(false);
    expect(
      byId(data, "qwen2.5-coder-1.5b-instruct-q4_k_m")?.context_length,
    ).toBeLessThanOrEqual(2048);
  });
});
