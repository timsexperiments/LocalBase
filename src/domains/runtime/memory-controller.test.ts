import { describe, expect, test } from "bun:test";
import {
  MemorySafetyController,
  RuntimeMemoryAdmissionError,
} from "./memory-controller";
import {
  defaultMemorySafetyConfig,
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

function sequencedProvider(availableBytes: readonly number[]) {
  let snapshotCount = 0;
  return {
    provider: {
      topology,
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

  test("blocks starts while constrained until three normal samples recover", async () => {
    const source = sequencedProvider([
      14 * gibibyte,
      32 * gibibyte,
      32 * gibibyte,
      32 * gibibyte,
    ]);
    const controller = new MemorySafetyController(
      source.provider,
      defaultMemorySafetyConfig(),
    );

    expect(await controller.poll()).toMatchObject({
      current: { state: "constrained", consecutiveNormalSnapshots: 0 },
      action: "constrain",
    });
    for (let index = 1; index <= 2; index += 1) {
      await expect(
        controller.reserve({ runtimeId: `llm:model:${index}`, demand }),
      ).rejects.toMatchObject({
        decision: {
          kind: "rejected",
          reason: "memory-pressure",
          poolId: "system",
        },
      });
    }

    const reservation = await controller.reserve({
      runtimeId: "llm:model:3",
      demand,
    });
    reservation.release();
    expect(source.snapshotCount()).toBe(4);
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
});
