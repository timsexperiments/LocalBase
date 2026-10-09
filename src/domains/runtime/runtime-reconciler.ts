import type { ILogger } from "../observability/logging";
import type { LocalBaseConfig } from "../../manager";
import {
  type RuntimeConfigController,
  type RuntimeConfigSnapshot,
} from "./config-snapshot";
import { ModalityAdmissionBarrier } from "./modality-admission";
import type { RuntimeMemoryAdmissionError } from "./memory-controller";
import type { RuntimeLifecycleSnapshot } from "./lifecycle-snapshot";
import {
  InferenceQueue,
  InferenceQueueAbortedError,
  InferenceQueueUnavailableError,
  type InferenceDispatchLease,
  type InferencePermitSnapshot,
} from "./inference-queue";
import {
  modalityComponents,
  runtimeModalities,
  type RuntimeModality,
} from "./modality";
import {
  configuredRuntimeModality,
  createRuntimeReconciliationPlan,
  type RuntimeOverrideOwnership,
} from "./reconciliation-plan";
import type { RuntimeSupervisorFactory } from "./supervisor-factory";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";

export type RuntimeAdmission = Readonly<{
  modality: RuntimeModality;
  snapshot: RuntimeConfigSnapshot;
  supervisor: RuntimeSupervisor;
  ready: Promise<void>;
  onPendingDetach: (callback: () => void) => void;
  onIdleCancellation: (callback: () => void) => void;
  markResponseStarted: () => void;
  cancel: () => void;
  release: () => void;
}>;

type RuntimeLease = Omit<RuntimeAdmission, "ready">;

type PreparedModelAdmission = Readonly<{
  modelId: string;
  admission: RuntimeAdmission;
}>;

type ModelAdmission = PreparedModelAdmission &
  Readonly<{
    queueWaitMs: number;
    admissionSnapshot: InferencePermitSnapshot;
  }>;

type ModelAdmissionFailure =
  | Readonly<{ kind: "not-configured" }>
  | Readonly<{ kind: "model-not-found" }>
  | Readonly<{ kind: "unavailable" }>
  | Readonly<{
      kind: "insufficient-memory";
      error: RuntimeMemoryAdmissionError;
    }>;

/** Raised before any state changes when the requested model cannot fit. */
class ModelSwitchRejectedError extends Error {
  constructor(readonly cause: RuntimeMemoryAdmissionError) {
    super(cause.message);
  }
}

type PreparedModelAdmissionResult =
  | Readonly<{ kind: "admitted"; value: PreparedModelAdmission }>
  | ModelAdmissionFailure;

export type ModelAdmissionResult =
  Readonly<{ kind: "admitted"; value: ModelAdmission }> | ModelAdmissionFailure;

const MEMORY_SETTLE_TIMEOUT_MS = 3_000;
const MEMORY_SETTLE_POLL_INTERVAL_MS = 100;

type ConfiguredModalities = Record<RuntimeModality, boolean>;
type ModalityTransitions = Record<RuntimeModality, Promise<void>>;

export type RuntimeReconciliationHooks = Readonly<{
  beforeModalityDrain?: (modality: RuntimeModality) => Promise<void>;
}>;

type CoordinatedSnapshot = Readonly<{
  snapshot: RuntimeConfigSnapshot;
  transitions: Readonly<ModalityTransitions>;
}>;

export class RuntimeRequestAbortedError extends Error {
  constructor() {
    super("Request aborted before runtime admission.");
    this.name = "RuntimeRequestAbortedError";
  }
}

class ModelNoLongerSelectedError extends Error {}

function configuredModalities(
  snapshot: RuntimeConfigSnapshot,
  ownership: RuntimeOverrideOwnership,
): ConfiguredModalities {
  return Object.fromEntries(
    runtimeModalities.map((modality) => [
      modality,
      configuredRuntimeModality(modality, snapshot.config, ownership),
    ]),
  ) as ConfiguredModalities;
}

function activeModelField(
  modality: RuntimeModality,
):
  | "activeLlmModel"
  | "activeSttModel"
  | "activeTtsModel"
  | "activeImageModel"
  | "activeVideoModel" {
  if (modality === "llm") return "activeLlmModel";
  if (modality === "stt") return "activeSttModel";
  if (modality === "tts") return "activeTtsModel";
  if (modality === "image") return "activeImageModel";
  return "activeVideoModel";
}

function activeModel(
  modality: RuntimeModality,
  config: RuntimeConfigSnapshot["config"],
): string {
  return config[activeModelField(modality)];
}

function selectedModels(
  modality: RuntimeModality,
  config: RuntimeConfigSnapshot["config"],
): readonly string[] {
  if (modality === "llm") return config.selectedLlmModels;
  if (modality === "stt") return config.selectedSttModels;
  if (modality === "tts") return config.selectedTtsModels;
  if (modality === "image") return config.selectedImageModels;
  return config.selectedVideoModels;
}

/** Applies persisted runtime changes while preserving the gateway listener. */
export class RuntimeReconciler {
  private readonly barriers: Record<RuntimeModality, ModalityAdmissionBarrier>;
  private readonly ownedFields: ReadonlySet<keyof LocalBaseConfig>;
  private configured: ConfiguredModalities;
  /** Latest persisted configuration observed by short coordination. */
  private snapshot: RuntimeConfigSnapshot;
  /** Configuration generation currently represented by each supervisor. */
  private readonly appliedSnapshots: Record<
    RuntimeModality,
    RuntimeConfigSnapshot
  >;
  private readonly reconciliationScheduled = new Set<RuntimeModality>();
  private readonly pendingModelReferences = new Set<ReadonlySet<string>>();
  private readonly switchPreflights = new Map<
    RuntimeModality,
    AbortController
  >();
  /** Recovery owns detached barriers until its claim is released. */
  private readonly recoveryClaims = new Map<RuntimeModality, symbol>();
  private transitions = Promise.resolve();
  private readonly modalityTransitions: ModalityTransitions =
    Object.fromEntries(
      runtimeModalities.map((modality) => [modality, Promise.resolve()]),
    ) as Record<RuntimeModality, Promise<void>>;
  private sharedRefresh: Promise<RuntimeConfigSnapshot> | undefined;
  private readonly queues: Record<
    RuntimeModality,
    InferenceQueue<PreparedModelAdmissionResult>
  >;

  constructor(
    private readonly controller: RuntimeConfigController,
    private readonly ownership: RuntimeOverrideOwnership,
    private readonly supervisors: SupervisorRegistry,
    private readonly factory: RuntimeSupervisorFactory,
    private readonly logger: Pick<ILogger, "event">,
    queueOptions: Readonly<{ maxWaiting?: number; waitMs?: number }> = {},
    private readonly hooks: RuntimeReconciliationHooks = {},
  ) {
    this.snapshot = controller.read();
    this.appliedSnapshots = Object.fromEntries(
      runtimeModalities.map((modality) => [modality, this.snapshot]),
    ) as Record<RuntimeModality, RuntimeConfigSnapshot>;
    this.configured = configuredModalities(this.snapshot, ownership);
    this.ownedFields = new Set(ownership.configFields ?? []);
    this.barriers = Object.fromEntries(
      runtimeModalities.map((modality) => [
        modality,
        new ModalityAdmissionBarrier(modality, this.configured[modality]),
      ]),
    ) as Record<RuntimeModality, ModalityAdmissionBarrier>;
    this.queues = Object.fromEntries(
      runtimeModalities.map((modality) => [
        modality,
        new InferenceQueue(modality, {
          ...queueOptions,
          isAdmitted: (result) => result.kind === "admitted",
          resolvedSlots: (result) =>
            result.kind === "admitted"
              ? (result.value.admission.supervisor.resolvedSlots?.() ?? 1)
              : 1,
          whenSlotsResolved: async (result) => {
            if (result.kind === "admitted") await result.value.admission.ready;
          },
          discard: (result) => {
            if (result.kind === "admitted") result.value.admission.cancel();
          },
        }),
      ]),
    ) as Record<RuntimeModality, InferenceQueue<PreparedModelAdmissionResult>>;
    this.supervisors.setAdmissionReader((modality) =>
      this.barriers[modality].snapshot(),
    );
  }

  read(): RuntimeConfigSnapshot {
    return this.snapshot;
  }

  configuredModalities(): Readonly<ConfiguredModalities> {
    return Object.freeze({ ...this.configured });
  }

  protectedModelIds(): ReadonlySet<string> {
    const ids = new Set<string>();
    for (const modality of runtimeModalities) {
      if (this.supervisors.get(modality)) {
        const modelId = activeModel(
          modality,
          this.appliedSnapshots[modality].config,
        );
        if (modelId) ids.add(modelId);
      }
    }
    for (const pending of this.pendingModelReferences)
      for (const modelId of pending) if (modelId) ids.add(modelId);
    return ids;
  }

  lifecycleSnapshot(): Readonly<
    Record<RuntimeModality, RuntimeLifecycleSnapshot>
  > {
    return Object.freeze(
      Object.fromEntries(
        runtimeModalities.map((modality) => [
          modality,
          (() => {
            const applied = this.appliedSnapshots[modality];
            const modelId = this.supervisors.get(modality)
              ? activeModel(modality, applied.config) || null
              : null;
            return this.supervisors.lifecycleSnapshot({
              modality,
              configured: configuredRuntimeModality(
                modality,
                applied.config,
                this.ownership,
              ),
              modelId,
              admission: this.barriers[modality].snapshot(),
              queue: this.queues[modality].snapshot(modelId ?? undefined),
            });
          })(),
        ]),
      ) as Record<RuntimeModality, RuntimeLifecycleSnapshot>,
    );
  }

  async refresh(): Promise<RuntimeConfigSnapshot> {
    if (!this.sharedRefresh) {
      this.sharedRefresh = this.coordinateRefresh();
    }
    const refresh = this.sharedRefresh;
    try {
      return await refresh;
    } finally {
      if (this.sharedRefresh === refresh) this.sharedRefresh = undefined;
    }
  }

  async refreshConfiguration(): Promise<RuntimeConfigSnapshot> {
    return (await this.coordinate()).snapshot;
  }

  async evictIdleRuntimes(excludedModality?: RuntimeModality): Promise<void> {
    await Promise.all(
      runtimeModalities.map((modality) =>
        this.exclusiveModality(modality, async () => {
          if (modality === excludedModality) return;
          const supervisor = this.supervisors.get(modality);
          if (!supervisor || supervisor.state() !== "running") return;
          const barrier = this.barriers[modality];
          if (!barrier.detachIfIdle()) return;
          try {
            await supervisor.kill();
          } finally {
            this.attachBarrier(modality);
          }
        }),
      ),
    );
  }

  /**
   * Checks whether stopping every currently eligible idle peer would make the
   * requested model admissible, without stopping peers or changing admission
   * state.
   */
  async canAdmitAfterIdleEviction(
    modality: RuntimeModality,
    modelId: string,
    signal?: AbortSignal,
  ): Promise<boolean> {
    return await this.projectCanAdmitAfterIdleEviction(
      modality,
      modelId,
      signal,
    );
  }

  /** Claims idle peers before projecting or stopping them, closing the gap in
   * which a peer could accept work after it was counted as releasable. */
  async recoverWithIdleEviction(
    modality: RuntimeModality,
    modelId: string,
    signal?: AbortSignal,
  ): Promise<boolean> {
    const claims: Array<{
      modality: RuntimeModality;
      supervisor: RuntimeSupervisor;
      token: symbol;
    }> = [];
    try {
      await Promise.all(
        runtimeModalities.map((peerModality) =>
          this.exclusiveModality(
            peerModality,
            async () => {
              if (peerModality === modality) return;
              const supervisor = this.supervisors.get(peerModality);
              if (!supervisor || supervisor.state() !== "running") return;
              if (!this.barriers[peerModality].detachIfIdle()) return;
              const token = Symbol(peerModality);
              this.recoveryClaims.set(peerModality, token);
              claims.push({ modality: peerModality, supervisor, token });
            },
            signal,
          ),
        ),
      );
      this.throwIfAborted(signal);
      const canAdmit = await this.projectCanAdmitAfterIdleEviction(
        modality,
        modelId,
        signal,
        claims.map(({ supervisor }) => supervisor.runtimeId()),
      );
      this.throwIfAborted(signal);
      if (!canAdmit) return false;

      const allClaimsStillCurrent = claims.every(
        (claim) =>
          this.recoveryClaimIsCurrent(claim) &&
          this.supervisors.get(claim.modality) === claim.supervisor &&
          claim.supervisor.state() === "running",
      );
      if (!allClaimsStillCurrent) return false;

      const stopped = await this.exclusiveModalities(
        runtimeModalities.filter((peerModality) =>
          claims.some((claim) => claim.modality === peerModality),
        ),
        async () => {
          this.throwIfAborted(signal);
          if (
            !claims.every((claim) => {
              const admission = this.barriers[claim.modality].snapshot();
              return (
                this.recoveryClaimIsCurrent(claim) &&
                this.supervisors.get(claim.modality) === claim.supervisor &&
                claim.supervisor.state() === "running" &&
                admission.kind === "known" &&
                admission.activeCount === 0 &&
                !admission.accepting
              );
            })
          ) {
            return false;
          }
          for (const claim of claims) {
            this.throwIfAborted(signal);
            await claim.supervisor.kill();
          }
          this.throwIfAborted(signal);
          return true;
        },
        signal,
      );
      return stopped;
    } finally {
      for (const claim of claims) {
        if (this.recoveryClaimIsCurrent(claim))
          this.recoveryClaims.delete(claim.modality);
      }
      const releaseClaims = Promise.all(
        claims.map(({ modality: peerModality }) =>
          this.exclusiveModality(peerModality, async () => {
            if (this.recoveryClaims.has(peerModality)) return;
            this.attachBarrier(peerModality);
          }),
        ),
      );
      if (signal?.aborted) {
        void releaseClaims.catch(() => {});
      } else {
        await releaseClaims;
      }
    }
  }

  /**
   * Waits for an uncredited host-memory sample to admit a model after runtimes
   * have stopped. Projected credits are used to decide whether eviction is
   * worthwhile, but are never carried into the actual post-stop admission.
   */
  async waitForAdmissionAfterEviction(
    modality: RuntimeModality,
    modelId: string,
    signal?: AbortSignal,
    source: RuntimeConfigSnapshot = this.snapshot,
  ): Promise<boolean> {
    if (!configuredRuntimeModality(modality, source.config, this.ownership))
      return false;
    const candidate = this.factory.create(modality, {
      ...source,
      config: { ...source.config, [activeModelField(modality)]: modelId },
    });
    if (!candidate.preflight) return true;
    const stillToBeStopped =
      modality === "video" &&
      modelId !== activeModel(modality, this.appliedSnapshots[modality].config)
        ? [this.supervisors.get(modality)?.runtimeId()].filter(
            (runtimeId): runtimeId is string => runtimeId !== undefined,
          )
        : [];

    const deadline = Date.now() + MEMORY_SETTLE_TIMEOUT_MS;
    while (true) {
      this.throwIfAborted(signal);
      try {
        const rejection = await this.waitForAbort(
          candidate.preflight(stillToBeStopped, signal),
          signal,
        );
        if (!rejection) return true;
        if (rejection.capacity) return false;
      } catch (error) {
        if (signal?.aborted || error instanceof RuntimeRequestAbortedError)
          throw new RuntimeRequestAbortedError();
        return false;
      }

      const remainingMs = deadline - Date.now();
      if (remainingMs <= 0) return false;
      await this.waitForAbort(
        new Promise<void>((resolve) =>
          setTimeout(
            resolve,
            Math.min(MEMORY_SETTLE_POLL_INTERVAL_MS, remainingMs),
          ),
        ),
        signal,
      );
    }
  }

  private async projectCanAdmitAfterIdleEviction(
    modality: RuntimeModality,
    modelId: string,
    signal?: AbortSignal,
    claimedRuntimeIds?: readonly string[],
  ): Promise<boolean> {
    try {
      const source = this.snapshot;
      if (!configuredRuntimeModality(modality, source.config, this.ownership))
        return false;
      const candidate = this.factory.create(modality, {
        ...source,
        config: { ...source.config, [activeModelField(modality)]: modelId },
      });
      if (!candidate.preflight) return false;

      const releasingRuntimeIds = [
        ...(claimedRuntimeIds ??
          runtimeModalities.flatMap((peerModality) => {
            if (peerModality === modality) return [];
            const supervisor = this.supervisors.get(peerModality);
            const admission = this.barriers[peerModality].snapshot();
            if (
              !supervisor ||
              supervisor.state() !== "running" ||
              admission.kind !== "known" ||
              !admission.accepting ||
              admission.activeCount !== 0
            ) {
              return [];
            }
            return [supervisor.runtimeId()];
          })),
      ];
      if (
        modality === "video" &&
        modelId !== activeModel("video", this.appliedSnapshots.video.config)
      ) {
        const currentVideo = this.supervisors.get("video");
        if (currentVideo) releasingRuntimeIds.push(currentVideo.runtimeId());
      }

      const rejection = await this.waitForAbort(
        candidate.preflight(releasingRuntimeIds, signal),
        signal,
      );
      return rejection === undefined;
    } catch (error) {
      if (signal?.aborted || error instanceof RuntimeRequestAbortedError)
        throw new RuntimeRequestAbortedError();
      return false;
    }
  }

  async evictAllRuntimes(): Promise<void> {
    for (const preflight of this.switchPreflights.values()) preflight.abort();
    this.rejectQueued("Inference rejected by memory emergency.");
    const results = await Promise.allSettled(
      runtimeModalities.map((modality) =>
        this.exclusiveModality(modality, async () => {
          const supervisor = this.supervisors.get(modality);
          if (!supervisor) return;
          this.supervisors.markDraining(modality);
          const barrier = this.barriers[modality];
          const drain = barrier.drainWithoutCancellation();
          let killError: unknown;
          try {
            await supervisor.kill();
          } catch (error) {
            killError = error;
          }
          await drain;
          this.supervisors.clearDraining(modality);
          this.attachBarrier(modality);
          if (killError) throw killError;
        }),
      ),
    );
    const failure = results.find(
      (result): result is PromiseRejectedResult => result.status === "rejected",
    );
    if (failure) throw failure.reason;
  }

  rejectQueued(message: string): void {
    for (const queue of Object.values(this.queues))
      queue.rejectPending(new InferenceQueueUnavailableError(message));
  }

  closeQueues(): void {
    for (const queue of Object.values(this.queues)) queue.close();
  }

  async admit(
    modality: RuntimeModality,
  ): Promise<RuntimeAdmission | undefined> {
    const coordinated = await this.coordinate();
    return await this.exclusiveModality(modality, async () => {
      await coordinated.transitions[modality];
      const admission = this.acquire(modality, this.appliedSnapshots[modality]);
      return admission ? this.prepare(admission) : undefined;
    });
  }

  async admitModel(
    modality: RuntimeModality,
    requestedModel: string | undefined,
    signal?: AbortSignal,
  ): Promise<ModelAdmissionResult> {
    const captured = await this.coordinate();
    const modelId =
      requestedModel ?? activeModel(modality, captured.snapshot.config);
    const queuedAdmission = this.queues[modality].acquire(
      modelId,
      async (dispatchLease) =>
        await this.coordinateAdmission(
          modality,
          modelId,
          signal,
          captured,
          dispatchLease,
        ),
      signal,
    );
    try {
      const queued = await this.waitForAbort(queuedAdmission, signal);
      const result = queued.value;
      if (signal?.aborted) {
        if (result.kind === "admitted") result.value.admission.cancel();
        queued.release();
        throw new RuntimeRequestAbortedError();
      }
      if (result.kind !== "admitted") {
        queued.release();
        return result;
      }
      const admission = result.value.admission;
      let settled = false;
      const settle = (cancel: boolean) => {
        if (settled) return;
        settled = true;
        try {
          cancel ? admission.cancel() : admission.release();
        } finally {
          queued.release();
        }
      };
      return {
        kind: "admitted",
        value: {
          ...result.value,
          queueWaitMs: queued.queueWaitMs,
          admissionSnapshot: queued.permitSnapshot,
          admission: Object.freeze({
            ...admission,
            cancel: () => settle(true),
            release: () => settle(false),
          }),
        },
      };
    } catch (error) {
      if (
        error instanceof InferenceQueueAbortedError ||
        error instanceof RuntimeRequestAbortedError
      ) {
        void queuedAdmission.then(
          (queued) => {
            if (queued.value.kind === "admitted")
              queued.value.value.admission.cancel();
            queued.release();
          },
          () => {},
        );
        throw new RuntimeRequestAbortedError();
      }
      throw error;
    }
  }

  private async coordinateAdmission(
    modality: RuntimeModality,
    requestedModel: string | undefined,
    signal: AbortSignal | undefined,
    coordinated?: CoordinatedSnapshot,
    dispatchLease?: InferenceDispatchLease,
  ): Promise<PreparedModelAdmissionResult> {
    coordinated ??= await this.coordinate();
    return await this.exclusiveModality(modality, async () => {
      await coordinated.transitions[modality];
      this.throwIfAborted(signal);
      dispatchLease?.throwIfCancelled();
      dispatchLease?.start();
      const desiredSnapshot = this.snapshot;
      if (
        !configuredRuntimeModality(
          modality,
          desiredSnapshot.config,
          this.ownership,
        )
      ) {
        return { kind: "not-configured" };
      }
      const modelId = this.resolveRequestedModel(
        modality,
        requestedModel,
        desiredSnapshot,
      );
      if (!modelId) return { kind: "model-not-found" };
      let admissionSnapshot = this.appliedSnapshots[modality];
      if (
        modelId !== activeModel(modality, admissionSnapshot.config) &&
        !this.ownedFields.has(activeModelField(modality))
      ) {
        try {
          admissionSnapshot = await this.activateModel(
            modality,
            modelId,
            desiredSnapshot,
            signal,
            dispatchLease,
          );
        } catch (error) {
          if (error instanceof ModelNoLongerSelectedError)
            return { kind: "model-not-found" };
          if (error instanceof ModelSwitchRejectedError) {
            return { kind: "insufficient-memory", error: error.cause };
          }
          throw error;
        }
      }
      this.throwIfAborted(signal);
      dispatchLease?.throwIfCancelled();
      const admission = this.acquire(modality, admissionSnapshot);
      if (!admission) return { kind: "unavailable" };
      dispatchLease?.throwIfCancelled();
      const prepared = this.prepare(admission);
      try {
        dispatchLease?.throwIfCancelled();
      } catch (error) {
        prepared.cancel();
        throw error;
      }
      return {
        kind: "admitted",
        value: { modelId, admission: prepared },
      };
    });
  }

  private async exclusive<Value>(work: () => Promise<Value>): Promise<Value> {
    const next = this.transitions.then(work, work);
    this.transitions = next.then(
      () => undefined,
      () => undefined,
    );
    return await next;
  }

  private async exclusiveModality<Value>(
    modality: RuntimeModality,
    work: () => Promise<Value>,
    signal?: AbortSignal,
  ): Promise<Value> {
    const previous = this.modalityTransitions[modality];
    const run = async () => {
      this.throwIfAborted(signal);
      return await work();
    };
    const next = previous.then(run, run);
    this.modalityTransitions[modality] = next.then(
      () => undefined,
      () => undefined,
    );
    return await this.waitForAbort(next, signal);
  }

  private async exclusiveModalities<Value>(
    modalities: readonly RuntimeModality[],
    work: () => Promise<Value>,
    signal?: AbortSignal,
  ): Promise<Value> {
    const [modality, ...remaining] = modalities;
    if (!modality) return await work();
    return await this.exclusiveModality(
      modality,
      async () => await this.exclusiveModalities(remaining, work, signal),
      signal,
    );
  }

  private recoveryClaimIsCurrent(claim: {
    modality: RuntimeModality;
    token: symbol;
  }): boolean {
    return this.recoveryClaims.get(claim.modality) === claim.token;
  }

  /** Reattachment invalidates recovery ownership before reopening admission. */
  private attachBarrier(modality: RuntimeModality): void {
    this.recoveryClaims.delete(modality);
    if (this.configured[modality]) this.barriers[modality].attach();
  }

  private async coordinate(): Promise<CoordinatedSnapshot> {
    return await this.exclusive(async () =>
      this.scheduleReconciliation(await this.controller.refresh()),
    );
  }

  private async coordinateRefresh(): Promise<RuntimeConfigSnapshot> {
    const coordinated = await this.coordinate();
    await Promise.all(Object.values(coordinated.transitions));
    return coordinated.snapshot;
  }

  private acquire(
    modality: RuntimeModality,
    snapshot: RuntimeConfigSnapshot,
  ): RuntimeLease | undefined {
    if (!configuredRuntimeModality(modality, snapshot.config, this.ownership)) {
      return undefined;
    }
    const supervisor = this.supervisors.get(modality);
    if (!supervisor) return undefined;
    const lease = this.barriers[modality].acquire({
      snapshot,
      supervisor,
    });
    if (!lease) return undefined;
    return {
      modality,
      snapshot: lease.value.snapshot,
      supervisor: lease.value.supervisor,
      onPendingDetach: lease.onPendingDetach,
      onIdleCancellation: lease.onIdleCancellation,
      markResponseStarted: lease.markResponseStarted,
      cancel: lease.cancel,
      release: lease.release,
    };
  }

  private prepare(admission: RuntimeLease): RuntimeAdmission {
    let startupPending = true;
    let stopRequested = false;
    const stop = (allowRunning: boolean) => {
      if (stopRequested) return;
      const state = admission.supervisor.state();
      if (state === "starting" || (allowRunning && state === "running")) {
        stopRequested = true;
        void admission.supervisor.kill();
      }
    };
    admission.onPendingDetach(() => {
      if (startupPending || admission.supervisor.kind === "speech") {
        stop(admission.supervisor.kind === "speech");
      }
    });
    admission.onIdleCancellation(() => {
      if (stopRequested) return;
      stopRequested = true;
      void this.exclusiveModality(admission.modality, async () => {
        try {
          if (
            this.supervisors.get(admission.modality) === admission.supervisor &&
            ["starting", "running"].includes(admission.supervisor.state())
          ) {
            await admission.supervisor.kill();
          }
        } catch (error) {
          this.logger.event({
            severity: "error",
            eventName: "runtime.cancellation-failed",
            category: "runtime",
            component: modalityComponents[admission.modality],
            runtime: admission.modality,
            message: "Runtime cancellation failed.",
            error: {
              type: error instanceof Error ? error.name : "Error",
              message: error instanceof Error ? error.message : String(error),
            },
          });
        } finally {
          if (
            this.configured[admission.modality] &&
            this.supervisors.get(admission.modality) === admission.supervisor
          ) {
            this.attachBarrier(admission.modality);
          }
        }
      });
    });
    const ready = admission.supervisor.ensureRunning();
    void ready.then(
      () => {
        startupPending = false;
      },
      () => {
        startupPending = false;
      },
    );
    return Object.freeze({ ...admission, ready });
  }

  private resolveRequestedModel(
    modality: RuntimeModality,
    requestedModel: string | undefined,
    snapshot: RuntimeConfigSnapshot,
  ): string | undefined {
    const active = activeModel(modality, snapshot.config);
    if (requestedModel === undefined) return active;
    return [active, ...selectedModels(modality, snapshot.config)].find(
      (modelId) => modelId === requestedModel,
    );
  }

  /**
   * Verifies the target model would be admitted once the current one is
   * stopped, before anything is drained or stopped. Throws
   * ModelSwitchRejectedError (leaving the current model running) otherwise.
   */
  private async assertSwitchFits(
    modality: RuntimeModality,
    modelId: string,
    source: RuntimeConfigSnapshot,
    signal?: AbortSignal,
  ): Promise<void> {
    const current = this.supervisors.get(modality);
    const preflightController = new AbortController();
    this.switchPreflights.set(modality, preflightController);
    const preflightSignal = signal
      ? AbortSignal.any([signal, preflightController.signal])
      : preflightController.signal;
    try {
      const candidate = this.factory.create(modality, {
        ...source,
        config: { ...source.config, [activeModelField(modality)]: modelId },
      });
      if (!candidate.preflight) return;
      const rejection = await this.waitForAbort(
        candidate.preflight(
          current ? [current.runtimeId()] : [],
          preflightSignal,
        ),
        preflightSignal,
      );
      if (rejection) throw new ModelSwitchRejectedError(rejection);
    } catch (error) {
      if (error instanceof ModelSwitchRejectedError) throw error;
      if (error instanceof RuntimeRequestAbortedError) throw error;
      // Resolution failures surface through the normal startup path.
      this.logger.event({
        severity: "warn",
        eventName: "model.switch-preflight-failed",
        category: "runtime",
        component: modalityComponents[modality],
        runtime: modality,
        message: "Memory preflight for the model switch could not run.",
        error: {
          type: error instanceof Error ? error.name : "Error",
          message: error instanceof Error ? error.message : String(error),
        },
      });
    } finally {
      if (this.switchPreflights.get(modality) === preflightController)
        this.switchPreflights.delete(modality);
    }
  }

  private async activateModel(
    modality: RuntimeModality,
    modelId: string,
    source: RuntimeConfigSnapshot,
    signal?: AbortSignal,
    dispatchLease?: InferenceDispatchLease,
  ): Promise<RuntimeConfigSnapshot> {
    const field = activeModelField(modality);
    const previousModel = activeModel(modality, source.config);
    const pending = new Set([
      previousModel,
      modelId,
      activeModel(modality, this.appliedSnapshots[modality].config),
    ]);
    this.pendingModelReferences.add(pending);
    try {
      await this.assertSwitchFits(modality, modelId, source, signal);
      this.logger.event({
        severity: "info",
        eventName: "model.switching",
        category: "runtime",
        component: modalityComponents[modality],
        runtime: modality,
        message: "Switching the active model.",
        attributes: { from_model: previousModel, to_model: modelId },
      });
      this.supervisors.markDraining(modality);
      const drain = this.barriers[modality].drain();
      try {
        await this.waitForAbort(drain, signal);
        this.throwIfAborted(signal);
        dispatchLease?.throwIfCancelled();
      } catch (error) {
        if (error instanceof RuntimeRequestAbortedError) {
          await drain;
          this.supervisors.clearDraining(modality);
          this.attachBarrier(modality);
        }
        throw error;
      }
      let target: RuntimeConfigSnapshot;
      try {
        target = await this.exclusive(async () => {
          dispatchLease?.throwIfCancelled();
          const target = await this.controller.update((config) => {
            if (!selectedModels(modality, config).includes(modelId))
              throw new ModelNoLongerSelectedError();
            config[field] = modelId;
          });
          this.snapshot = target;
          this.configured = configuredModalities(target, this.ownership);
          return target;
        });
        dispatchLease?.throwIfCancelled();
      } catch (error) {
        this.supervisors.clearDraining(modality);
        this.attachBarrier(modality);
        throw error;
      }
      const previous = this.supervisors.take(modality);
      await previous?.shutdown();
      dispatchLease?.throwIfCancelled();
      await this.waitForAdmissionAfterEviction(
        modality,
        modelId,
        signal,
        source,
      );
      dispatchLease?.throwIfCancelled();
      this.supervisors.add(modality, this.factory.create(modality, target));
      this.appliedSnapshots[modality] = target;
      this.attachBarrier(modality);
      this.logger.event({
        severity: "info",
        eventName: "model.switched",
        category: "runtime",
        component: modalityComponents[modality],
        runtime: modality,
        message: "Active model switched.",
        attributes: { from_model: previousModel, to_model: modelId },
      });
      return target;
    } finally {
      this.pendingModelReferences.delete(pending);
    }
  }

  private scheduleReconciliation(
    target: RuntimeConfigSnapshot,
  ): CoordinatedSnapshot {
    if (target.revision === this.snapshot.revision) {
      for (const modality of runtimeModalities) this.scheduleModality(modality);
      return Object.freeze({
        snapshot: this.snapshot,
        transitions: Object.freeze({ ...this.modalityTransitions }),
      });
    }
    const plan = createRuntimeReconciliationPlan(
      this.snapshot,
      target,
      this.ownership,
    );
    if (plan.restartRequired.action === "restart-required") {
      this.logger.event({
        severity: "error",
        eventName: "runtime.reconciliation-failed",
        category: "runtime",
        component: "gateway",
        runtime: "gateway",
        message: "Runtime configuration change requires a gateway restart.",
        attributes: { revision: target.revision },
      });
      return Object.freeze({
        snapshot: this.snapshot,
        transitions: Object.freeze({ ...this.modalityTransitions }),
      });
    }

    this.snapshot = target;
    this.configured = configuredModalities(target, this.ownership);
    for (const modality of runtimeModalities) this.scheduleModality(modality);
    return Object.freeze({
      snapshot: target,
      transitions: Object.freeze({ ...this.modalityTransitions }),
    });
  }

  private scheduleModality(modality: RuntimeModality): void {
    if (
      this.reconciliationScheduled.has(modality) ||
      this.appliedSnapshots[modality].revision === this.snapshot.revision
    ) {
      return;
    }
    this.reconciliationScheduled.add(modality);
    void this.exclusiveModality(modality, async () => {
      try {
        while (
          this.appliedSnapshots[modality].revision !== this.snapshot.revision
        ) {
          const before = this.appliedSnapshots[modality];
          await this.reconcileModality(modality);
          if (this.appliedSnapshots[modality] === before) return;
        }
      } finally {
        this.reconciliationScheduled.delete(modality);
      }
    });
  }

  private async reconcileModality(modality: RuntimeModality): Promise<void> {
    const target = this.snapshot;
    const plan = createRuntimeReconciliationPlan(
      this.appliedSnapshots[modality],
      target,
      this.ownership,
    );
    if (plan.modalities[modality].action === "unchanged") {
      if (
        configuredRuntimeModality(modality, target.config, this.ownership) &&
        !this.supervisors.get(modality)
      ) {
        try {
          this.supervisors.add(modality, this.factory.create(modality, target));
          this.attachBarrier(modality);
        } catch (error) {
          this.recordReconciliationFailure(modality, target, "add", error);
          return;
        }
      }
      this.appliedSnapshots[modality] = target;
      return;
    }
    await this.applyModality(modality, plan, target);
  }

  private async applyModality(
    modality: RuntimeModality,
    plan: ReturnType<typeof createRuntimeReconciliationPlan>,
    target: RuntimeConfigSnapshot,
  ): Promise<void> {
    const action = plan.modalities[modality];
    if (action.action === "unchanged") return;
    const pending = new Set([
      activeModel(modality, this.appliedSnapshots[modality].config),
      activeModel(modality, target.config),
    ]);
    this.pendingModelReferences.add(pending);
    try {
      if (action.action === "add") {
        this.supervisors.add(modality, this.factory.create(modality, target));
        this.attachBarrier(modality);
        this.appliedSnapshots[modality] = target;
        return;
      }

      if (
        action.action === "drain-and-replace" &&
        this.supervisors.get(modality) !== undefined
      ) {
        try {
          await this.assertSwitchFits(
            modality,
            activeModel(modality, target.config),
            target,
          );
        } catch (error) {
          if (error instanceof ModelSwitchRejectedError) {
            this.logger.event({
              severity: "error",
              eventName: "model.switch-preflight-rejected",
              category: "runtime",
              component: modalityComponents[modality],
              runtime: modality,
              message:
                "Configuration model switch was rejected before shutdown.",
              error: {
                type: error.cause.name,
                message: error.cause.message,
              },
              attributes: {
                from_model: activeModel(
                  modality,
                  this.appliedSnapshots[modality].config,
                ),
                to_model: activeModel(modality, target.config),
              },
            });
            return;
          }
          throw error;
        }
      }

      await this.hooks.beforeModalityDrain?.(modality);

      if (action.action === "drain-and-remove")
        this.queues[modality].rejectPending(
          new InferenceQueueUnavailableError(
            `${modality} runtime is disabled.`,
          ),
        );

      this.supervisors.markDraining(modality);
      await this.barriers[modality].drain();
      const previous = this.supervisors.take(modality);
      await previous?.shutdown();

      if (action.action === "drain-and-replace") {
        this.supervisors.add(modality, this.factory.create(modality, target));
        this.attachBarrier(modality);
      }
      this.appliedSnapshots[modality] = target;
    } catch (error) {
      this.recordReconciliationFailure(modality, target, action.action, error);
    } finally {
      this.pendingModelReferences.delete(pending);
    }
  }

  private recordReconciliationFailure(
    modality: RuntimeModality,
    target: RuntimeConfigSnapshot,
    action: string,
    error: unknown,
  ): void {
    this.supervisors.markFailed(modality);
    this.logger.event({
      severity: "error",
      eventName: "runtime.reconciliation-failed",
      category: "runtime",
      component: modalityComponents[modality],
      runtime: modality,
      message: "Runtime reconciliation failed.",
      error: {
        type: error instanceof Error ? error.name : "Error",
        message: error instanceof Error ? error.message : String(error),
      },
      attributes: { revision: target.revision, action },
    });
  }

  private throwIfAborted(signal: AbortSignal | undefined): void {
    if (signal?.aborted) throw new RuntimeRequestAbortedError();
  }

  private async waitForAbort<Value>(
    work: Promise<Value>,
    signal: AbortSignal | undefined,
  ): Promise<Value> {
    this.throwIfAborted(signal);
    if (!signal) return await work;
    return await new Promise<Value>((resolve, reject) => {
      const abort = () => reject(new RuntimeRequestAbortedError());
      signal.addEventListener("abort", abort, { once: true });
      work.then(
        (value) => {
          signal.removeEventListener("abort", abort);
          resolve(value);
        },
        (error) => {
          signal.removeEventListener("abort", abort);
          reject(error);
        },
      );
    });
  }
}
