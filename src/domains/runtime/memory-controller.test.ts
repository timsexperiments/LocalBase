import { describe, expect, test } from "bun:test";
import { byId, primaryArtifact } from "../../catalog";
import { resolveLlmLaunchPlan } from "./launch-plan";
import {
  MemorySafetyController,
  RuntimeMemoryAdmissionError,
} from "./memory-controller";
import {
  defaultMemorySafetyConfig,
  effectiveMemoryReserveBytes,
  gibibyte,
  GPU_WORKING_SET_POOL_ID,
  type HostMemorySnapshot,
  type MemoryTopology,
} from "./memory-safety";

const topology: MemoryTopology = {
  kind: "unified",
  system: { id: "system", capacityBytes: 32 * gibibyte },
};

const demand = {
  unifiedBytes: 14 * gibibyte,
  hostBytes: 0,
  acceleratorBytes: 0,
  confidence: "authoritative" as const,
};

const discreteTopology: MemoryTopology = {
  kind: "discrete",
  system: { id: "system", capacityBytes: 32 * gibibyte },
  accelerators: [
    { id: "gpu-a", capacityBytes: 16 * gibibyte },
    { id: "gpu-b", capacityBytes: 16 * gibibyte },
  ],
};

const discreteDemand = {
  unifiedBytes: 0,
  hostBytes: 1 * gibibyte,
  acceleratorBytes: 8 * gibibyte,
  confidence: "authoritative" as const,
};

function provider(availableBytes = 32 * gibibyte) {
  const snapshot: HostMemorySnapshot = {
    capturedAtMs: 1,
    pools: [
      {
        poolId: "system",
        availability: "available",
        availableBytes,
        pressure: "normal",
      },
    ],
  };
  return {
    topology,
    snapshot: async () => snapshot,
    async close() {},
  };
}

function sequencedProvider(
  availableBytes: readonly number[],
  memoryTopology = topology,
) {
  let snapshotCount = 0;
  return {
    provider: {
      topology: memoryTopology,
      async snapshot() {
        const available =
          availableBytes[Math.min(snapshotCount, availableBytes.length - 1)]!;
        snapshotCount += 1;
        return {
          capturedAtMs: snapshotCount,
          pools: [
            {
              poolId: "system",
              availability: "available" as const,
              availableBytes: available,
              pressure: "normal" as const,
            },
          ],
        };
      },
      async close() {},
    },
    snapshotCount: () => snapshotCount,
  };
}

function discreteProvider(
  accelerators: readonly { id: string; availableBytes: number }[] = [
    { id: "gpu-a", availableBytes: 4 * gibibyte },
    { id: "gpu-b", availableBytes: 16 * gibibyte },
  ],
) {
  return {
    topology: discreteTopology,
    snapshot: async () => ({
      capturedAtMs: 1,
      pools: [
        {
          poolId: "system",
          availability: "available" as const,
          availableBytes: 32 * gibibyte,
          pressure: "normal" as const,
        },
        ...accelerators.map((accelerator) => ({
          poolId: accelerator.id,
          availability: "available" as const,
          availableBytes: accelerator.availableBytes,
          pressure: "normal" as const,
        })),
      ],
    }),
    async close() {},
  };
}

describe("memory controller", () => {
  test("admits Coder-Next under available memory and rejects a larger artifact", async () => {
    const availableBytes = 54.4 * gibibyte;
    const testTopology: MemoryTopology = {
      kind: "unified",
      system: { id: "system", capacityBytes: 64 * gibibyte },
    };
    const memoryProvider = {
      topology: testTopology,
      snapshot: async (): Promise<HostMemorySnapshot> => ({
        capturedAtMs: 1,
        pools: [
          {
            poolId: "system",
            availability: "available",
            availableBytes,
            pressure: "normal",
          },
        ],
      }),
      async close() {},
    };
    const noReserveConfig = {
      systemReserve: { percent: 0, minimumGb: 0 },
      acceleratorReserve: { percent: 0, minimumGb: 0 },
    };
    const model = byId("qwen3-coder-next-q4_k_m");
    if (!model) throw new Error("Missing Qwen3-Coder-Next fixture model.");
    const coderNext = resolveLlmLaunchPlan({
      runtimeId: "llm:qwen3-coder-next:1",
      root: "/unused",
      modelsDirectory: "/unused/models",
      modelId: model.modelId,
      modelFile: primaryArtifact(model).filename,
      host: "127.0.0.1",
      port: 1,
      ctxSize: 8192,
      parallel: "auto",
      model: model,
      artifactBytes: model.artifacts.reduce(
        (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
        0,
      ),
      hardware: { memoryGb: 64 },
    });

    const admittingController = new MemorySafetyController(
      memoryProvider,
      noReserveConfig,
    );
    const reservation = await admittingController.reserve({
      runtimeId: coderNext.runtimeId,
      demand: coderNext.memoryDemand,
    });
    reservation.release();

    const tooLarge = resolveLlmLaunchPlan({
      runtimeId: "llm:too-large:1",
      root: "/unused",
      modelsDirectory: "/unused/models",
      modelId: "too-large",
      modelFile: "too-large.gguf",
      host: "127.0.0.1",
      port: 1,
      ctxSize: 8192,
      parallel: 1,
      artifactBytes: 60 * gibibyte,
      hardware: { memoryGb: 64 },
    });
    const rejectingController = new MemorySafetyController(
      memoryProvider,
      noReserveConfig,
    );
    await expect(
      rejectingController.reserve({
        runtimeId: tooLarge.runtimeId,
        demand: tooLarge.memoryDemand,
      }),
    ).rejects.toMatchObject({
      decision: { kind: "rejected", reason: "system-memory" },
    });
  });

  test("serializes concurrent reservations against pending demand", async () => {
    const controller = new MemorySafetyController(
      provider(),
      defaultMemorySafetyConfig(),
    );

    const results = await Promise.allSettled([
      controller.reserve({ runtimeId: "llm:one:1", demand }),
      controller.reserve({ runtimeId: "llm:two:2", demand }),
    ]);

    expect(results.filter(({ status }) => status === "fulfilled")).toHaveLength(
      1,
    );
    const rejected = results.find(
      (result): result is PromiseRejectedResult => result.status === "rejected",
    );
    expect(rejected?.reason).toBeInstanceOf(RuntimeMemoryAdmissionError);
    expect(rejected?.reason.diagnostics).toMatchObject({
      measured_available_bytes: 32 * gibibyte,
      reserve_bytes: 8 * gibibyte,
      pending_bytes: demand.unifiedBytes,
      requested_bytes: demand.unifiedBytes,
      effective_available_bytes: 18 * gibibyte,
    });
  });

  test("released cancellation demand gives no credit before measured memory recovers", async () => {
    const model = byId("gpt-oss-20b-q4_k_m");
    if (!model) throw new Error("Missing GPT-OSS fixture model.");
    const artifact = primaryArtifact(model);
    if (artifact.expectedSizeBytes === undefined) {
      throw new Error("Missing GPT-OSS fixture artifact size.");
    }
    const plan = resolveLlmLaunchPlan({
      runtimeId: "llm:cancellation:1",
      root: "/unused",
      modelsDirectory: "/unused/models",
      modelId: model.modelId,
      modelFile: artifact.filename,
      host: "127.0.0.1",
      port: 1,
      ctxSize: 8192,
      parallel: 1,
      model: model,
      artifactBytes: artifact.expectedSizeBytes,
      hardware: { memoryGb: 64 },
    });
    const memoryTopology: MemoryTopology = {
      kind: "unified",
      system: { id: "system", capacityBytes: 64 * gibibyte },
    };
    const config = defaultMemorySafetyConfig();
    const reserve = effectiveMemoryReserveBytes(
      config.systemReserve,
      memoryTopology.system.capacityBytes,
    );
    const required = reserve + plan.memoryDemand.unifiedBytes;
    // A post-abort host estimate, not a captured admission sample.
    const postCancellationAvailable = 20_953_399_296;
    const source = sequencedProvider(
      [32 * gibibyte, postCancellationAvailable, required - 1, required],
      memoryTopology,
    );
    const controller = new MemorySafetyController(source.provider, config);
    const request = { runtimeId: plan.runtimeId, demand: plan.memoryDemand };
    const first = await controller.reserve(request);
    first.materialize();
    first.release();

    for (const [index, available] of [
      postCancellationAvailable,
      required - 1,
    ].entries()) {
      await expect(controller.reserve(request)).rejects.toMatchObject({
        decision: {
          kind: "rejected",
          reason: "system-memory",
          poolId: "system",
        },
        diagnostics: {
          measured_available_bytes: available,
          reserve_bytes: reserve,
          pending_bytes: 0,
          requested_bytes: plan.memoryDemand.unifiedBytes,
          effective_available_bytes: available,
          sample_captured_at_ms: index + 2,
          measured_pressure: "normal",
          safety_state: "healthy",
          recovery_samples: 0,
        },
      });
    }
    const replacement = await controller.reserve(request);
    replacement.release();
    expect(source.snapshotCount()).toBe(4);
  });

  test("does not subtract a materialized reservation from a fresh OS sample", async () => {
    const controller = new MemorySafetyController(
      provider(),
      defaultMemorySafetyConfig(),
    );
    const first = await controller.reserve({
      runtimeId: "llm:model:1",
      demand,
    });

    first.materialize();
    first.materialize();
    const second = await controller.reserve({
      runtimeId: "llm:model:2",
      demand,
    });

    second.release();
    first.release();
  });

  test("keeps a replacement reservation when an earlier token is released", async () => {
    const controller = new MemorySafetyController(
      provider(),
      defaultMemorySafetyConfig(),
    );
    const first = await controller.reserve({
      runtimeId: "llm:model:1",
      demand,
    });
    first.release();
    const replacement = await controller.reserve({
      runtimeId: "llm:model:1",
      demand,
    });

    first.materialize();
    const staleMaterialization = await Promise.allSettled([
      controller.reserve({ runtimeId: "llm:model:2", demand }),
    ]);
    expect(staleMaterialization[0]?.status).toBe("rejected");
    first.release();
    replacement.release();
  });

  test("honors the explicit memory-check bypass", async () => {
    const unavailable = provider();
    let snapshotCount = 0;
    unavailable.snapshot = async () => ({
      capturedAtMs: ++snapshotCount,
      pools: [
        {
          poolId: "system",
          availability: "unavailable",
          pressure: "unknown",
        },
      ],
    });
    const controller = new MemorySafetyController(
      unavailable,
      defaultMemorySafetyConfig(),
      true,
    );

    expect(await controller.poll()).toMatchObject({ action: "allow" });
    const reservation = await controller.reserve({
      runtimeId: "llm:model:1",
      demand,
    });
    reservation.release();
    expect(snapshotCount).toBe(0);
  });

  test("reserve rejects fresh critical pressure without mutating hysteresis", async () => {
    const source = sequencedProvider([7 * gibibyte, 32 * gibibyte]);
    const controller = new MemorySafetyController(
      source.provider,
      defaultMemorySafetyConfig(),
    );

    await expect(
      controller.reserve({ runtimeId: "llm:model:1", demand }),
    ).rejects.toMatchObject({
      decision: {
        kind: "rejected",
        reason: "memory-pressure",
        poolId: "system",
      },
      diagnostics: { safety_state: "healthy", recovery_samples: 0 },
    });
    expect(await controller.poll()).toMatchObject({
      previous: { state: "healthy" },
      current: { state: "healthy" },
      action: "allow",
    });
  });

  test("blocks starts while constrained until three polled normal samples recover", async () => {
    const source = sequencedProvider([9 * gibibyte, 32 * gibibyte]);
    const controller = new MemorySafetyController(
      source.provider,
      defaultMemorySafetyConfig(),
    );

    expect(await controller.poll()).toMatchObject({
      current: { state: "constrained", consecutiveNormalSnapshots: 0 },
      action: "constrain",
    });
    for (let index = 1; index <= 4; index += 1) {
      await expect(
        controller.reserve({ runtimeId: `llm:model:${index}`, demand }),
      ).rejects.toMatchObject({
        decision: {
          kind: "rejected",
          reason: "memory-pressure",
          poolId: "system",
        },
        diagnostics: {
          measured_available_bytes: 32 * gibibyte,
          reserve_bytes: 8 * gibibyte,
          pending_bytes: 0,
          requested_bytes: demand.unifiedBytes,
          measured_pressure: "normal",
          safety_state: "constrained",
          recovery_samples: 0,
        },
      });
    }

    await controller.poll();
    await controller.poll();
    expect(await controller.poll()).toMatchObject({
      current: { state: "healthy" },
    });

    const reservation = await controller.reserve({
      runtimeId: "llm:model:5",
      demand,
    });
    reservation.release();
    expect(source.snapshotCount()).toBe(9);
  });

  test("enters critical immediately and rejects new starts", async () => {
    const source = sequencedProvider([7 * gibibyte]);
    const controller = new MemorySafetyController(
      source.provider,
      defaultMemorySafetyConfig(),
    );

    expect(await controller.poll()).toMatchObject({
      current: { state: "critical", consecutiveNormalSnapshots: 0 },
      action: "emergency-stop",
    });
    await expect(
      controller.reserve({ runtimeId: "llm:model:1", demand }),
    ).rejects.toMatchObject({
      decision: {
        kind: "rejected",
        reason: "memory-pressure",
        poolId: "system",
      },
    });
  });

  test("uses one snapshot for pressure and admission", async () => {
    const source = sequencedProvider([32 * gibibyte]);
    const controller = new MemorySafetyController(
      source.provider,
      defaultMemorySafetyConfig(),
    );

    const reservation = await controller.reserve({
      runtimeId: "llm:model:1",
      demand,
    });
    reservation.release();
    expect(source.snapshotCount()).toBe(1);
  });

  test("rejects ambiguous multi-accelerator placement", async () => {
    const controller = new MemorySafetyController(
      discreteProvider(),
      defaultMemorySafetyConfig(),
    );

    const result = await Promise.allSettled([
      controller.reserve({ runtimeId: "llm:model:1", demand: discreteDemand }),
    ]);
    expect(result[0]?.status).toBe("rejected");
    if (result[0]?.status === "rejected") {
      expect(result[0].reason).toBeInstanceOf(RuntimeMemoryAdmissionError);
      expect(result[0].reason.decision).toEqual({
        kind: "rejected",
        reason: "measurement-unavailable",
        poolId: "accelerator",
      });
      expect(result[0].reason.diagnostics).toMatchObject({
        measured_available_bytes: "unavailable",
        reserve_bytes: "unavailable",
        pending_bytes: "unavailable",
        effective_available_bytes: "unavailable",
        measured_pressure: "unknown",
      });
    }
  });

  test("admits against a single discrete accelerator", async () => {
    const singleAcceleratorTopology = {
      ...discreteTopology,
      accelerators: [discreteTopology.accelerators[1]!],
    };
    const controller = new MemorySafetyController(
      {
        ...discreteProvider([{ id: "gpu-b", availableBytes: 16 * gibibyte }]),
        topology: singleAcceleratorTopology,
      },
      defaultMemorySafetyConfig(),
    );

    const reservation = await controller.reserve({
      runtimeId: "llm:model:1",
      demand: discreteDemand,
    });
    reservation.materialize();
    reservation.release();
  });

  test("rejects accelerator demand when no discrete pool exists", async () => {
    const providerWithoutAccelerators = {
      ...discreteProvider([]),
      topology: {
        kind: "discrete" as const,
        system: discreteTopology.system,
        accelerators: [],
      },
    };
    const controller = new MemorySafetyController(
      providerWithoutAccelerators,
      defaultMemorySafetyConfig(),
    );

    const result = await Promise.allSettled([
      controller.reserve({ runtimeId: "llm:model:1", demand: discreteDemand }),
    ]);
    expect(result[0]?.status).toBe("rejected");
    if (result[0]?.status === "rejected") {
      expect(result[0].reason).toBeInstanceOf(RuntimeMemoryAdmissionError);
      expect(result[0].reason.decision).toEqual({
        kind: "rejected",
        reason: "measurement-unavailable",
        poolId: "accelerator",
      });
    }
  });

  test("admits host-only demand without a discrete accelerator", async () => {
    const providerWithoutAccelerators = {
      ...discreteProvider([]),
      topology: {
        kind: "discrete" as const,
        system: discreteTopology.system,
        accelerators: [],
      },
    };
    const controller = new MemorySafetyController(
      providerWithoutAccelerators,
      defaultMemorySafetyConfig(),
    );

    const reservation = await controller.reserve({
      runtimeId: "stt:model:1",
      demand: {
        unifiedBytes: 0,
        hostBytes: 1 * gibibyte,
        acceleratorBytes: 0,
        confidence: "authoritative",
      },
    });
    reservation.materialize();
    reservation.release();
  });

  describe("checkAdmission", () => {
    test("does not permanently reject an unknown capacity after a failed sample", async () => {
      let memoryTopology: MemoryTopology = {
        kind: "unified",
        system: { id: "system", capacityBytes: 0 },
      };
      let sample = 0;
      const controller = new MemorySafetyController(
        {
          get topology() {
            return memoryTopology;
          },
          async snapshot() {
            sample += 1;
            if (sample === 1) {
              return {
                capturedAtMs: sample,
                pools: [
                  {
                    poolId: "system",
                    availability: "unavailable",
                    pressure: "unknown",
                  },
                ],
              };
            }
            memoryTopology = {
              kind: "unified",
              system: { id: "system", capacityBytes: 32 * gibibyte },
            };
            return {
              capturedAtMs: sample,
              pools: [
                {
                  poolId: "system",
                  availability: "available",
                  availableBytes: 32 * gibibyte,
                  pressure: "normal",
                },
              ],
            };
          },
          async close() {},
        },
        defaultMemorySafetyConfig(),
      );

      const initial = await controller.checkAdmission({ demand });
      expect(initial).toMatchObject({
        decision: { reason: "measurement-unavailable" },
        capacity: undefined,
      });
      expect(await controller.checkAdmission({ demand })).toBeUndefined();
      expect(sample).toBe(2);
    });

    test("admits a switch only because the releasing runtime frees memory", async () => {
      const controller = new MemorySafetyController(
        // Plenty free at load time, then 10 GiB once the model is resident:
        // a second 14 GiB model fits only once the resident one is gone.
        sequencedProvider([32 * gibibyte, 10 * gibibyte]).provider,
        defaultMemorySafetyConfig(),
      );
      const resident = await controller.reserve({
        runtimeId: "llm:a:1",
        demand,
      });
      resident.materialize();

      const without = await controller.checkAdmission({ demand });
      expect(without).toBeInstanceOf(RuntimeMemoryAdmissionError);
      expect(without?.capacity).toBeUndefined();
      expect(
        await controller.checkAdmission(
          { demand },
          { releasingRuntimeIds: ["llm:a:1"] },
        ),
      ).toBeUndefined();

      // Probing creates no reservation and the resident one is untouched.
      const next = await controller
        .reserve({ runtimeId: "llm:b:2", demand })
        .catch((error: unknown) => error);
      expect(next).toBeInstanceOf(RuntimeMemoryAdmissionError);
    });

    test("rejects a switch that cannot fit even after the release", async () => {
      const controller = new MemorySafetyController(
        sequencedProvider([32 * gibibyte, 2 * gibibyte]).provider,
        defaultMemorySafetyConfig(),
      );
      const resident = await controller.reserve({
        runtimeId: "llm:a:1",
        demand: { ...demand, unifiedBytes: 4 * gibibyte },
      });
      resident.materialize();

      const rejection = await controller.checkAdmission(
        { demand },
        { releasingRuntimeIds: ["llm:a:1"] },
      );
      expect(rejection).toBeInstanceOf(RuntimeMemoryAdmissionError);
      expect(rejection?.capacity).toBeUndefined();
    });

    test("credits a releasing runtime no more than its measured footprint", async () => {
      const sampled: number[] = [];
      const controller = new MemorySafetyController(
        // Same switch that fits on the projection alone (see above), but the
        // resident process holds only 6 GiB of its 14 GiB projection.
        sequencedProvider([32 * gibibyte, 10 * gibibyte]).provider,
        defaultMemorySafetyConfig(),
        false,
        async (pid) => {
          sampled.push(pid);
          return { bytes: 6 * gibibyte, method: "footprint" };
        },
      );
      const resident = await controller.reserve({
        runtimeId: "llm:a:1",
        demand,
      });
      resident.materialize(4242);

      const rejection = await controller.checkAdmission(
        { demand },
        { releasingRuntimeIds: ["llm:a:1"] },
      );
      expect(rejection).toBeInstanceOf(RuntimeMemoryAdmissionError);
      expect(rejection?.capacity).toBeUndefined();
      expect(sampled).toEqual([4242]);
      // Without releases there is nothing to measure.
      await controller.checkAdmission({ demand });
      expect(sampled).toEqual([4242]);
    });

    test("never credits more than the projection when the footprint is larger", async () => {
      const controller = new MemorySafetyController(
        sequencedProvider([32 * gibibyte, 10 * gibibyte]).provider,
        defaultMemorySafetyConfig(),
        false,
        async () => ({ bytes: 20 * gibibyte, method: "footprint" }),
      );
      const resident = await controller.reserve({
        runtimeId: "llm:a:1",
        demand,
      });
      resident.materialize(4242);

      expect(
        await controller.checkAdmission(
          { demand },
          { releasingRuntimeIds: ["llm:a:1"] },
        ),
      ).toBeUndefined();
    });

    test("falls back to the projection when the footprint is unavailable", async () => {
      for (const sample of [
        async () => null,
        async () => {
          throw new Error("footprint failed");
        },
      ]) {
        const controller = new MemorySafetyController(
          sequencedProvider([32 * gibibyte, 10 * gibibyte]).provider,
          defaultMemorySafetyConfig(),
          false,
          sample,
        );
        const resident = await controller.reserve({
          runtimeId: "llm:a:1",
          demand,
        });
        // Also covers a resident runtime whose pid was never reported.
        resident.materialize();
        expect(
          await controller.checkAdmission(
            { demand },
            { releasingRuntimeIds: ["llm:a:1"] },
          ),
        ).toBeUndefined();
        resident.materialize(4242);
        expect(
          await controller.checkAdmission(
            { demand },
            { releasingRuntimeIds: ["llm:a:1"] },
          ),
        ).toBeUndefined();
      }
    });

    test("does not advance hysteresis or reserve memory", async () => {
      const sequenced = sequencedProvider([32 * gibibyte]);
      const controller = new MemorySafetyController(
        sequenced.provider,
        defaultMemorySafetyConfig(),
      );
      await controller.checkAdmission({ demand });
      await controller.checkAdmission({ demand });
      // Both probes see an empty pending set: two full-size starts still fit.
      const first = await controller.reserve({ runtimeId: "llm:a:1", demand });
      first.release();
      expect(await controller.poll()).toMatchObject({
        previous: { state: "healthy", consecutiveNormalSnapshots: 0 },
      });
    });

    test("classifies a model larger than usable capacity as permanent", async () => {
      const controller = new MemorySafetyController(
        provider(32 * gibibyte),
        defaultMemorySafetyConfig(),
      );
      const rejection = await controller.checkAdmission({
        demand: { ...demand, unifiedBytes: 40 * gibibyte },
      });
      expect(rejection?.capacity).toMatchObject({
        poolId: "system",
        requiredBytes: 40 * gibibyte,
        capacityBytes: 32 * gibibyte,
      });
      await expect(
        controller.reserve({
          runtimeId: "llm:big:1",
          demand: { ...demand, unifiedBytes: 40 * gibibyte },
        }),
      ).rejects.toMatchObject({ capacity: { requiredBytes: 40 * gibibyte } });
    });
  });
});

describe("unified GPU working-set cap", () => {
  test("reports a permanent capacity shortfall against the cap", async () => {
    const capped: MemoryTopology = {
      ...topology,
      gpuWorkingSetBytes: 6 * gibibyte,
    };
    const controller = new MemorySafetyController(
      { ...provider(), topology: capped },
      defaultMemorySafetyConfig(),
    );
    const error = await controller
      .reserve({
        runtimeId: "llm:capped",
        demand: { ...demand, acceleratorBytes: 8 * gibibyte },
      })
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(RuntimeMemoryAdmissionError);
    const admission = error as RuntimeMemoryAdmissionError;
    expect(admission.decision).toEqual({
      kind: "rejected",
      reason: "accelerator-memory",
      poolId: GPU_WORKING_SET_POOL_ID,
    });
    expect(admission.capacity).toEqual({
      poolId: GPU_WORKING_SET_POOL_ID,
      requiredBytes: 8 * gibibyte,
      usableBytes: 6 * gibibyte,
      capacityBytes: 6 * gibibyte,
    });
    expect(admission.diagnostics?.requested_bytes).toBe(8 * gibibyte);

    const ok = await controller.reserve({
      runtimeId: "llm:fits",
      demand: { ...demand, acceleratorBytes: 4 * gibibyte },
    });
    ok.release();
  });
});

describe("unified GPU working-set cap across resident runtimes", () => {
  const cappedTopology: MemoryTopology = {
    ...topology,
    gpuWorkingSetBytes: 10 * gibibyte,
  };
  const gpuDemand = {
    unifiedBytes: 2 * gibibyte,
    hostBytes: 0,
    acceleratorBytes: 6 * gibibyte,
    confidence: "measured" as const,
  };
  const cappedController = () =>
    new MemorySafetyController(
      { ...provider(), topology: cappedTopology },
      defaultMemorySafetyConfig(),
    );

  test("rejects a runtime that fits alone but not beside a resident runtime", async () => {
    const controller = cappedController();
    const first = await controller.reserve({
      runtimeId: "llm:a:1",
      demand: gpuDemand,
    });
    first.materialize();

    const error = await controller
      .reserve({ runtimeId: "image:b:2", demand: gpuDemand })
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(RuntimeMemoryAdmissionError);
    const admission = error as RuntimeMemoryAdmissionError;
    expect(admission.decision).toEqual({
      kind: "rejected",
      reason: "accelerator-memory",
      poolId: GPU_WORKING_SET_POOL_ID,
    });
    // Transient: stopping the resident runtime can make room.
    expect(admission.capacity).toBeUndefined();
    expect(admission.gpuWorkingSet).toEqual({
      requiredBytes: 6 * gibibyte,
      residentBytes: 6 * gibibyte,
      capBytes: 10 * gibibyte,
    });
    expect(admission.message).toContain("GPU working set");

    first.release();
    const second = await controller.reserve({
      runtimeId: "image:b:2",
      demand: gpuDemand,
    });
    second.release();
  });

  test("counts runtimes that are still starting", async () => {
    const controller = cappedController();
    const starting = await controller.reserve({
      runtimeId: "llm:a:1",
      demand: gpuDemand,
    });
    await expect(
      controller.reserve({ runtimeId: "image:b:2", demand: gpuDemand }),
    ).rejects.toMatchObject({
      gpuWorkingSet: { residentBytes: 6 * gibibyte },
    });
    starting.release();
  });

  test("admits demand that sums to exactly the cap", async () => {
    const controller = cappedController();
    const first = await controller.reserve({
      runtimeId: "llm:a:1",
      demand: { ...gpuDemand, acceleratorBytes: 4 * gibibyte },
    });
    first.materialize();
    const second = await controller.reserve({
      runtimeId: "image:b:2",
      demand: gpuDemand,
    });
    second.release();
    first.release();
  });

  test("does not count a runtime that is being replaced", async () => {
    const controller = cappedController();
    const current = await controller.reserve({
      runtimeId: "llm:a:1",
      demand: gpuDemand,
    });
    current.materialize();
    const other = await controller.reserve({
      runtimeId: "stt:c:3",
      demand: { ...gpuDemand, acceleratorBytes: 1 * gibibyte },
    });
    other.materialize();

    expect(
      await controller.checkAdmission({ demand: gpuDemand }),
    ).toMatchObject({ gpuWorkingSet: { residentBytes: 7 * gibibyte } });
    expect(
      await controller.checkAdmission(
        { demand: gpuDemand },
        { releasingRuntimeIds: ["llm:a:1"] },
      ),
    ).toBeUndefined();
    // Still rejected when the remaining runtimes alone leave too little room.
    expect(
      await controller.checkAdmission(
        { demand: { ...gpuDemand, acceleratorBytes: 9.5 * gibibyte } },
        { releasingRuntimeIds: ["llm:a:1"] },
      ),
    ).toMatchObject({ gpuWorkingSet: { residentBytes: 1 * gibibyte } });
    other.release();
    current.release();
  });

  test("releasing a runtime also releases its per-generation reservations", async () => {
    const controller = cappedController();
    const generation = await controller.reserve({
      runtimeId: "tts:voice:4:generation:1",
      demand: gpuDemand,
    });
    expect(
      await controller.checkAdmission(
        { demand: gpuDemand },
        { releasingRuntimeIds: ["tts:voice:4"] },
      ),
    ).toBeUndefined();
    // A runtime id that merely shares a prefix is not released.
    expect(
      await controller.checkAdmission(
        { demand: gpuDemand },
        { releasingRuntimeIds: ["tts:voi"] },
      ),
    ).toBeInstanceOf(RuntimeMemoryAdmissionError);
    generation.release();
  });

  test("ignores runtimes without accelerator demand and topologies without a cap", async () => {
    const controller = cappedController();
    const cpuOnly = await controller.reserve({
      runtimeId: "stt:cpu:1",
      demand: { ...gpuDemand, acceleratorBytes: 0 },
    });
    cpuOnly.materialize();
    const gpu = await controller.reserve({
      runtimeId: "llm:a:2",
      demand: { ...gpuDemand, acceleratorBytes: 10 * gibibyte },
    });
    gpu.release();
    cpuOnly.release();

    const uncapped = new MemorySafetyController(
      provider(),
      defaultMemorySafetyConfig(),
    );
    const a = await uncapped.reserve({
      runtimeId: "llm:a:1",
      demand: gpuDemand,
    });
    a.materialize();
    const b = await uncapped.reserve({
      runtimeId: "image:b:2",
      demand: gpuDemand,
    });
    b.release();
    a.release();
  });
});
