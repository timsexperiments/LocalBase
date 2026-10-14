import { describe, expect, test } from "bun:test";

import { byId, type ModelSpec } from "../../catalog";
import type { LlmKvGeometry } from "./gguf-metadata";
import {
  ACCELERATOR_OVERHEAD_BYTES,
  LLAMA_UBATCH_TOKENS,
  MIN_VRAM_TIERS_GB,
  RUNTIME_HOST_OVERHEAD_BYTES,
  estimateImageMemory,
  estimateLlmMemory,
  estimateVideoMemory,
  fallbackLlmShape,
  genericArtifactDemand,
  minVramTierGb,
  preferMeasured,
  resolveLlmShape,
  totalArtifactBytes,
} from "./memory-estimate";
import { gibibyte } from "./memory-safety";

const mebibyte = 1024 * 1024;

function catalogModel(modelId: string): ModelSpec {
  const model = byId(modelId);
  if (!model) throw new Error(`missing catalog model ${modelId}`);
  return model;
}

function withLaunchOptions(
  model: ModelSpec,
  launchOptions: Record<string, unknown>,
): ModelSpec {
  const runtime = model.videoRuntime;
  if (!runtime) throw new Error(`${model.modelId} has no video runtime`);
  return {
    ...model,
    videoRuntime: {
      ...runtime,
      qualification: {
        ...runtime.qualification,
        launchOptions: {
          ...runtime.qualification.launchOptions,
          ...launchOptions,
        },
      },
    },
  } as ModelSpec;
}

const geometry: LlmKvGeometry = {
  architecture: "llama",
  blockCount: 32,
  fullKvHeads: 32 * 8,
  swaKvHeads: 0,
  slidingWindow: null,
  keyLength: 128,
  valueLength: 128,
  swaKeyLength: 128,
  swaValueLength: 128,
  q8Compatible: true,
  recurrentBytesPerSlot: 0,
  contextLength: 131072,
};

describe("minVramTierGb", () => {
  test("adds max(512 MiB, 10%) before rounding up to a tier", () => {
    // 512 MiB + 512 MiB margin fits exactly in 1 GiB.
    expect(minVramTierGb(512 * mebibyte)).toBe(1);
    expect(minVramTierGb(512 * mebibyte + 1)).toBe(2);
    // 7 GiB + 0.7 GiB = 7.7 GiB -> 8; 7.5 GiB + 0.75 GiB = 8.25 GiB -> 12.
    expect(minVramTierGb(7 * gibibyte)).toBe(8);
    expect(minVramTierGb(7.5 * gibibyte)).toBe(12);
    // 10% dominates above 5 GiB: 40 GiB -> 44 GiB -> 48.
    expect(minVramTierGb(40 * gibibyte)).toBe(48);
    expect(minVramTierGb(44 * gibibyte)).toBe(64);
  });

  test("returns a listed tier and rounds beyond the table to 64 GiB steps", () => {
    for (const bytes of [0, gibibyte, 3 * gibibyte, 20 * gibibyte]) {
      expect(MIN_VRAM_TIERS_GB as readonly number[]).toContain(
        minVramTierGb(bytes),
      );
    }
    expect(minVramTierGb(120 * gibibyte)).toBe(192);
  });
});

describe("preferMeasured", () => {
  const estimate = genericArtifactDemand(4 * gibibyte);

  test("keeps the estimate without a measurement", () => {
    expect(preferMeasured(estimate)).toBe(estimate.demand);
    expect(preferMeasured(estimate, null)).toBe(estimate.demand);
  });

  test("a measurement replaces the estimate", () => {
    expect(
      preferMeasured(estimate, {
        unifiedBytes: 1,
        hostBytes: 2,
        acceleratorBytes: 3,
      }),
    ).toEqual({
      unifiedBytes: 1,
      hostBytes: 2,
      acceleratorBytes: 3,
      confidence: "measured",
    });
  });
});

test("genericArtifactDemand puts weights on the accelerator plus host overhead", () => {
  expect(genericArtifactDemand(gibibyte).demand).toEqual({
    unifiedBytes: gibibyte + RUNTIME_HOST_OVERHEAD_BYTES,
    hostBytes: gibibyte + RUNTIME_HOST_OVERHEAD_BYTES,
    acceleratorBytes: gibibyte,
    confidence: "estimated",
  });
});

describe("resolveLlmShape", () => {
  const weightsBytes = 5 * gibibyte;

  test("GGUF geometry wins over the table", () => {
    const resolved = resolveLlmShape({
      modelId: "llama-3.1-8b-instruct-q4_k_m",
      weightsBytes,
      geometry,
    });
    expect(resolved.source).toBe("gguf");
    expect(resolved.shape.geometry).toBe(geometry);
  });

  test("the published table beats the size fallback", () => {
    expect(
      resolveLlmShape({ modelId: "llama-3.1-8b-instruct-q4_k_m", weightsBytes })
        .source,
    ).toBe("table");
  });

  test("unknown models use the size-based fallback", () => {
    const resolved = resolveLlmShape({
      modelId: "not-in-the-table",
      size: "8B",
      weightsBytes,
    });
    expect(resolved.source).toBe("fallback");
    expect(resolved.shape).toEqual(fallbackLlmShape(8));
  });

  test("the fallback shape grows with parameter count", () => {
    const small = fallbackLlmShape(1);
    const large = fallbackLlmShape(70);
    expect(large.geometry.blockCount).toBeGreaterThan(
      small.geometry.blockCount,
    );
    expect(large.embeddingLength).toBeGreaterThan(small.embeddingLength);
    expect(small.vocabSize).toBe(152064);
  });
});

describe("estimateLlmMemory", () => {
  const weightsBytes = 4 * gibibyte;
  const shape = resolveLlmShape({ weightsBytes, size: "8B", geometry });
  const base = {
    weightsBytes,
    shape,
    ctxTokens: 8192,
    slots: 1,
    cacheTypeK: "f16",
    cacheTypeV: "f16",
    promptCacheBytes: 0,
  } as const;

  test("KV cache follows geometry and halves (roughly) at q8_0", () => {
    const f16 = estimateLlmMemory(base);
    // 32 layers x 8 heads x 128 dims x (K + V) x 2 B x 8192 tokens = 1 GiB.
    expect(f16.terms.kvCache).toBe(gibibyte);
    const q8 = estimateLlmMemory({
      ...base,
      cacheTypeK: "q8_0",
      cacheTypeV: "q8_0",
    });
    expect(q8.terms.kvCache).toBe(Math.ceil((gibibyte * 34) / 64));
  });

  test("compute buffer = ubatch x f32 x (vocab + 4 x embd)", () => {
    const estimate = estimateLlmMemory(base);
    expect(estimate.terms.compute).toBe(
      LLAMA_UBATCH_TOKENS *
        4 *
        (shape.shape.vocabSize + 4 * shape.shape.embeddingLength),
    );
    expect(estimate.demand.acceleratorBytes).toBe(
      weightsBytes +
        gibibyte +
        estimate.terms.compute! +
        ACCELERATOR_OVERHEAD_BYTES,
    );
  });

  test("disabling flash attention adds the KQ score matrix", () => {
    const fa = estimateLlmMemory(base);
    const noFa = estimateLlmMemory({ ...base, flashAttention: false });
    expect(noFa.terms.compute! - fa.terms.compute!).toBe(
      LLAMA_UBATCH_TOKENS * 8192 * shape.shape.queryHeads * 4,
    );
  });

  test("prompt cache and slot state stay on the host", () => {
    const estimate = estimateLlmMemory({ ...base, promptCacheBytes: gibibyte });
    expect(estimate.demand.hostBytes).toBe(
      weightsBytes +
        RUNTIME_HOST_OVERHEAD_BYTES +
        estimate.terms.kvCache! +
        gibibyte +
        estimate.terms.slotState!,
    );
    expect(estimate.demand.acceleratorBytes).toBe(
      estimateLlmMemory(base).demand.acceleratorBytes,
    );
  });

  test("confidence is derived for known shapes and estimated for fallback", () => {
    expect(estimateLlmMemory(base).demand.confidence).toBe("derived");
    expect(
      estimateLlmMemory({
        ...base,
        shape: resolveLlmShape({ weightsBytes, size: "8B" }),
      }).demand.confidence,
    ).toBe("estimated");
  });
});

describe("estimateImageMemory", () => {
  test("flux1 keeps CLIP/T5 off the accelerator (--clip-on-cpu)", () => {
    const model = catalogModel("flux1-schnell-q4_0");
    const estimate = estimateImageMemory(model);
    expect(estimate.terms.hostOnlyWeights).toBeGreaterThan(0);
    expect(estimate.demand.acceleratorBytes).toBeLessThan(
      estimate.demand.unifiedBytes,
    );
    expect(estimate.demand.hostBytes).toBe(
      totalArtifactBytes(model) + RUNTIME_HOST_OVERHEAD_BYTES,
    );
    expect(estimate.demand.confidence).toBe("derived");
  });

  test("qwen3-encoder models hold every weight on the accelerator", () => {
    const model = catalogModel("flux2-klein-4b-q4_0");
    const estimate = estimateImageMemory(model);
    expect(estimate.terms.hostOnlyWeights).toBe(0);
    expect(estimate.demand.acceleratorBytes).toBeGreaterThan(
      totalArtifactBytes(model),
    );
  });
});

describe("estimateVideoMemory", () => {
  const wan = catalogModel("wan2.1-t2v-1.3b-q8_0");

  test("CPU offload only needs the largest stage resident", () => {
    const offload = estimateVideoMemory(
      withLaunchOptions(wan, { cpuOffload: true }),
    );
    const resident = estimateVideoMemory(
      withLaunchOptions(wan, { cpuOffload: false }),
    );
    expect(offload.demand.acceleratorBytes).toBeLessThan(
      resident.demand.acceleratorBytes,
    );
    expect(offload.demand.unifiedBytes).toBe(resident.demand.unifiedBytes);
    expect(offload.demand.acceleratorBytes % (256 * mebibyte)).toBe(0);
  });

  test("disabling diffusion flash attention adds T^2 x heads scores", () => {
    const fa = estimateVideoMemory(
      withLaunchOptions(wan, { diffusionFlashAttention: true }),
    );
    const noFa = estimateVideoMemory(
      withLaunchOptions(wan, { diffusionFlashAttention: false }),
    );
    expect(noFa.terms.activation!).toBeGreaterThan(fa.terms.activation!);
  });

  test("the TAE decoder decodes with fewer channels than the Wan VAE", () => {
    const runtime = wan.videoRuntime!;
    const tae = estimateVideoMemory({
      ...wan,
      videoRuntime: {
        ...runtime,
        artifacts: {
          ...runtime.artifacts,
          decoder: { ...runtime.artifacts.decoder, kind: "tae" },
        },
      },
    } as ModelSpec);
    const vae = estimateVideoMemory(wan);
    expect(tae.terms.vaeDecode! / vae.terms.vaeDecode!).toBeCloseTo(64 / 96);
  });

  test("catalog-only models are estimated with a text-encoder allowance", () => {
    const estimate = estimateVideoMemory(
      catalogModel("wan2.2-t2v-a14b-q4_k_m"),
    );
    expect(estimate.demand.confidence).toBe("estimated");
    expect(estimate.terms.textEncoderWeights).toBeGreaterThan(0);
    expect(estimateVideoMemory(wan).demand.confidence).toBe("derived");
  });
});
