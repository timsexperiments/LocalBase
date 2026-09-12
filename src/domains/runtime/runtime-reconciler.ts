import type { ILogger } from "../observability/logging";
import type { LocalBaseConfig } from "../../manager";
import {
  type RuntimeConfigController,
  type RuntimeConfigSnapshot,
} from "./config-snapshot";
import { ModalityAdmissionBarrier } from "./modality-admission";
import type { RuntimeLifecycleSnapshot } from "./lifecycle-snapshot";
import {
  InferenceQueue,
  InferenceQueueAbortedError,
  InferenceQueueUnavailableError,
  type InferenceDispatchLease,
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

type ModelAdmission = Readonly<{
  modelId: string;
  queueWaitMs?: number;
  admission: RuntimeAdmission;
}>;

export type ModelAdmissionResult =
  | Readonly<{ kind: "admitted"; value: ModelAdmission }>
  | Readonly<{ kind: "not-configured" }>
  | Readonly<{ kind: "model-not-found" }>
  | Readonly<{ kind: "unavailable" }>;

type ConfiguredModalities = Record<RuntimeModality, boolean>;
type ModalityTransitions = Record<RuntimeModality, Promise<void>>;

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
): "activeLlmModel" | "activeSttModel" | "activeTtsModel" | "activeImageModel" {
  if (modality === "llm") return "activeLlmModel";
  if (modality === "stt") return "activeSttModel";
  if (modality === "tts") return "activeTtsModel";
  return "activeImageModel";
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
  return config.selectedImageModels;
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
  private transitions = Promise.resolve();
  private readonly modalityTransitions: ModalityTransitions =
    Object.fromEntries(
      runtimeModalities.map((modality) => [modality, Promise.resolve()]),
    ) as Record<RuntimeModality, Promise<void>>;
  private sharedRefresh: Promise<RuntimeConfigSnapshot> | undefined;
  private readonly queues: Record<
    RuntimeModality,
    InferenceQueue<ModelAdmissionResult>
  >;

  constructor(
    private readonly controller: RuntimeConfigController,
    private readonly ownership: RuntimeOverrideOwnership,
    private readonly supervisors: SupervisorRegistry,
    private readonly factory: RuntimeSupervisorFactory,
    private readonly logger: ILogger,
    queueOptions: Readonly<{ maxWaiting?: number; waitMs?: number }> = {},
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
    ) as Record<RuntimeModality, InferenceQueue<ModelAdmissionResult>>;
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

  lifecycleSnapshot(): Readonly<
    Record<RuntimeModality, RuntimeLifecycleSnapshot>
  > {
    return Object.freeze(
      Object.fromEntries(
        runtimeModalities.map((modality) => [
          modality,
          (() => {
            const applied = this.appliedSnapshots[modality];
            return this.supervisors.lifecycleSnapshot({
              modality,
              configured: configuredRuntimeModality(
                modality,
                applied.config,
                this.ownership,
              ),
              modelId: this.supervisors.get(modality)
                ? activeModel(modality, applied.config) || null
                : null,
              admission: this.barriers[modality].snapshot(),
              queue: this.queues[modality].snapshot(),
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

  async evictIdleRuntimes(): Promise<void> {
    await Promise.all(
      runtimeModalities.map((modality) =>
        this.exclusiveModality(modality, async () => {
          const supervisor = this.supervisors.get(modality);
          if (!supervisor || supervisor.state() !== "running") return;
          const barrier = this.barriers[modality];
          if (!barrier.detachIfIdle()) return;
          try {
            await supervisor.kill();
          } finally {
            barrier.attach();
          }
        }),
      ),
    );
  }

  async evictAllRuntimes(): Promise<void> {
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
          if (this.configured[modality]) barrier.attach();
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
  ): Promise<ModelAdmissionResult> {
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
        admissionSnapshot = await this.activateModel(
          modality,
          modelId,
          desiredSnapshot,
          signal,
          dispatchLease,
        );
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
  ): Promise<Value> {
    const previous = this.modalityTransitions[modality];
    const next = previous.then(work, work);
    this.modalityTransitions[modality] = next.then(
      () => undefined,
      () => undefined,
    );
    return await next;
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
            this.barriers[admission.modality].attach();
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

  private async activateModel(
    modality: RuntimeModality,
    modelId: string,
    source: RuntimeConfigSnapshot,
    signal?: AbortSignal,
    dispatchLease?: InferenceDispatchLease,
  ): Promise<RuntimeConfigSnapshot> {
    const field = activeModelField(modality);
    const previousModel = activeModel(modality, source.config);
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
        this.barriers[modality].attach();
      }
      throw error;
    }
    const target = await this.exclusive(async () => {
      dispatchLease?.throwIfCancelled();
      const target = this.controller.update((config) => {
        config[field] = modelId;
      });
      this.snapshot = target;
      this.configured = configuredModalities(target, this.ownership);
      return target;
    });
    dispatchLease?.throwIfCancelled();
    const previous = this.supervisors.take(modality);
    await previous?.shutdown();
    dispatchLease?.throwIfCancelled();
    this.supervisors.add(modality, this.factory.create(modality, target));
    this.appliedSnapshots[modality] = target;
    this.barriers[modality].attach();
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
          this.barriers[modality].attach();
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

    try {
      if (action.action === "add") {
        this.supervisors.add(modality, this.factory.create(modality, target));
        this.barriers[modality].attach();
        this.appliedSnapshots[modality] = target;
        return;
      }

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
        this.barriers[modality].attach();
      }
      this.appliedSnapshots[modality] = target;
    } catch (error) {
      this.recordReconciliationFailure(modality, target, action.action, error);
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
