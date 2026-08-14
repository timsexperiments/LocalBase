import { describe, expect, test } from "bun:test";
import {
  defaultMemorySafetyConfig,
  effectiveMemoryReserveBytes,
  evaluateMemoryAdmission,
  gibibyte,
  memorySafetyConfigSchema,
  projectRuntimeMemoryDemand,
  transitionMemorySafetyState,
  type HostMemorySnapshot,
  type MemorySafetyHysteresis,
  type MemoryTopology,
  type RuntimeMemoryDemand,
} from "./memory-safety";

const unifiedTopology: MemoryTopology = {
  kind: "unified",
  system: { id: "system", capacityBytes: 96 * gibibyte },
};

const discreteTopology: MemoryTopology = {
  kind: "discrete",
  system: { id: "system", capacityBytes: 64 * gibibyte },
  accelerators: [{ id: "gpu-0", capacityBytes: 24 * gibibyte }],
};

const demand: RuntimeMemoryDemand = {
  unifiedBytes: 20 * gibibyte,
  hostBytes: 4 * gibibyte,
  acceleratorBytes: 12 * gibibyte,
  confidence: "estimated",
};

function snapshot(
  pools: readonly HostMemorySnapshot["pools"][number][],
): HostMemorySnapshot {
  return { capturedAtMs: 1, pools: [...pools] };
}

describe("runtime memory safety", () => {
  test("projects unified demand once onto shared system memory", () => {
    expect(
      projectRuntimeMemoryDemand({ topology: unifiedTopology, demand }),
    ).toEqual([{ poolId: "system", bytes: 20 * gibibyte }]);
  });

  test("projects discrete host and accelerator demand onto their selected pools", () => {
    expect(
      projectRuntimeMemoryDemand({
        topology: discreteTopology,
        demand,
        acceleratorPoolId: "gpu-0",
      }),
    ).toEqual([
      { poolId: "system", bytes: 4 * gibibyte },
      { poolId: "gpu-0", bytes: 12 * gibibyte },
    ]);
  });

  test("uses the larger percentage or minimum reserve", () => {
    expect(
      effectiveMemoryReserveBytes({ percent: 15, minimumGb: 8 }, 32 * gibibyte),
    ).toBe(8 * gibibyte);
    expect(
      effectiveMemoryReserveBytes({ percent: 15, minimumGb: 2 }, 32 * gibibyte),
    ).toBe(Math.floor(4.8 * gibibyte));
    expect(defaultMemorySafetyConfig().systemReserve.minimumGb).toBe(8);
    expect(defaultMemorySafetyConfig().acceleratorReserve).toEqual({
      percent: 10,
      minimumGb: 2,
    });
  });

  test("admits a discrete runtime when both pools retain their reserves", () => {
    expect(
      evaluateMemoryAdmission({
        topology: discreteTopology,
        config: defaultMemorySafetyConfig(),
        demand,
        acceleratorPoolId: "gpu-0",
        snapshot: snapshot([
          {
            poolId: "system",
            availability: "available",
            availableBytes: 20 * gibibyte,
            pressure: "normal",
          },
          {
            poolId: "gpu-0",
            availability: "available",
            availableBytes: 18 * gibibyte,
            pressure: "normal",
          },
        ]),
      }),
    ).toMatchObject({ kind: "admitted" });
  });

  test.each([
    {
      poolId: "system",
      pools: [
        {
          poolId: "system",
          availability: "available",
          availableBytes: 11 * gibibyte,
          pressure: "normal",
        },
        {
          poolId: "gpu-0",
          availability: "available",
          availableBytes: 16 * gibibyte,
          pressure: "normal",
        },
      ],
      reason: "system-memory" as const,
    },
    {
      poolId: "gpu-0",
      pools: [
        {
          poolId: "system",
          availability: "available",
          availableBytes: 20 * gibibyte,
          pressure: "normal",
        },
        {
          poolId: "gpu-0",
          availability: "available",
          availableBytes: 13 * gibibyte,
          pressure: "normal",
        },
      ],
      reason: "accelerator-memory" as const,
    },
  ])(
    "rejects the specific $poolId pool that cannot retain its reserve",
    ({ poolId, pools, reason }) => {
      const config = defaultMemorySafetyConfig();
      const result = evaluateMemoryAdmission({
        topology: discreteTopology,
        config,
        demand,
        acceleratorPoolId: "gpu-0",
        snapshot: snapshot(pools),
      });
      expect(result).toEqual({
        kind: "rejected",
        reason,
        poolId,
      });
    },
  );

  test("rejects invalid persisted reserve values", () => {
    expect(() =>
      memorySafetyConfigSchema.parse({
        systemReserve: { percent: -1, minimumGb: 1 },
        acceleratorReserve: { percent: 10, minimumGb: 2 },
      }),
    ).toThrow();
  });

  test("lets observed pressure override otherwise sufficient capacity", () => {
    const result = evaluateMemoryAdmission({
      topology: unifiedTopology,
      config: defaultMemorySafetyConfig(),
      demand,
      snapshot: snapshot([
        {
          poolId: "system",
          availability: "available",
          availableBytes: 60 * gibibyte,
          pressure: "constrained",
        },
      ]),
    });
    expect(result).toEqual({
      kind: "rejected",
      reason: "memory-pressure",
      poolId: "system",
    });
  });

  test.each([
    {
      topology: unifiedTopology,
      demand,
      pools: [],
      poolId: "system",
    },
    {
      topology: discreteTopology,
      demand,
      pools: [
        {
          poolId: "system",
          availability: "available",
          availableBytes: 20 * gibibyte,
          pressure: "normal",
        },
      ],
      acceleratorPoolId: "gpu-0",
      poolId: "gpu-0",
    },
  ])(
    "fails closed when the required $poolId measurement is unavailable",
    ({ topology, demand, pools, acceleratorPoolId, poolId }) => {
      expect(
        evaluateMemoryAdmission({
          topology,
          config: defaultMemorySafetyConfig(),
          demand,
          snapshot: snapshot(pools),
          ...(acceleratorPoolId ? { acceleratorPoolId } : {}),
        }),
      ).toEqual({
        kind: "rejected",
        reason: "measurement-unavailable",
        poolId,
      });
    },
  );

  test("fails closed when a measurement reports unknown pressure", () => {
    expect(
      evaluateMemoryAdmission({
        topology: unifiedTopology,
        config: defaultMemorySafetyConfig(),
        demand,
        snapshot: snapshot([
          {
            poolId: "system",
            availability: "available",
            availableBytes: 60 * gibibyte,
            pressure: "unknown",
          },
        ]),
      }),
    ).toEqual({
      kind: "rejected",
      reason: "measurement-unavailable",
      poolId: "system",
    });
  });

  test("escalates immediately and recovers after three normal snapshots", () => {
    let state: MemorySafetyHysteresis = {
      state: "healthy",
      consecutiveNormalSnapshots: 0,
    };
    state = transitionMemorySafetyState(state, "critical").current;
    expect(state).toEqual({ state: "critical", consecutiveNormalSnapshots: 0 });

    state = transitionMemorySafetyState(state, "constrained").current;
    expect(state).toEqual({ state: "critical", consecutiveNormalSnapshots: 0 });

    state = transitionMemorySafetyState(state, "normal").current;
    expect(state).toEqual({ state: "critical", consecutiveNormalSnapshots: 1 });
    state = transitionMemorySafetyState(state, "normal").current;
    expect(state).toEqual({ state: "critical", consecutiveNormalSnapshots: 2 });
    state = transitionMemorySafetyState(state, "normal").current;
    expect(state).toEqual({ state: "healthy", consecutiveNormalSnapshots: 0 });

    const constrained = transitionMemorySafetyState(
      state,
      "constrained",
    ).current;
    expect(transitionMemorySafetyState(constrained, "unknown")).toMatchObject({
      current: { state: "constrained", consecutiveNormalSnapshots: 0 },
      action: "constrain",
    });
    expect(
      transitionMemorySafetyState(
        { state: "healthy", consecutiveNormalSnapshots: 0 },
        "unknown",
      ),
    ).toMatchObject({ action: "allow" });
  });
});
