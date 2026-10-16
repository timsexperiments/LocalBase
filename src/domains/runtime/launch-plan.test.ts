import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  byId,
  llmProjectorArtifact,
  primaryArtifact,
  type ModelSpec,
} from "../../catalog";
import { kvCacheBytes } from "./gguf-metadata";
import { computedMinVramGb } from "../models/memory-fit";
import {
  estimateLlmMemory,
  resolveLlmShape,
  type MemoryEstimate,
} from "./memory-estimate";
import { describe, expect, test } from "bun:test";
import {
  backendBindHost,
  configuredLlmContextSize,
  DECISION_CONTEXT_TOKENS,
  describeLlmContextBudget,
  recommendedLlmContextSize,
  resolveImageLaunchPlan,
  resolveLlmLaunchPlan,
  resolveSttLaunchPlan,
  resolveVideoLaunchPlan,
} from "./launch-plan";
import { buildLlamaServerArgs } from "./launcher";
import { resolveLlmProjectorPath, runtimeEndpoint } from "./supervisor-factory";
import { SupervisorRegistry } from "./supervisor-registry";
import type { ParallelSlots } from "../config/parallel";

function catalogModel(modelId: string): ModelSpec {
  const model = byId(modelId);
  if (!model) throw new Error(`Missing ${modelId} fixture model.`);
  return model;
}

// Catalog video models with their artifacts renamed to the fixture filenames,
// so the derived demand is computed from the real pinned artifact sizes.
function withFixtureFilenames(
  model: ModelSpec,
  filenames: Readonly<Record<string, string>>,
): ModelSpec {
  return {
    ...model,
    artifacts: model.artifacts.map((artifact) => ({
      ...artifact,
      filename: filenames[artifact.filename] ?? artifact.filename,
    })),
  };
}

const wanT2v = withFixtureFilenames(catalogModel("wan2.1-t2v-1.3b-q8_0"), {
  "Wan2.1-T2V-1.3B-Q8_0.gguf": "diffusion.gguf",
  "umt5-xxl-encoder-Q8_0.gguf": "encoder.gguf",
  "wan_2.1_vae.safetensors": "vae.safetensors",
});
const wanS2v = withFixtureFilenames(catalogModel("wan2.2-s2v-14b-fp8"), {
  "wan2.2_s2v_14B_fp8_scaled.safetensors": "diffusion.safetensors",
  "umt5-xxl-encoder-Q8_0.gguf": "encoder.safetensors",
  "wan_2.1_vae.safetensors": "vae.safetensors",
  "wav2vec2_large_english_fp16.safetensors": "wav2vec2.safetensors",
});

const root = "/tmp/local-base";

describe("runtime launch plans", () => {
  test("uses measured weights and the size fallback shape for LLM demand", () => {
    const model = byId("qwen3-coder-next-q4_k_m");
    if (!model) throw new Error("Missing Qwen3-Coder-Next fixture model.");
    const artifactBytes = model.artifacts.reduce(
      (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
      0,
    );
    const plan = resolveLlmLaunchPlan({
      runtimeId: "llm:qwen3-coder-next:1",
      root,
      modelsDirectory: `${root}/models/llm`,
      modelId: model.modelId,
      modelFile: primaryArtifact(model).filename,
      host: "127.0.0.1",
      port: 8080,
      ctxSize: 8192,
      contextWindowTokens: model.contextWindowTokens,
      parallel: "auto",
      model: model,
      artifactBytes,
      hardware: { memoryGb: 64 },
    });

    expect(computedMinVramGb(model)).toBeLessThanOrEqual(64);
    expect(artifactBytes).toBeGreaterThan(48 * 10 ** 9);
    expect(plan.memoryDemand.unifiedBytes / 1024 ** 3).toBeCloseTo(50.9, 1);
    expect(plan.parallel.slots).toBe(4);
    expect(plan.memoryDemand.unifiedBytes).toBeLessThan(54.4 * 1024 ** 3);
  });

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
          artifactBytes: 4 * 1024 ** 3,
          hardware: { memoryGb: 16 },
        }),
      expected: {
        modality: "llm",
        component: "llama-server",
        modelPath: `${root}/models/llm/model.gguf`,
        healthUrl: "http://127.0.0.1:8080/health",
        // No catalog shape: the size-based fallback keeps "estimated".
        memoryDemand: {
          unifiedBytes: 8575500288,
          hostBytes: 8230567936,
          acceleratorBytes: 6351175680,
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
          model: wanT2v,
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
        // Measured at 320x320x33 with CPU offload: the largest stage (text
        // encoder) rounded up to 0.5 GiB on the accelerator, peak RSS on the
        // host, and unified as the unrounded accelerator demand (6566.05 MiB)
        // plus the 7463.53 MiB of host weight buffers.
        memoryDemand: {
          unifiedBytes: 14711080878,
          hostBytes: 12_147_949_568,
          acceleratorBytes: 6.5 * 1024 ** 3,
          confidence: "measured",
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
      model: wanS2v,
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
      memoryDemand: { confidence: "derived" },
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
      artifactBytes: 14 * 1024 ** 3,
      hardware: { memoryGb: 64 },
    };
    // Non-KV terms for an unlisted model of the given weight size: the
    // llama.cpp compute buffer and per-slot sampler state.
    const llmTerms = (weightsBytes: number): MemoryEstimate["terms"] =>
      estimateLlmMemory({
        weightsBytes,
        shape: resolveLlmShape({ weightsBytes }),
        ctxTokens: 32768,
        slots: 1,
        cacheTypeK: "f16",
        cacheTypeV: "f16",
        promptCacheBytes: 0,
      }).terms;
    const compute = llmTerms(14 * 1024 ** 3).compute!;
    const slotState = llmTerms(14 * 1024 ** 3).slotState!;

    test("uses geometry with q8_0 cache for chat models", () => {
      const plan = resolveLlmLaunchPlan({ ...input, kvGeometry: mistral });
      const kv = (5 * 1024 ** 3 * 34) / 64;
      expect(plan.kvCache).toEqual({ typeK: "q8_0", typeV: "q8_0" });
      expect(plan.memoryDemand.acceleratorBytes).toBe(
        14 * 1024 ** 3 + kv + compute + 0.5 * 1024 ** 3,
      );
      expect(plan.memoryDemand.hostBytes).toBe(
        14 * 1024 ** 3 + 0.5 * 1024 ** 3 + kv + 2048 * 1024 ** 2 + slotState,
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
        14 * 1024 ** 3 + kv + compute + 0.5 * 1024 ** 3,
      );
    });

    test("uses f16 when geometry is unknown", () => {
      expect(resolveLlmLaunchPlan(input).kvCache).toEqual({
        typeK: "f16",
        typeV: "f16",
      });
    });

    test("falls back to the size-based shape without geometry", () => {
      const plan = resolveLlmLaunchPlan(input);
      const kv = kvCacheBytes(
        resolveLlmShape({ weightsBytes: 14 * 1024 ** 3 }).shape.geometry,
        { ctxTokens: 32768, slots: 1, cacheTypeK: "f16", cacheTypeV: "f16" },
      );
      expect(plan.memoryDemand).toMatchObject({
        acceleratorBytes: 14 * 1024 ** 3 + kv + compute + 0.5 * 1024 ** 3,
        confidence: "estimated",
      });
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
        14 * 1024 ** 3 + 0.5 * 1024 ** 3 + 5 * 1024 ** 3 + slotState,
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
        7.5 * 1024 ** 3 +
          kv +
          llmTerms(7.5 * 1024 ** 3).compute! +
          0.5 * 1024 ** 3,
      );
    });

    test("allocates fewer auto slots for heavy-KV models", () => {
      const tight = {
        ...input,
        parallel: "auto" as const,
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
        model: wanT2v,
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

describe("backend bind host", () => {
  test.each([
    ["127.0.0.1", "127.0.0.1"],
    ["127.12.34.56", "127.12.34.56"],
    ["::1", "::1"],
    ["[::1]", "::1"],
    ["::1%lo0", "::1"],
    ["[::1%anything]", "::1"],
    ["localhost", "localhost"],
    ["LOCALHOST.", "LOCALHOST."],
    ["0.0.0.0", "127.0.0.1"],
    ["::", "127.0.0.1"],
    ["*", "127.0.0.1"],
    ["192.168.1.20", "127.0.0.1"],
    ["10.0.0.1", "127.0.0.1"],
    ["2001:db8::1", "127.0.0.1"],
    ["fe80::1%lo0", "127.0.0.1"],
    ["::ffff:192.168.1.20", "127.0.0.1"],
    ["example.com", "127.0.0.1"],
    ["0.0.0.0.nip.io", "127.0.0.1"],
    ["::invalid-address", "127.0.0.1"],
    ["", "127.0.0.1"],
  ])("normalizes %p to %p with consistent parseable URLs", (host, expected) => {
    const normalized = backendBindHost(host);
    const plan = resolveLlmLaunchPlan({
      root,
      modelId: "model",
      host,
      artifactBytes: 1024 ** 3,
      runtimeId: "llm:1",
      modelsDirectory: `${root}/models/llm`,
      modelFile: "model.gguf",
      port: 8080,
      ctxSize: 8192,
      parallel: "auto",
      hardware: { memoryGb: 16 },
    });
    const proxyUrl = runtimeEndpoint(host, 8080);
    const launch = buildLlamaServerArgs(plan);
    expect(launch.args).toContain("--no-kv-unified");

    expect(normalized).toBe(expected);
    expect(plan.host).toBe(expected);
    expect(new URL(plan.healthUrl).hostname).toBe(
      expected.includes(":") ? `[${expected}]` : expected.toLowerCase(),
    );
    expect(new URL(proxyUrl).hostname).toBe(
      expected.includes(":") ? `[${expected}]` : expected.toLowerCase(),
    );
    expect(
      launch.args.slice(
        launch.args.indexOf("--host"),
        launch.args.indexOf("--host") + 2,
      ),
    ).toEqual(["--host", expected]);
  });

  test("normalizes hostname consistently for launch args, health, and proxy URL", () => {
    const host = "0.0.0.0.nip.io";
    const plan = resolveLlmLaunchPlan({
      root,
      modelId: "model",
      host,
      artifactBytes: 1024 ** 3,
      runtimeId: "llm:1",
      modelsDirectory: `${root}/models/llm`,
      modelFile: "model.gguf",
      port: 8080,
      ctxSize: 8192,
      parallel: "auto",
      hardware: { memoryGb: 16 },
    });
    const launch = buildLlamaServerArgs(plan);

    expect(plan.host).toBe("127.0.0.1");
    expect(plan.healthUrl).toBe("http://127.0.0.1:8080/health");
    expect(
      launch.args.slice(
        launch.args.indexOf("--host"),
        launch.args.indexOf("--host") + 2,
      ),
    ).toEqual(["--host", "127.0.0.1"]);
    expect(runtimeEndpoint(host, 8080)).toBe("http://127.0.0.1:8080");
  });

  test.each(["0.0.0.0", "::", "192.168.1.20"].map((h) => [h]))(
    "llm, stt and image plans never bind or connect via wildcard host %p",
    (host) => {
      const base = {
        root,
        modelId: "model",
        host,
        artifactBytes: 1024 ** 3,
      };
      const expected = "127.0.0.1";
      const plans = [
        resolveLlmLaunchPlan({
          ...base,
          runtimeId: "llm:1",
          modelsDirectory: `${root}/models/llm`,
          modelFile: "model.gguf",
          port: 8080,
          ctxSize: 8192,
          parallel: "auto",
          hardware: { memoryGb: 16 },
        }),
        resolveSttLaunchPlan({
          ...base,
          runtimeId: "stt:1",
          modelsDirectory: `${root}/models/stt`,
          modelFile: "model.bin",
          port: 8081,
        }),
        resolveImageLaunchPlan({
          ...base,
          runtimeId: "image:1",
          modelsDirectory: `${root}/models/image`,
          modelFile: "model.safetensors",
          port: 8082,
        }),
      ];
      for (const plan of plans) {
        expect(plan.host).toBe(expected);
        expect(plan.healthUrl.startsWith(`http://${expected}:`)).toBe(true);
        expect(() => new URL(plan.healthUrl)).not.toThrow();
      }
    },
  );
});

describe("LLM multimodal projector", () => {
  const qwen = catalogModel("qwen2.5-coder-7b-instruct-q4_k_m");
  const vision: ModelSpec = {
    ...qwen,
    modelId: "vision-fixture",
    artifacts: [
      ...qwen.artifacts,
      {
        ...primaryArtifact(qwen),
        role: "supplementary",
        filename: "mmproj-vision-fixture-f16.gguf",
      },
    ],
  };
  const planInput = {
    root,
    modelId: "vision-fixture",
    host: "127.0.0.1",
    artifactBytes: 1024 ** 3,
    runtimeId: "llm:1",
    modelsDirectory: `${root}/models/llm`,
    modelFile: "model.gguf",
    port: 8080,
    ctxSize: 8192,
    parallel: "auto" as const,
    hardware: { memoryGb: 16 },
  };

  test("finds only supplementary mmproj artifacts on LLMs", () => {
    expect(llmProjectorArtifact(qwen)).toBeUndefined();
    expect(llmProjectorArtifact(vision)?.filename).toBe(
      "mmproj-vision-fixture-f16.gguf",
    );
  });

  test("passes --mmproj only when the plan carries a projector", () => {
    const textOnly = resolveLlmLaunchPlan(planInput);
    expect(textOnly.projectorPath).toBeNull();
    expect(buildLlamaServerArgs(textOnly).args).not.toContain("--mmproj");

    const projectorPath = `${root}/models/llm/mmproj-vision-fixture-f16.gguf`;
    const multimodal = resolveLlmLaunchPlan({ ...planInput, projectorPath });
    expect(multimodal.projectorPath).toBe(projectorPath);
    const args = buildLlamaServerArgs(multimodal).args;
    const flag = args.indexOf("--mmproj");
    expect(args.slice(flag, flag + 2)).toEqual(["--mmproj", projectorPath]);
    expect(args.filter((arg) => arg === "--mmproj")).toHaveLength(1);
  });

  test("resolves the installed projector and warns when it is missing", async () => {
    const directory = mkdtempSync(join(tmpdir(), "local-base-mmproj-"));
    try {
      const warnings: string[] = [];
      const logger = {
        info: () => {},
        warn: (_component: string, message: string) => warnings.push(message),
      };
      expect(await resolveLlmProjectorPath(qwen, directory, logger)).toBeNull();
      expect(
        await resolveLlmProjectorPath(vision, directory, logger),
      ).toBeNull();
      expect(warnings).toEqual([
        'Projector mmproj-vision-fixture-f16.gguf for "vision-fixture" is not installed; launching text-only',
      ]);

      const path = join(directory, "mmproj-vision-fixture-f16.gguf");
      writeFileSync(path, "projector");
      expect(await resolveLlmProjectorPath(vision, directory, logger)).toBe(
        path,
      );
      expect(warnings).toHaveLength(1);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });
});

describe("LLM context budget", () => {
  const model = byId("qwen2.5-coder-1.5b-instruct-q4_k_m");
  if (!model) throw new Error("Missing Qwen2.5 Coder 1.5B fixture model.");
  const memoryGb = 4;
  const recommended = recommendedLlmContextSize(model, memoryGb);

  test("explicit mode launches at the configured ceiling", () => {
    const ceiling = recommended * 2;
    expect(configuredLlmContextSize(model, ceiling, memoryGb)).toBe(
      recommended,
    );
    expect(
      configuredLlmContextSize(model, ceiling, memoryGb, undefined, "explicit"),
    ).toBe(ceiling);
    expect(
      configuredLlmContextSize(model, ceiling, memoryGb, 4096, "explicit"),
    ).toBe(4096);
  });

  test("auto mode explains the clamp below the ceiling", () => {
    const ceiling = recommended * 2;
    const budget = describeLlmContextBudget({
      model,
      configCtxSize: ceiling,
      memoryGb,
      plan: {
        ctxSize: recommended,
        parallel: { slots: 1, isAuto: false, contextPerSlot: recommended },
      },
    });
    expect(budget.mode).toBe("auto");
    expect(budget.configured_ceiling).toBe(ceiling);
    expect(budget.recommended_max).toBe(recommended);
    expect(budget.launch_context).toBe(recommended);
    expect(budget.effective_context).toBe(recommended);
    expect(budget.reasons).toHaveLength(1);
    expect(budget.reasons[0]).toContain("--ctx-mode explicit");
  });

  test("explicit mode above the recommendation keeps the ceiling and warns", () => {
    const ceiling = Math.min(
      recommended * 2,
      model.contextWindowTokens ?? Infinity,
    );
    const budget = describeLlmContextBudget({
      model,
      configCtxSize: ceiling,
      ctxMode: "explicit",
      memoryGb,
      plan: {
        ctxSize: ceiling,
        parallel: { slots: 1, isAuto: false, contextPerSlot: ceiling },
      },
    });
    expect(budget.mode).toBe("explicit");
    expect(budget.launch_context).toBe(ceiling);
    expect(budget.effective_context).toBe(ceiling);
    expect(budget.reasons).toHaveLength(1);
    expect(budget.reasons[0]).toContain("above the memory-safe recommendation");
  });

  test("process overrides and parallel slots are reported as reductions", () => {
    const budget = describeLlmContextBudget({
      model,
      configCtxSize: 16384,
      ctxSizeOverride: 8192,
      memoryGb,
      plan: {
        ctxSize: 8192,
        parallel: { slots: 2, isAuto: true, contextPerSlot: 4096 },
        trainingContextLength: 2048,
      },
    });
    expect(budget.process_override).toBe(8192);
    expect(budget.launch_context).toBe(8192);
    expect(budget.parallel_slots).toBe(2);
    expect(budget.training_context).toBe(2048);
    expect(budget.effective_context).toBe(2048);
    expect(budget.reasons.some((r) => r.includes("does not persist"))).toBe(
      true,
    );
    expect(budget.reasons.some((r) => r.includes("parallel = auto"))).toBe(
      true,
    );
    expect(budget.reasons.some((r) => r.includes("training context"))).toBe(
      true,
    );
  });

  test("reports no reasons when the ceiling is served unchanged", () => {
    const budget = describeLlmContextBudget({
      model,
      configCtxSize: 4096,
      memoryGb,
      plan: {
        ctxSize: 4096,
        parallel: { slots: 1, isAuto: false, contextPerSlot: 4096 },
      },
    });
    expect(budget.effective_context).toBe(4096);
    expect(budget.reasons).toEqual([]);
  });
});

describe("decision model launch", () => {
  const plan = (
    decision: boolean,
    ctxSize = 8192,
    parallel: ParallelSlots = 1,
  ) =>
    resolveLlmLaunchPlan({
      root,
      modelId: "clef-flash",
      host: "127.0.0.1",
      artifactBytes: 6 * 1024 ** 3,
      runtimeId: "llm:1",
      modelsDirectory: `${root}/models/llm`,
      modelFile: "clef.gguf",
      port: 8080,
      ctxSize,
      parallel,
      hardware: { memoryGb: 64 },
      decision,
    });

  test("decision servers skip the prompt cache and batch a slot at once", () => {
    const decision = plan(true);
    expect(decision.decision).toBe(true);
    expect(decision.promptCacheRamMib).toBe(0);
    const { args } = buildLlamaServerArgs(decision);
    const at = (flag: string) => args[args.indexOf(flag) + 1];
    expect(at("-b")).toBe("8192");
    expect(at("-ub")).toBe("8192");
    expect(at("--cache-ram")).toBe("0");
    expect(at("--flash-attn")).toBe("on");
    expect(args).not.toContain("--jinja");
    expect(args).not.toContain("--embeddings");
    expect(args).not.toContain("--cache-reuse");
  });

  test("decision servers cap each slot at the decision context", () => {
    const single = plan(true, 65536);
    expect(single.ctxSize).toBe(DECISION_CONTEXT_TOKENS);
    expect(single.parallel.contextPerSlot).toBe(DECISION_CONTEXT_TOKENS);
    const at = (flag: string) => {
      const { args } = buildLlamaServerArgs(single);
      return args[args.indexOf(flag) + 1];
    };
    expect(at("-c")).toBe("8192");
    expect(at("-ub")).toBe("8192");

    const four = plan(true, 65536, 4);
    expect(four.ctxSize).toBe(4 * DECISION_CONTEXT_TOKENS);
    expect(four.parallel.contextPerSlot).toBe(DECISION_CONTEXT_TOKENS);

    const auto = plan(true, 65536, "auto");
    expect(auto.parallel.contextPerSlot).toBeLessThanOrEqual(
      DECISION_CONTEXT_TOKENS,
    );
    expect(plan(true, 65536).memoryDemand).toEqual(plan(true).memoryDemand);
  });

  test("the context budget explains the decision cap", () => {
    const budget = describeLlmContextBudget({
      model: catalogModel("clef-flash-q4_k_m"),
      configCtxSize: 65536,
      ctxMode: "explicit",
      memoryGb: 64,
      plan: plan(true, 65536),
    });
    expect(budget.launch_context).toBe(DECISION_CONTEXT_TOKENS);
    expect(budget.reasons.join(" ")).toContain("Decision models");
  });

  test("chat models keep the configured context", () => {
    expect(plan(false, 65536).ctxSize).toBe(65536);
  });

  test("chat models keep the chat flags", () => {
    const chat = plan(false);
    expect(chat.decision).toBe(false);
    const { args } = buildLlamaServerArgs(chat);
    expect(args).toContain("--jinja");
    expect(args).toContain("--cache-reuse");
    expect(args).not.toContain("-ub");
  });
});
