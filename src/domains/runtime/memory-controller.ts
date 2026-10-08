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
  projectRuntimeMemoryDemand,
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

/**
 * Set when a runtime can never be admitted on this host, even with every other
 * runtime stopped, because its demand exceeds the pool's usable capacity.
 */
export type RuntimeMemoryCapacityShortfall = Readonly<{
  poolId: string;
  requiredBytes: number;
  usableBytes: number;
  capacityBytes: number;
}>;

/** Rejects backend starts that would violate the current host-memory policy. */
export class RuntimeMemoryAdmissionError extends Error {
  constructor(
    readonly decision: Extract<MemoryAdmissionDecision, { kind: "rejected" }>,
    readonly diagnostics?: MemoryAdmissionDiagnostics,
    /** Present only for permanent failures; absent means retrying may help. */
    readonly capacity?: RuntimeMemoryCapacityShortfall,
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

  get topology() {
    return this.provider.topology;
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

  /**
   * Side-effect-free admission probe. Evaluates the request as if the given
   * runtimes were already stopped, without creating a reservation, advancing
   * hysteresis, or mutating any state. Returns the rejection, if any.
   *
   * Freed memory is estimated from the releasing runtimes' projected demand.
   */
  async checkAdmission(
    request: Omit<RuntimeMemoryReservationRequest, "runtimeId"> & {
      runtimeId?: string;
    },
    options: { releasingRuntimeIds?: readonly string[] } = {},
  ): Promise<RuntimeMemoryAdmissionError | undefined> {
    if (this.bypassAdmission) return undefined;
    return await this.exclusive(async () => {
      const releasing = new Set(options.releasingRuntimeIds ?? []);
      try {
        this.evaluate(
          {
            runtimeId: request.runtimeId ?? "admission-check",
            demand: request.demand,
          },
          await this.provider.snapshot(),
          releasing,
        );
        return undefined;
      } catch (error) {
        if (error instanceof RuntimeMemoryAdmissionError) return error;
        throw error;
      }
    });
  }

  private materialize(runtimeId: string, token: symbol): void {
    const current = this.reservations.get(runtimeId);
    if (current?.token === token) current.state = "resident";
  }

  private evaluate(
    request: RuntimeMemoryReservationRequest,
    rawSnapshot: HostMemorySnapshot,
    releasing: ReadonlySet<string> = new Set(),
  ): readonly ProjectedMemoryDemand[] {
    const topology = this.provider.topology;
    const remaining = [...this.reservations].filter(
      ([id]) => !releasing.has(id),
    );
    const pending = pendingDemandByPool(remaining.map(([, entry]) => entry));
    const snapshot = this.addReleasedMemory(rawSnapshot, releasing);
    const capacity = this.capacityShortfall(request);
    if (capacity) {
      this.reject(
        request,
        snapshot,
        pending,
        {
          kind: "rejected",
          reason:
            capacity.poolId === topology.system.id
              ? "system-memory"
              : "accelerator-memory",
          poolId: capacity.poolId,
        },
        capacity,
      );
    }
    // Only poll() advances hysteresis; reserve() must not count as a sample.
    if (this.hysteresis.state !== "healthy") {
      this.reject(request, snapshot, pending, {
        kind: "rejected",
        reason: "memory-pressure",
        poolId: this.pressurePoolId,
      });
    }
    const observation = observeMemoryPressure({
      topology,
      snapshot,
      config: this.config,
    });
    if (
      observation.pressure === "constrained" ||
      observation.pressure === "critical"
    ) {
      this.reject(request, snapshot, pending, {
        kind: "rejected",
        reason: "memory-pressure",
        poolId: observation.poolId,
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

  /** Adds back the resident bytes of runtimes that are about to be stopped. */
  private addReleasedMemory(
    snapshot: HostMemorySnapshot,
    releasing: ReadonlySet<string>,
  ): HostMemorySnapshot {
    if (releasing.size === 0) return snapshot;
    const freed = new Map<string, number>();
    for (const id of releasing) {
      const entry = this.reservations.get(id);
      // Pending reservations are not part of the measured sample.
      if (entry?.state !== "resident") continue;
      for (const demand of entry.projectedDemand) {
        freed.set(
          demand.poolId,
          (freed.get(demand.poolId) ?? 0) + demand.bytes,
        );
      }
    }
    return {
      ...snapshot,
      pools: snapshot.pools.map((pool) =>
        pool.availability === "unavailable"
          ? pool
          : {
              ...pool,
              availableBytes:
                pool.availableBytes + (freed.get(pool.poolId) ?? 0),
            },
      ),
    };
  }

  /** Detects demand that exceeds a pool's usable capacity on an idle host. */
  private capacityShortfall(
    request: RuntimeMemoryReservationRequest,
  ): RuntimeMemoryCapacityShortfall | undefined {
    const topology = this.provider.topology;
    const requiresAccelerator =
      topology.kind === "discrete" && request.demand.acceleratorBytes > 0;
    if (requiresAccelerator && topology.accelerators.length !== 1) {
      return undefined;
    }
    const projected = projectRuntimeMemoryDemand({
      topology,
      demand: request.demand,
      ...(requiresAccelerator && topology.kind === "discrete"
        ? { acceleratorPoolId: topology.accelerators[0]!.id }
        : {}),
    });
    for (const demand of projected) {
      const isSystem = demand.poolId === topology.system.id;
      const pool = isSystem
        ? topology.system
        : topology.kind === "discrete"
          ? topology.accelerators.find(({ id }) => id === demand.poolId)
          : undefined;
      if (!pool) continue;
      // A zero capacity is used for pools whose capacity has not yet been
      // measured. Do not turn that unknown state into a permanent rejection.
      if (pool.capacityBytes <= 0) continue;
      const usableBytes = Math.max(
        0,
        pool.capacityBytes -
          effectiveMemoryReserveBytes(
            isSystem
              ? this.config.systemReserve
              : this.config.acceleratorReserve,
            pool.capacityBytes,
          ),
      );
      if (demand.bytes > usableBytes) {
        return {
          poolId: pool.id,
          requiredBytes: demand.bytes,
          usableBytes,
          capacityBytes: pool.capacityBytes,
        };
      }
    }
    return undefined;
  }

  private reject(
    request: RuntimeMemoryReservationRequest,
    snapshot: HostMemorySnapshot,
    pending: ReadonlyMap<string, number>,
    decision: Extract<MemoryAdmissionDecision, { kind: "rejected" }>,
    capacity?: RuntimeMemoryCapacityShortfall,
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

    throw new RuntimeMemoryAdmissionError(
      decision,
      {
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
      },
      capacity,
    );
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
