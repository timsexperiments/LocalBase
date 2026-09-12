import type { HostMemoryProvider } from "./memory/host-memory-provider";
import {
  evaluateMemoryAdmission,
  effectiveMemoryReserveBytes,
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

function pendingDemandByPool(
  reservations: Iterable<ReservationEntry>,
): ReadonlyMap<string, number> {
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
  return reserved;
}

function subtractReservations(
  snapshot: HostMemorySnapshot,
  reserved: ReadonlyMap<string, number>,
): HostMemorySnapshot {
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

/** The deciding sample and policy inputs, for server diagnostics only. */
type MemoryAdmissionDiagnostics = Readonly<{
  measured_available_bytes: number | "unavailable";
  reserve_bytes: number | "unavailable";
  pending_bytes: number | "unavailable";
  requested_bytes: number | "unavailable";
  effective_available_bytes: number | "unavailable";
  sample_captured_at_ms: number;
  sample_age_ms: number;
  measured_pressure: MemoryPoolSnapshot["pressure"];
  safety_state: MemorySafetyHysteresis["state"];
  recovery_samples: number;
  demand_confidence: RuntimeMemoryDemand["confidence"];
}>;

/** Rejects backend starts that would violate the current host-memory policy. */
export class RuntimeMemoryAdmissionError extends Error {
  constructor(
    readonly decision: Extract<MemoryAdmissionDecision, { kind: "rejected" }>,
    readonly diagnostics?: MemoryAdmissionDiagnostics,
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
    const pending = pendingDemandByPool(this.reservations.values());
    const transition = this.observe(snapshot);
    if (transition.action !== "allow") {
      this.reject(request, snapshot, pending, {
        kind: "rejected",
        reason: "memory-pressure",
        poolId: this.pressurePoolId,
      });
    }

    const requiresAccelerator =
      topology.kind === "discrete" && request.demand.acceleratorBytes > 0;
    if (requiresAccelerator && topology.accelerators.length !== 1) {
      this.reject(request, snapshot, pending, {
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
      snapshot: subtractReservations(snapshot, pending),
      config: this.config,
      demand: request.demand,
      ...(acceleratorPoolId ? { acceleratorPoolId } : {}),
    });
    if (decision.kind === "rejected") {
      this.reject(request, snapshot, pending, decision);
    }
    return decision.projectedDemand;
  }

  private reject(
    request: RuntimeMemoryReservationRequest,
    snapshot: HostMemorySnapshot,
    pending: ReadonlyMap<string, number>,
    decision: Extract<MemoryAdmissionDecision, { kind: "rejected" }>,
  ): never {
    const topology = this.provider.topology;
    const isSystem = decision.poolId === topology.system.id;
    const pool = isSystem
      ? topology.system
      : topology.kind === "discrete"
        ? topology.accelerators.find(({ id }) => id === decision.poolId)
        : undefined;
    const observed = snapshot.pools.find(
      ({ poolId }) => poolId === decision.poolId,
    );
    const pendingBytes = pool ? (pending.get(pool.id) ?? 0) : "unavailable";
    const measuredBytes =
      observed?.availability === "available"
        ? observed.availableBytes
        : "unavailable";

    throw new RuntimeMemoryAdmissionError(decision, {
      measured_available_bytes: measuredBytes,
      reserve_bytes: pool
        ? effectiveMemoryReserveBytes(
            isSystem
              ? this.config.systemReserve
              : this.config.acceleratorReserve,
            pool.capacityBytes,
          )
        : "unavailable",
      pending_bytes: pendingBytes,
      requested_bytes:
        topology.kind === "unified"
          ? request.demand.unifiedBytes
          : isSystem
            ? request.demand.hostBytes
            : request.demand.acceleratorBytes === 0 ||
                topology.accelerators.length === 1
              ? request.demand.acceleratorBytes
              : "unavailable",
      effective_available_bytes:
        typeof measuredBytes === "number" && typeof pendingBytes === "number"
          ? Math.max(0, measuredBytes - pendingBytes)
          : "unavailable",
      sample_captured_at_ms: snapshot.capturedAtMs,
      sample_age_ms: Math.max(0, Date.now() - snapshot.capturedAtMs),
      measured_pressure: observed?.pressure ?? "unknown",
      safety_state: this.hysteresis.state,
      recovery_samples: this.hysteresis.consecutiveNormalSnapshots,
      demand_confidence: request.demand.confidence,
    });
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
