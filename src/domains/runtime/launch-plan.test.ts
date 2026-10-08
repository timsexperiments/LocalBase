import { kvCacheBytes } from "./gguf-metadata";
import { describe, expect, test } from "bun:test";
import {
  resolveImageLaunchPlan,
  resolveLlmLaunchPlan,
  resolveSttLaunchPlan,
  resolveVideoLaunchPlan,
} from "./launch-plan";
import { SupervisorRegistry } from "./supervisor-registry";

const root = "/tmp/local-base";

describe("runtime launch plans", () => {
  test.each([
    {
      name: "llm",
      resolve: () =>
        resolveLlmLaunchPlan({
          runtimeId: "llm:model:1",
          root,
          modelsDirectory: `${root}/models/llm`,
          modelId: "model",
          modelFile: "model.gguf",
          host: "127.0.0.1",
          port: 8080,
          ctxSize: 8192,
          parallel: "auto",
          modelRequirementGb: 4,
          artifactBytes: 4 * 1024 ** 3,
          hardware: { memoryGb: 16 },
        }),
      expected: {
        modality: "llm",
        component: "llama-server",
        modelPath: `${root}/models/llm/model.gguf`,
        healthUrl: "http://127.0.0.1:8080/health",
        memoryDemand: {
          unifiedBytes: 9 * 1024 ** 3,
          hostBytes: 7 * 1024 ** 3,
          acceleratorBytes: 6.5 * 1024 ** 3,
          confidence: "estimated",
        },
      },
    },
    {
      name: "stt",
      resolve: () =>
        resolveSttLaunchPlan({
          runtimeId: "stt:model:1",
          root,
          modelsDirectory: `${root}/models/stt`,
          modelId: "model",
          modelFile: "model.bin",
          host: "127.0.0.1",
          port: 8081,
          modelRequirementGb: 1,
          artifactBytes: 1024 ** 3,
        }),
      expected: {
        modality: "stt",
        component: "whisper-server",
        modelPath: `${root}/models/stt/model.bin`,
        healthUrl: "http://127.0.0.1:8081/health",
        memoryDemand: {
          unifiedBytes: 1.5 * 1024 ** 3,
          hostBytes: 1.5 * 1024 ** 3,
          acceleratorBytes: 1024 ** 3,
          confidence: "estimated",
        },
      },
    },
    {
      name: "image",
      resolve: () =>
        resolveImageLaunchPlan({
          runtimeId: "image:model:1",
          root,
          modelsDirectory: `${root}/models/image`,
          modelId: "model",
          modelFile: "model.safetensors",
          host: "127.0.0.1",
          port: 8082,
          modelRequirementGb: 2,
          artifactBytes: 2 * 1024 ** 3,
        }),
      expected: {
        modality: "image",
        component: "sd-server",
        modelPath: `${root}/models/image/model.safetensors`,
        healthUrl: "http://127.0.0.1:8082/",
        memoryDemand: {
          unifiedBytes: 2.5 * 1024 ** 3,
          hostBytes: 2.5 * 1024 ** 3,
          acceleratorBytes: 2 * 1024 ** 3,
          confidence: "estimated",
        },
      },
    },
    {
      name: "video",
      resolve: () =>
        resolveVideoLaunchPlan({
          runtimeId: "video:model:1",
          root,
          modelsDirectory: `${root}/models/video`,
          modelId: "model",
          diffusionModelFile: "diffusion.gguf",
          textEncoderFile: "encoder.gguf",
          host: "127.0.0.1",
          port: 8091,
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
              unifiedBytes: 16 * 1024 ** 3,
              hostBytes: 8 * 1024 ** 3,
              acceleratorBytes: 5 * 1024 ** 3,
            },
            supportedTargets: [
              {
                platform: "linux",
                architecture: "x64",
                accelerator: "nvidia",
              },
            ],
          },
          target: {
            platform: "linux",
            architecture: "x64",
            accelerator: "nvidia",
          },
        }),
      expected: {
        modality: "video",
        component: "sd-server",
        mode: "t2v",
        diffusionModelPath: `${root}/models/video/diffusion.gguf`,
        textEncoderPath: `${root}/models/video/encoder.gguf`,
        decoder: { kind: "vae", path: `${root}/models/video/vae.safetensors` },
        inputBounds: { maxWidth: 320, maxHeight: 320, maxFrames: 33, fps: 16 },
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
        healthUrl: "http://127.0.0.1:8091/",
        memoryDemand: {
          unifiedBytes: 16 * 1024 ** 3,
          hostBytes: 8 * 1024 ** 3,
          acceleratorBytes: 5 * 1024 ** 3,
          confidence: "estimated",
        },
      },
    },
  ])("resolves $name launch settings without I/O", ({ resolve, expected }) => {
    expect(resolve()).toMatchObject(expected);
  });

  test("resolves the S2V audio encoder path", () => {
    const plan = resolveVideoLaunchPlan({
      runtimeId: "video:s2v:1",
      root,
      modelsDirectory: `${root}/models/video`,
      modelId: "s2v",
      diffusionModelFile: "diffusion.safetensors",
      textEncoderFile: "encoder.safetensors",
      host: "127.0.0.1",
      port: 8091,
      videoRuntime: {
        mode: "s2v",
        artifacts: {
          diffusionModel: "diffusion.safetensors",
          textEncoder: "encoder.safetensors",
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
      target: {
        platform: "linux",
        architecture: "x64",
        accelerator: "nvidia",
      },
    });

    expect(plan).toMatchObject({
      mode: "s2v",
      audioEncoderPath: `${root}/models/video/wav2vec2.safetensors`,
    });
  });

  test("detaches and freezes launch inputs", () => {
    const hardware = { memoryGb: 16 };
    const plan = resolveLlmLaunchPlan({
      runtimeId: "llm:model:1",
      root,
      modelsDirectory: `${root}/models/llm`,
      modelId: "model",
      modelFile: "model.gguf",
      host: "127.0.0.1",
      port: 8080,
      ctxSize: 8192,
      parallel: "auto",
      modelRequirementGb: 4,
      artifactBytes: 4 * 1024 ** 3,
      hardware,
    });
    hardware.memoryGb = 32;

    expect(Object.isFrozen(plan)).toBe(true);
    expect(Object.isFrozen(plan.hardware)).toBe(true);
    expect(Object.isFrozen(plan.parallel)).toBe(true);
    expect(plan.hardware.memoryGb).toBe(16);
    expect(plan.parallel).toEqual({
      slots: 4,
      isAuto: true,
      contextPerSlot: 2048,
    });
  });

  test("caps LLM context before parallel allocation and memory estimation", () => {
    const plan = resolveLlmLaunchPlan({
      runtimeId: "llm:embedding:1",
      root,
      modelsDirectory: `${root}/models/llm`,
      modelId: "qwen3-embedding-0.6b-q8_0",
      modelFile: "Qwen3-Embedding-0.6B-Q8_0.gguf",
      host: "127.0.0.1",
      port: 8080,
      ctxSize: 131_072,
      contextWindowTokens: 32_768,
      parallel: 1,
      modelRequirementGb: 1.5,
      artifactBytes: 639150592,
      hardware: { memoryGb: 64 },
    });

    expect(plan.ctxSize).toBe(32_768);
    expect(plan.parallel.contextPerSlot).toBe(32_768);
  });

  describe("model-specific KV estimates", () => {
    const mistral = {
      architecture: "llama",
      blockCount: 40,
      fullKvHeads: 320,
      swaKvHeads: 0,
      slidingWindow: null,
      keyLength: 128,
      valueLength: 128,
      swaKeyLength: 128,
      swaValueLength: 128,
      q8Compatible: true,
      recurrentBytesPerSlot: 0,
      contextLength: null,
    };
    const input = {
      runtimeId: "llm:model:1",
      root,
      modelsDirectory: `${root}/models/llm`,
      modelId: "model",
      modelFile: "model.gguf",
      host: "127.0.0.1",
      port: 8080,
      ctxSize: 32768,
      parallel: 1 as const,
      modelRequirementGb: 14,
      artifactBytes: 14 * 1024 ** 3,
      hardware: { memoryGb: 64 },
    };

    test("uses geometry with q8_0 cache for chat models", () => {
      const plan = resolveLlmLaunchPlan({ ...input, kvGeometry: mistral });
      const kv = (5 * 1024 ** 3 * 34) / 64;
      expect(plan.kvCache).toEqual({ typeK: "q8_0", typeV: "q8_0" });
      expect(plan.memoryDemand.acceleratorBytes).toBe(
        14 * 1024 ** 3 + kv + 0.5 * 1024 ** 3,
      );
      expect(plan.memoryDemand.hostBytes).toBe(
        14 * 1024 ** 3 + 0.5 * 1024 ** 3 + kv + 2048 * 1024 ** 2,
      );
    });

    test("falls back to f16 when head dims are not divisible by 32", () => {
      const odd = { ...mistral, keyLength: 80, q8Compatible: false };
      const plan = resolveLlmLaunchPlan({ ...input, kvGeometry: odd });
      expect(plan.kvCache).toEqual({ typeK: "f16", typeV: "f16" });
      const kv = kvCacheBytes(odd, {
        ctxTokens: 32768,
        slots: 1,
        cacheTypeK: "f16",
        cacheTypeV: "f16",
      });
      expect(plan.memoryDemand.acceleratorBytes).toBe(
        14 * 1024 ** 3 + kv + 0.5 * 1024 ** 3,
      );
    });

    test("uses f16 when geometry is unknown", () => {
      expect(resolveLlmLaunchPlan(input).kvCache).toEqual({
        typeK: "f16",
        typeV: "f16",
      });
    });

    test("falls back to the flat estimate without geometry", () => {
      const plan = resolveLlmLaunchPlan(input);
      expect(plan.memoryDemand.acceleratorBytes).toBe(
        14 * 1024 ** 3 + 2 * 1024 ** 3 + 0.5 * 1024 ** 3,
      );
    });

    test("keeps embedding models on f16 without prompt-cache RAM", () => {
      const plan = resolveLlmLaunchPlan({
        ...input,
        kvGeometry: mistral,
        embedding: {
          capability: "embedding-only",
          pooling: "last",
          dimensions: { minimum: 1024, maximum: 1024 },
        },
      });
      expect(plan.kvCache).toEqual({ typeK: "f16", typeV: "f16" });
      expect(plan.promptCacheRamMib).toBe(0);
      expect(plan.memoryDemand.hostBytes).toBe(
        14 * 1024 ** 3 + 0.5 * 1024 ** 3 + 5 * 1024 ** 3,
      );
    });

    test("admits Gemma 3 12B at 32K by counting sliding-window layers per slot", () => {
      const gemma = {
        architecture: "gemma3",
        blockCount: 48,
        fullKvHeads: 8 * 8,
        swaKvHeads: 40 * 8,
        slidingWindow: 1024,
        keyLength: 256,
        valueLength: 256,
        swaKeyLength: 256,
        swaValueLength: 256,
        q8Compatible: true,
        recurrentBytesPerSlot: 0,
        contextLength: 131072,
      };
      const gemmaInput = {
        ...input,
        modelRequirementGb: 7.5,
        artifactBytes: 7.5 * 1024 ** 3,
        hardware: { memoryGb: 24 },
      };
      const plan = resolveLlmLaunchPlan({ ...gemmaInput, kvGeometry: gemma });
      const kv = kvCacheBytes(gemma, {
        ctxTokens: 32768,
        slots: 1,
        cacheTypeK: "q8_0",
        cacheTypeV: "q8_0",
      });
      expect(kv).toBeLessThan(1.4 * 1024 ** 3);
      expect(plan.memoryDemand.acceleratorBytes).toBe(
        7.5 * 1024 ** 3 + kv + 0.5 * 1024 ** 3,
      );
    });

    test("allocates fewer auto slots for heavy-KV models", () => {
      const tight = {
        ...input,
        parallel: "auto" as const,
        modelRequirementGb: 14,
        hardware: { memoryGb: 20 },
      };
      expect(resolveLlmLaunchPlan(tight).parallel.slots).toBe(4);
      expect(
        resolveLlmLaunchPlan({ ...tight, kvGeometry: mistral }).parallel.slots,
      ).toBe(2);
    });
  });

  test("rejects video admission on an unsupported platform", () => {
    expect(() =>
      resolveVideoLaunchPlan({
        runtimeId: "video:model:1",
        root,
        modelsDirectory: `${root}/models/video`,
        modelId: "model",
        diffusionModelFile: "diffusion.gguf",
        textEncoderFile: "encoder.gguf",
        host: "127.0.0.1",
        port: 8091,
        target: {
          platform: "darwin",
          architecture: "arm64",
          accelerator: "apple-unified",
        },
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
            unifiedBytes: 16 * 1024 ** 3,
            hostBytes: 8 * 1024 ** 3,
            acceleratorBytes: 5 * 1024 ** 3,
          },
          supportedTargets: [
            {
              platform: "linux",
              architecture: "x64",
              accelerator: "nvidia",
            },
          ],
        },
      }),
    ).toThrow("does not support this runtime target");
  });
});

test("supervisor registry reports configured state and shuts down each supervisor", async () => {
  let shutdowns = 0;
  const service = {
    kind: "server" as const,
    runtimeId: () => "test",
    state: () => "running" as const,
    async ensureRunning() {},
    async kill() {},
    async shutdown() {
      shutdowns += 1;
    },
  };
  const registry = new SupervisorRegistry({
    llm: service,
    image: service,
  });

  expect(registry.state("llm", true)).toEqual({
    configured: true,
    state: "running",
  });
  expect(registry.state("stt", false)).toEqual({
    configured: false,
    state: "disabled",
  });
  expect(
    registry.lifecycleSnapshot({
      modality: "llm",
      configured: true,
      modelId: "model",
    }).admission,
  ).toEqual({ kind: "unknown" });
  await registry.shutdown();
  expect(shutdowns).toBe(2);
});
