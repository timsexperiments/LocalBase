import { afterAll, describe, expect, test } from "bun:test";

import { byId, type ModelSpec } from "../../catalog";
import type { LlmKvGeometry } from "./gguf-metadata";
import {
  ACCELERATOR_OVERHEAD_BYTES,
  catalogModelDemand,
  estimateCatalogModelMemory,
  estimateTtsMemory,
  measuredDemand,
  measuredLlmDemand,
  measuredModelDemand,
  LLAMA_UBATCH_TOKENS,
  RUNTIME_HOST_OVERHEAD_BYTES,
  estimateImageMemory,
  estimateLlmMemory,
  estimateVideoMemory,
  fallbackLlmShape,
  genericArtifactDemand,
  minimumMemoryGb,
  preferMeasured,
  resolveLlmShape,
  totalArtifactBytes,
} from "./memory-estimate";
import { gibibyte } from "./memory-safety";
import {
  DECISION_MEASUREMENT_DATE,
  IMAGE_VIDEO_MEASUREMENT_DATE,
  MEASUREMENT_DATE,
  MEMORY_MEASUREMENTS,
  findLlmMeasurement,
} from "./memory-measurements";
import { SPEECH_CONTEXT_TOKENS } from "./speech-supervisor";
import {
  keepFormerCatalogEntries,
  restoreBuiltInCatalog,
} from "../../test/former-catalog";

// Some fixtures are former built-in entries an upgraded install kept.
keepFormerCatalogEntries();
afterAll(restoreBuiltInCatalog);

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

describe("minimumMemoryGb", () => {
  test("adds max(512 MiB, 10%) and rounds up to whole GiB", () => {
    // 512 MiB + 512 MiB margin fits exactly in 1 GiB.
    expect(minimumMemoryGb(512 * mebibyte)).toBe(1);
    expect(minimumMemoryGb(512 * mebibyte + 1)).toBe(2);
    // 7 GiB + 0.7 GiB = 7.7 GiB -> 8; 7.5 GiB + 0.75 GiB = 8.25 GiB -> 9.
    expect(minimumMemoryGb(7 * gibibyte)).toBe(8);
    expect(minimumMemoryGb(7.5 * gibibyte)).toBe(9);
    // 10% dominates above 5 GiB: 40 GiB -> 44 GiB.
    expect(minimumMemoryGb(40 * gibibyte)).toBe(44);
    expect(minimumMemoryGb(120 * gibibyte)).toBe(132);
  });

  test("never reports less than 1 GiB", () => {
    expect(minimumMemoryGb(0)).toBe(1);
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

  test("GGUF geometry wins over the size fallback", () => {
    const resolved = resolveLlmShape({
      size: "8B",
      weightsBytes,
      geometry,
    });
    expect(resolved.source).toBe("gguf");
    expect(resolved.shape.geometry).toBe(geometry);
  });

  test("without geometry the shape comes from the parameter count", () => {
    const resolved = resolveLlmShape({
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

  test("diffusion flash attention drops the T^2 x heads scores only", () => {
    const sd15 = catalogModel("dreamshaper-v8");
    const fa = estimateImageMemory({
      ...sd15,
      imageLaunchOptions: { diffusionFlashAttention: true },
    });
    const noFa = estimateImageMemory({
      ...sd15,
      imageLaunchOptions: { diffusionFlashAttention: false },
    });
    expect(noFa.terms.activation! - fa.terms.activation!).toBe(
      4096 ** 2 * 8 * 4,
    );
    // The untiled VAE decode, not attention, sets the image peak.
    expect(fa.terms.vaeDecode).toBe(noFa.terms.vaeDecode);
    expect(fa.demand).toEqual(noFa.demand);
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
    expect(tae.terms.vaeDecode! / vae.terms.vaeDecode!).toBeCloseTo(
      (64 * 15) / (96 * 23),
    );
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

describe("measured demand", () => {
  test("every record names a catalog model, the date, hardware and method", () => {
    for (const record of MEMORY_MEASUREMENTS) {
      const model = catalogModel(record.modelId);
      expect(model.kind).toBe(record.kind);
      expect([
        MEASUREMENT_DATE,
        IMAGE_VIDEO_MEASUREMENT_DATE,
        DECISION_MEASUREMENT_DATE,
      ]).toContain(record.date);
      expect(record.hardware).toContain("64 GiB");
      expect(record.method.length).toBeGreaterThan(0);
      // Only CPU-only runtimes (Kokoro) record no accelerator buffers.
      if (model.ttsRuntime?.kind !== "kokoro-onnx")
        expect(record.backendAcceleratorBytes).toBeGreaterThan(0);
      expect(record.peakRssBytes).toBeGreaterThan(0);
    }
  });

  test("measuredDemand adds the accelerator allowance and floors host at the weights", () => {
    const record = findLlmMeasurement("gpt-oss-20b-q4_k_m", {
      ctxTokens: 8192,
      slots: 1,
      cacheTypeK: "q8_0",
      cacheTypeV: "q8_0",
    })!;
    const artifactBytes = totalArtifactBytes(
      catalogModel("gpt-oss-20b-q4_k_m"),
    );
    const demand = measuredDemand(record, {
      artifactBytes,
      promptCacheBytes: gibibyte,
    });
    expect(demand.acceleratorBytes).toBe(
      record.backendAcceleratorBytes + ACCELERATOR_OVERHEAD_BYTES,
    );
    // The mmapped weights exceed the sampled RSS (untouched pages).
    expect(artifactBytes).toBeGreaterThan(record.peakRssBytes);
    expect(demand.hostBytes).toBe(artifactBytes + gibibyte);
    expect(demand.unifiedBytes).toBeGreaterThanOrEqual(
      demand.acceleratorBytes + gibibyte,
    );
  });

  test("measured catalog models report confidence measured", () => {
    for (const modelId of [
      "qwen3.5-9b-q4_k_m",
      "clef-flash-q4_k_m",
      "whisper-large-v3-turbo",
      "qwen3-tts-1.7b-base-q4_k_m",
      "kokoro-82m-v1.0-q8",
      "flux2-klein-4b-q4_0",
      "fastwan2.2-ti2v-5b-q6_k",
      "flux2-klein-4b-q8_0",
      "wan2.1-t2v-1.3b-q8_0",
      "flux1-schnell-q4_0",
    ]) {
      expect(catalogModelDemand(catalogModel(modelId)).confidence).toBe(
        "measured",
      );
    }
    const unmeasured = catalogModel("flux2-klein-9b-q4_0");
    expect(catalogModelDemand(unmeasured)).toEqual(
      estimateCatalogModelMemory(unmeasured).demand,
    );
  });

  test("a different LLM launch shape falls back to the estimate", () => {
    const launch = {
      modelId: "qwen3.5-9b-q4_k_m",
      artifactBytes: totalArtifactBytes(catalogModel("qwen3.5-9b-q4_k_m")),
      ctxTokens: 8192,
      slots: 1,
      cacheTypeK: "q8_0",
      cacheTypeV: "q8_0",
      promptCacheBytes: 0,
    };
    expect(measuredLlmDemand(launch)).toBeDefined();
    expect(measuredLlmDemand({ ...launch, ctxTokens: 4096 })).toBeUndefined();
    expect(measuredLlmDemand({ ...launch, slots: 2 })).toBeUndefined();
    expect(measuredLlmDemand({ ...launch, cacheTypeK: "f16" })).toBeUndefined();
    expect(
      measuredLlmDemand({ ...launch, modelId: undefined }),
    ).toBeUndefined();
  });

  test("image measurements only apply with the measured --diffusion-fa setting", () => {
    const sd15 = catalogModel("dreamshaper-v8");
    const launched = sd15.imageLaunchOptions!.diffusionFlashAttention;
    expect(measuredModelDemand(sd15)).toBeDefined();
    expect(
      measuredModelDemand({
        ...sd15,
        imageLaunchOptions: { diffusionFlashAttention: !launched },
      }),
    ).toBeUndefined();
    for (const record of MEMORY_MEASUREMENTS) {
      if (record.kind !== "image") continue;
      expect(record.shape.diffusionFlashAttention).toBe(
        catalogModel(record.modelId).imageLaunchOptions!
          .diffusionFlashAttention,
      );
    }
  });

  test("video measurements only apply at the measured bounds and options", () => {
    const fastwan = catalogModel("fastwan2.2-ti2v-5b-q6_k");
    expect(measuredModelDemand(fastwan)).toBeDefined();
    expect(
      measuredModelDemand(
        withLaunchOptions(fastwan, { diffusionFlashAttention: false }),
      ),
    ).toBeUndefined();
    // Offload keeps one stage resident; the accelerator is rounded like the
    // video estimate and the host holds the offloaded weights.
    const demand = measuredModelDemand(fastwan)!;
    expect(demand.acceleratorBytes % (256 * mebibyte)).toBe(0);
    expect(demand.hostBytes).toBeGreaterThan(demand.acceleratorBytes);
  });

  test("TTS estimates the talker at its launch context", () => {
    const model = catalogModel("qwen3-tts-1.7b-base-q4_k_m");
    const estimate = estimateTtsMemory(model).demand;
    const measured = MEMORY_MEASUREMENTS.find(
      (record) => record.modelId === model.modelId,
    )!;
    expect(measured.kind === "tts" && measured.shape.ctxTokens).toBe(
      SPEECH_CONTEXT_TOKENS,
    );
    expect(estimate.acceleratorBytes).toBeGreaterThanOrEqual(
      measured.backendAcceleratorBytes * 0.9,
    );
  });

  test("Kokoro is CPU-only in both the estimate and the measurement", () => {
    const model = catalogModel("kokoro-82m-v1.0-q8");
    const estimate = estimateTtsMemory(model).demand;
    const record = MEMORY_MEASUREMENTS.find(
      (candidate) => candidate.modelId === model.modelId,
    )!;
    expect(record.kind === "tts" && record.shape.ctxTokens).toBe(512);
    expect(estimate.acceleratorBytes).toBe(0);
    // The allowance covers the measured worst-case runtime RSS.
    expect(estimate.hostBytes).toBeGreaterThanOrEqual(record.peakRssBytes);

    const measured = measuredModelDemand(model)!;
    expect(measured.acceleratorBytes).toBe(0);
    expect(measured.hostBytes).toBe(record.peakRssBytes);
    expect(measured.unifiedBytes).toBe(record.peakRssBytes);
  });

  test("the image VAE estimate covers the measured decode buffer", () => {
    for (const modelId of ["dreamshaper-v8", "flux2-klein-4b-q4_0"]) {
      const model = catalogModel(modelId);
      const measured = MEMORY_MEASUREMENTS.find(
        (record) => record.modelId === modelId,
      )!;
      expect(
        estimateImageMemory(model).demand.acceleratorBytes,
      ).toBeGreaterThanOrEqual(measured.backendAcceleratorBytes * 0.9);
    }
  });
});

describe("estimateLlmMemory for decision models", () => {
  const weightsBytes = 6 * gibibyte;
  const shape = resolveLlmShape({ weightsBytes, size: "9B", geometry });
  const base = {
    weightsBytes,
    shape,
    ctxTokens: 8192,
    slots: 1,
    cacheTypeK: "q8_0",
    cacheTypeV: "q8_0",
    promptCacheBytes: 0,
  } as const;

  test("no KV cache; compute scales with the context micro-batch", () => {
    const decision = estimateLlmMemory({ ...base, decision: true });
    expect(decision.terms.kvCache).toBe(0);
    // 8192 tokens x f32 x 16 live rows x n_embd, no vocabulary logits.
    expect(decision.terms.compute).toBe(
      8192 * 4 * 16 * shape.shape.embeddingLength,
    );
    const halfContext = estimateLlmMemory({
      ...base,
      ctxTokens: 4096,
      decision: true,
    });
    expect(halfContext.terms.compute).toBe(decision.terms.compute! / 2);
  });
});
