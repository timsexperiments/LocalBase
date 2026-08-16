import type { HostMemoryProvider } from "./memory/host-memory-provider";
import {
  evaluateMemoryAdmission,
  observeMemoryPressure,
  type HostMemorySnapshot,
  type MemoryAdmissionDecision,
  type MemoryPoolSnapshot,
  type MemorySafetyConfig,
  type MemorySafetyHysteresis,
  type MemorySafetyTransition,
  type ProjectedMemoryDemand,
  type RuntimeMemoryDemand,
  transitionMemorySafetyState,
} from "./memory-safety";

export type RuntimeMemoryReservationRequest = Readonly<{
  runtimeId: string;
  demand: RuntimeMemoryDemand;
}>;

export type RuntimeMemoryReservation = Readonly<{
  runtimeId: string;
  materialize: () => void;
  release: () => void;
}>;

type ReservationEntry = {
  token: symbol;
  projectedDemand: readonly ProjectedMemoryDemand[];
  state: "pending" | "resident";
};

function subtractReservations(
  snapshot: HostMemorySnapshot,
  reservations: Iterable<ReservationEntry>,
): HostMemorySnapshot {
  const reserved = new Map<string, number>();
  for (const reservation of reservations) {
    if (reservation.state !== "pending") continue;
    for (const demand of reservation.projectedDemand) {
      reserved.set(
        demand.poolId,
        (reserved.get(demand.poolId) ?? 0) + demand.bytes,
      );
    }
  }

  const pools: MemoryPoolSnapshot[] = snapshot.pools.map((pool) => {
    if (pool.availability === "unavailable") return pool;
    return {
      ...pool,
      availableBytes: Math.max(
        0,
        pool.availableBytes - (reserved.get(pool.poolId) ?? 0),
      ),
    };
  });
  return { ...snapshot, pools };
}

/** Rejects backend starts that would violate the current host-memory policy. */
export class RuntimeMemoryAdmissionError extends Error {
  constructor(
    readonly decision: Extract<MemoryAdmissionDecision, { kind: "rejected" }>,
  ) {
    super("Insufficient memory to start the requested runtime.");
    this.name = "RuntimeMemoryAdmissionError";
  }
}

/** Owns in-process reservations for managed backend process generations. */
export class MemorySafetyController {
  private readonly reservations = new Map<string, ReservationEntry>();
  private operations = Promise.resolve();
  private hysteresis: MemorySafetyHysteresis = {
    state: "healthy",
    consecutiveNormalSnapshots: 0,
  };
  private pressurePoolId: string;

  constructor(
    private readonly provider: HostMemoryProvider,
    private readonly config: MemorySafetyConfig,
    private readonly bypassAdmission = false,
  ) {
    this.pressurePoolId = provider.topology.system.id;
  }

  async poll(): Promise<MemorySafetyTransition> {
    if (this.bypassAdmission) return this.currentTransition();
    return await this.exclusive(async () =>
      this.observe(await this.provider.snapshot()),
    );
  }

  async reserve(
    request: RuntimeMemoryReservationRequest,
  ): Promise<RuntimeMemoryReservation> {
    return await this.exclusive(async () => {
      if (this.reservations.has(request.runtimeId)) {
        throw new Error(
          `Runtime "${request.runtimeId}" already has a memory reservation.`,
        );
      }

      const projectedDemand = this.bypassAdmission
        ? []
        : this.evaluate(request, await this.provider.snapshot());

      const token = Symbol(request.runtimeId);
      this.reservations.set(request.runtimeId, {
        token,
        projectedDemand,
        state: "pending",
      });
      return Object.freeze({
        runtimeId: request.runtimeId,
        materialize: () => this.materialize(request.runtimeId, token),
        release: () => this.release(request.runtimeId, token),
      });
    });
  }

  private materialize(runtimeId: string, token: symbol): void {
    const current = this.reservations.get(runtimeId);
    if (current?.token === token) current.state = "resident";
  }

  private evaluate(
    request: RuntimeMemoryReservationRequest,
    snapshot: HostMemorySnapshot,
  ): readonly ProjectedMemoryDemand[] {
    const topology = this.provider.topology;
    const transition = this.observe(snapshot);
    if (transition.action !== "allow") {
      throw new RuntimeMemoryAdmissionError({
        kind: "rejected",
        reason: "memory-pressure",
        poolId: this.pressurePoolId,
      });
    }

    const requiresAccelerator =
      topology.kind === "discrete" && request.demand.acceleratorBytes > 0;
    if (requiresAccelerator && topology.accelerators.length !== 1) {
      throw new RuntimeMemoryAdmissionError({
        kind: "rejected",
        reason: "measurement-unavailable",
        poolId: "accelerator",
      });
    }
    const acceleratorPoolId = requiresAccelerator
      ? topology.accelerators[0]!.id
      : undefined;

    const decision = evaluateMemoryAdmission({
      topology,
      snapshot: subtractReservations(snapshot, this.reservations.values()),
      config: this.config,
      demand: request.demand,
      ...(acceleratorPoolId ? { acceleratorPoolId } : {}),
    });
    if (decision.kind === "rejected") {
      throw new RuntimeMemoryAdmissionError(decision);
    }
    return decision.projectedDemand;
  }

  private observe(snapshot: HostMemorySnapshot): MemorySafetyTransition {
    const observation = observeMemoryPressure({
      topology: this.provider.topology,
      snapshot,
      config: this.config,
    });
    const transition = transitionMemorySafetyState(
      this.hysteresis,
      observation.pressure,
    );
    this.hysteresis = transition.current;
    if (observation.pressure !== "normal") {
      this.pressurePoolId = observation.poolId;
    }
    return transition;
  }

  private currentTransition(): MemorySafetyTransition {
    return {
      previous: this.hysteresis,
      current: this.hysteresis,
      action: "allow",
    };
  }

  private release(runtimeId: string, token: symbol): void {
    const current = this.reservations.get(runtimeId);
    if (current?.token === token) this.reservations.delete(runtimeId);
  }

  private async exclusive<Value>(
    operation: () => Promise<Value>,
  ): Promise<Value> {
    const next = this.operations.then(operation, operation);
    this.operations = next.then(
      () => undefined,
      () => undefined,
    );
    return await next;
  }
}
