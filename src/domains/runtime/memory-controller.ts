import type { HostMemoryProvider } from "./memory/host-memory-provider";
import {
  evaluateMemoryAdmission,
  type HostMemorySnapshot,
  type MemoryAdmissionDecision,
  type MemoryPoolSnapshot,
  type MemorySafetyConfig,
  type ProjectedMemoryDemand,
  type RuntimeMemoryDemand,
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

  constructor(
    private readonly provider: HostMemoryProvider,
    private readonly config: MemorySafetyConfig,
    private readonly bypassAdmission = false,
  ) {}

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
        : await this.evaluate(request);

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

  private async evaluate(
    request: RuntimeMemoryReservationRequest,
  ): Promise<readonly ProjectedMemoryDemand[]> {
    const topology = this.provider.topology;
    const snapshot = subtractReservations(
      await this.provider.snapshot(),
      this.reservations.values(),
    );

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
      snapshot,
      config: this.config,
      demand: request.demand,
      ...(acceleratorPoolId ? { acceleratorPoolId } : {}),
    });
    if (decision.kind === "rejected") {
      throw new RuntimeMemoryAdmissionError(decision);
    }
    return decision.projectedDemand;
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
