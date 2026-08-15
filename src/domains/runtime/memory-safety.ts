import { z } from "zod";

export const gibibyte = 1024 ** 3;

export type MemoryReserve = {
  percent: number;
  minimumGb: number;
};

export type MemorySafetyConfig = {
  systemReserve: MemoryReserve;
  acceleratorReserve: MemoryReserve;
};

const memoryReserveSchema = z
  .object({
    percent: z.number().finite().min(0).max(100),
    minimumGb: z.number().finite().nonnegative(),
  })
  .strict();

export const memorySafetyConfigSchema = z
  .object({
    systemReserve: memoryReserveSchema,
    acceleratorReserve: memoryReserveSchema,
  })
  .strict();

export type MemoryPool = Readonly<{
  id: string;
  capacityBytes: number;
}>;

export type MemoryTopology =
  | Readonly<{
      kind: "unified";
      system: MemoryPool;
    }>
  | Readonly<{
      kind: "discrete";
      system: MemoryPool;
      accelerators: readonly MemoryPool[];
    }>;

export type MemoryPressure = "normal" | "constrained" | "critical" | "unknown";

export type MemoryPressureObservation = Readonly<{
  poolId: string;
  pressure: MemoryPressure;
}>;

export type MemoryPoolSnapshot =
  | Readonly<{
      poolId: string;
      availability: "available";
      availableBytes: number;
      pressure: MemoryPressure;
    }>
  | Readonly<{
      poolId: string;
      availability: "unavailable";
      pressure: "unknown";
    }>;

export type HostMemorySnapshot = Readonly<{
  capturedAtMs: number;
  pools: readonly MemoryPoolSnapshot[];
}>;

export type RuntimeMemoryDemand = Readonly<{
  unifiedBytes: number;
  hostBytes: number;
  acceleratorBytes: number;
  confidence: "authoritative" | "estimated";
}>;

export type ProjectedMemoryDemand = Readonly<{
  poolId: string;
  bytes: number;
}>;

export type MemoryAdmissionDecision =
  | Readonly<{
      kind: "admitted";
      projectedDemand: readonly ProjectedMemoryDemand[];
      headroomBytes: readonly ProjectedMemoryDemand[];
    }>
  | Readonly<{
      kind: "rejected";
      reason:
        | "system-memory"
        | "accelerator-memory"
        | "memory-pressure"
        | "measurement-unavailable";
      poolId: string;
    }>;

export type MemorySafetyState = "healthy" | "constrained" | "critical";

export type MemorySafetyHysteresis = Readonly<{
  state: MemorySafetyState;
  consecutiveNormalSnapshots: number;
}>;

export type MemorySafetyTransition = Readonly<{
  previous: MemorySafetyHysteresis;
  current: MemorySafetyHysteresis;
  action: "allow" | "constrain" | "emergency-stop";
}>;

export const memorySafetyRecoverySamples = 3;
export const memorySafetyConstrainedReserveMultiplier = 2;

export function defaultMemorySafetyConfig(): MemorySafetyConfig {
  return {
    systemReserve: {
      percent: 15,
      minimumGb: 8,
    },
    acceleratorReserve: { percent: 10, minimumGb: 2 },
  };
}

export function effectiveMemoryReserveBytes(
  reserve: MemoryReserve,
  capacityBytes: number,
): number {
  const minimumBytes = Math.ceil(reserve.minimumGb * gibibyte);
  return Math.max(
    minimumBytes,
    Math.floor((capacityBytes * reserve.percent) / 100),
  );
}

function acceleratorPool(
  topology: Extract<MemoryTopology, { kind: "discrete" }>,
  poolId: string | undefined,
): MemoryPool {
  const pool = topology.accelerators.find(({ id }) => id === poolId);
  if (!pool) throw new Error("A selected accelerator pool is required.");
  return pool;
}

export function projectRuntimeMemoryDemand(input: {
  topology: MemoryTopology;
  demand: RuntimeMemoryDemand;
  acceleratorPoolId?: string;
}): readonly ProjectedMemoryDemand[] {
  const { topology, demand } = input;
  if (topology.kind === "unified") {
    return [{ poolId: topology.system.id, bytes: demand.unifiedBytes }];
  }

  const projected: ProjectedMemoryDemand[] = [
    { poolId: topology.system.id, bytes: demand.hostBytes },
  ];
  if (demand.acceleratorBytes > 0) {
    const pool = acceleratorPool(topology, input.acceleratorPoolId);
    projected.push({ poolId: pool.id, bytes: demand.acceleratorBytes });
  }
  return projected;
}

function poolForId(topology: MemoryTopology, poolId: string): MemoryPool {
  if (topology.system.id === poolId) return topology.system;
  if (topology.kind === "discrete") {
    const accelerator = topology.accelerators.find(({ id }) => id === poolId);
    if (accelerator) return accelerator;
  }
  throw new Error(`Unknown memory pool: ${poolId}.`);
}

function reserveForPool(
  topology: MemoryTopology,
  config: MemorySafetyConfig,
  pool: MemoryPool,
): number {
  return effectiveMemoryReserveBytes(
    pool.id === topology.system.id
      ? config.systemReserve
      : config.acceleratorReserve,
    pool.capacityBytes,
  );
}

function pressureRank(pressure: MemoryPressure): number {
  if (pressure === "critical") return 2;
  if (pressure === "constrained") return 1;
  return pressure === "normal" ? 0 : -1;
}

function pressureForPool(
  topology: MemoryTopology,
  config: MemorySafetyConfig,
  pool: MemoryPool,
  observed: Extract<MemoryPoolSnapshot, { availability: "available" }>,
): Exclude<MemoryPressure, "unknown"> {
  if (observed.pressure === "critical" || observed.pressure === "constrained") {
    return observed.pressure;
  }

  const reserve = reserveForPool(topology, config, pool);
  if (observed.availableBytes < reserve) return "critical";
  if (
    observed.availableBytes <
    reserve * memorySafetyConstrainedReserveMultiplier
  ) {
    return "constrained";
  }
  return "normal";
}

/** Returns the most severe continuously observed memory-pressure condition. */
export function observeMemoryPressure(input: {
  topology: MemoryTopology;
  snapshot: HostMemorySnapshot;
  config: MemorySafetyConfig;
}): MemoryPressureObservation {
  const snapshots = new Map(
    input.snapshot.pools.map((pool) => [pool.poolId, pool]),
  );
  const system = snapshots.get(input.topology.system.id);
  if (
    !system ||
    system.availability === "unavailable" ||
    system.pressure === "unknown"
  ) {
    return { poolId: input.topology.system.id, pressure: "unknown" };
  }

  let result: MemoryPressureObservation = {
    poolId: input.topology.system.id,
    pressure: pressureForPool(
      input.topology,
      input.config,
      input.topology.system,
      system,
    ),
  };
  if (input.topology.kind !== "discrete") return result;

  for (const pool of input.topology.accelerators) {
    const observed = snapshots.get(pool.id);
    if (
      !observed ||
      observed.availability === "unavailable" ||
      observed.pressure === "unknown"
    ) {
      continue;
    }
    const pressure = pressureForPool(
      input.topology,
      input.config,
      pool,
      observed,
    );
    if (pressureRank(pressure) > pressureRank(result.pressure)) {
      result = { poolId: pool.id, pressure };
    }
  }
  return result;
}

export function evaluateMemoryAdmission(input: {
  topology: MemoryTopology;
  snapshot: HostMemorySnapshot;
  config: MemorySafetyConfig;
  demand: RuntimeMemoryDemand;
  acceleratorPoolId?: string;
}): MemoryAdmissionDecision {
  const projectedDemand = projectRuntimeMemoryDemand(input);
  const snapshots = new Map(
    input.snapshot.pools.map((pool) => [pool.poolId, pool]),
  );

  for (const demand of projectedDemand) {
    const pool = poolForId(input.topology, demand.poolId);
    const observed = snapshots.get(pool.id);
    if (
      !observed ||
      observed.availability === "unavailable" ||
      observed.pressure === "unknown"
    ) {
      return {
        kind: "rejected",
        reason: "measurement-unavailable",
        poolId: pool.id,
      };
    }
    if (observed.pressure !== "normal") {
      return {
        kind: "rejected",
        reason: "memory-pressure",
        poolId: pool.id,
      };
    }
    if (
      observed.availableBytes - demand.bytes <
      reserveForPool(input.topology, input.config, pool)
    ) {
      return {
        kind: "rejected",
        reason:
          pool.id === input.topology.system.id
            ? "system-memory"
            : "accelerator-memory",
        poolId: pool.id,
      };
    }
  }

  return {
    kind: "admitted",
    projectedDemand,
    headroomBytes: projectedDemand.map((demand) => {
      const pool = poolForId(input.topology, demand.poolId);
      const observed = snapshots.get(pool.id) as Extract<
        MemoryPoolSnapshot,
        { availability: "available" }
      >;
      return {
        poolId: pool.id,
        bytes:
          observed.availableBytes -
          demand.bytes -
          reserveForPool(input.topology, input.config, pool),
      };
    }),
  };
}

function actionForState(
  state: MemorySafetyState,
): MemorySafetyTransition["action"] {
  if (state === "healthy") return "allow";
  return state === "constrained" ? "constrain" : "emergency-stop";
}

export function transitionMemorySafetyState(
  previous: MemorySafetyHysteresis,
  observedPressure: MemoryPressure,
): MemorySafetyTransition {
  if (observedPressure === "unknown") {
    return {
      previous,
      current: { state: previous.state, consecutiveNormalSnapshots: 0 },
      action: actionForState(previous.state),
    };
  }
  if (observedPressure === "critical") {
    return {
      previous,
      current: { state: "critical", consecutiveNormalSnapshots: 0 },
      action: "emergency-stop",
    };
  }
  if (observedPressure === "constrained") {
    const state = previous.state === "critical" ? "critical" : "constrained";
    return {
      previous,
      current: { state, consecutiveNormalSnapshots: 0 },
      action: actionForState(state),
    };
  }
  if (previous.state === "healthy") {
    return {
      previous,
      current: { state: "healthy", consecutiveNormalSnapshots: 0 },
      action: "allow",
    };
  }

  const normalSnapshots = previous.consecutiveNormalSnapshots + 1;
  if (normalSnapshots < memorySafetyRecoverySamples) {
    return {
      previous,
      current: {
        state: previous.state,
        consecutiveNormalSnapshots: normalSnapshots,
      },
      action: actionForState(previous.state),
    };
  }
  return {
    previous,
    current: { state: "healthy", consecutiveNormalSnapshots: 0 },
    action: "allow",
  };
}
