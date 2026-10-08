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
      modelRequirementGb: model.minVramGb,
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
