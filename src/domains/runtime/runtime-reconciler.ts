import type { ILogger } from "../observability/logging";
import type { LocalBaseConfig } from "../../manager";
import {
  type RuntimeConfigController,
  type RuntimeConfigSnapshot,
} from "./config-snapshot";
import { ModalityAdmissionBarrier } from "./modality-admission";
import type { RuntimeLifecycleSnapshot } from "./lifecycle-snapshot";
import { runtimeModalities, type RuntimeModality } from "./modality";
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
): "activeLlmModel" | "activeSttModel" | "activeImageModel" {
  if (modality === "llm") return "activeLlmModel";
  if (modality === "stt") return "activeSttModel";
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
  return config.selectedImageModels;
}

/** Applies persisted runtime changes while preserving the gateway listener. */
export class RuntimeReconciler {
  private readonly barriers: Record<RuntimeModality, ModalityAdmissionBarrier>;
  private readonly ownedFields: ReadonlySet<keyof LocalBaseConfig>;
  private configured: ConfiguredModalities;
  private snapshot: RuntimeConfigSnapshot;
  private transitions = Promise.resolve();
  private readonly modalityTransitions: ModalityTransitions =
    Object.fromEntries(
      runtimeModalities.map((modality) => [modality, Promise.resolve()]),
    ) as Record<RuntimeModality, Promise<void>>;
  private sharedRefresh: Promise<RuntimeConfigSnapshot> | undefined;

  constructor(
    private readonly controller: RuntimeConfigController,
    private readonly ownership: RuntimeOverrideOwnership,
    private readonly supervisors: SupervisorRegistry,
    private readonly factory: RuntimeSupervisorFactory,
    private readonly logger: ILogger,
  ) {
    this.snapshot = controller.read();
    this.configured = configuredModalities(this.snapshot, ownership);
    this.ownedFields = new Set(ownership.configFields ?? []);
    this.barriers = Object.fromEntries(
      runtimeModalities.map((modality) => [
        modality,
        new ModalityAdmissionBarrier(modality, this.configured[modality]),
      ]),
    ) as Record<RuntimeModality, ModalityAdmissionBarrier>;
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
          this.supervisors.lifecycleSnapshot({
            modality,
            configured: this.configured[modality],
            modelId: activeModel(modality, this.snapshot.config) || null,
            admission: this.barriers[modality].snapshot(),
          }),
        ]),
      ) as Record<RuntimeModality, RuntimeLifecycleSnapshot>,
    );
  }

  async refresh(): Promise<RuntimeConfigSnapshot> {
    if (!this.sharedRefresh) {
      const refresh = this.coordinateRefresh();
      this.sharedRefresh = refresh;
      void refresh
        .finally(() => {
          if (this.sharedRefresh === refresh) this.sharedRefresh = undefined;
        })
        .catch(() => {});
    }
    return await this.sharedRefresh;
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

  async admit(
    modality: RuntimeModality,
  ): Promise<RuntimeAdmission | undefined> {
    const coordinated = await this.coordinate();
    return await this.exclusiveModality(modality, async () => {
      await coordinated.transitions[modality];
      const admission = this.acquire(modality, coordinated.snapshot);
      return admission ? this.prepare(admission) : undefined;
    });
  }

  async admitModel(
    modality: RuntimeModality,
    requestedModel: string | undefined,
    signal?: AbortSignal,
  ): Promise<ModelAdmissionResult> {
    const admitted = this.coordinateAdmission(modality, requestedModel, signal);
    try {
      return await this.waitForAbort(admitted, signal);
    } catch (error) {
      if (error instanceof RuntimeRequestAbortedError) {
        void admitted.then(
          (result) => {
            if (result.kind === "admitted") result.value.admission.cancel();
          },
          () => {},
        );
      }
      throw error;
    }
  }

  private async coordinateAdmission(
    modality: RuntimeModality,
    requestedModel: string | undefined,
    signal: AbortSignal | undefined,
  ): Promise<ModelAdmissionResult> {
    const coordinated = await this.coordinate();
    return await this.exclusiveModality(modality, async () => {
      await coordinated.transitions[modality];
      this.throwIfAborted(signal);
      if (
        !configuredRuntimeModality(
          modality,
          coordinated.snapshot.config,
          this.ownership,
        )
      ) {
        return { kind: "not-configured" };
      }
      const modelId = this.resolveRequestedModel(
        modality,
        requestedModel,
        coordinated.snapshot,
      );
      if (!modelId) return { kind: "model-not-found" };
      let admissionSnapshot = coordinated.snapshot;
      if (
        modelId !== activeModel(modality, coordinated.snapshot.config) &&
        !this.ownedFields.has(activeModelField(modality))
      ) {
        admissionSnapshot = await this.activateModel(
          modality,
          modelId,
          coordinated.snapshot,
          signal,
        );
      }
      this.throwIfAborted(signal);
      const admission = this.acquire(modality, admissionSnapshot);
      if (!admission) return { kind: "unavailable" };
      return {
        kind: "admitted",
        value: { modelId, admission: this.prepare(admission) },
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
      if (startupPending) stop(false);
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
            component:
              admission.modality === "llm"
                ? "llama-server"
                : admission.modality === "stt"
                  ? "whisper-server"
                  : "sd-server",
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
  ): Promise<RuntimeConfigSnapshot> {
    const field = activeModelField(modality);
    const previousModel = activeModel(modality, source.config);
    this.logger.event({
      severity: "info",
      eventName: "model.switching",
      category: "runtime",
      component:
        modality === "llm"
          ? "llama-server"
          : modality === "stt"
            ? "whisper-server"
            : "sd-server",
      runtime: modality,
      message: "Switching the active model.",
      attributes: { from_model: previousModel, to_model: modelId },
    });
    this.supervisors.markDraining(modality);
    const drain = this.barriers[modality].drain();
    try {
      await this.waitForAbort(drain, signal);
      this.throwIfAborted(signal);
    } catch (error) {
      if (error instanceof RuntimeRequestAbortedError) {
        await drain;
        this.supervisors.clearDraining(modality);
        this.barriers[modality].attach();
      }
      throw error;
    }
    const target = await this.exclusive(async () => {
      const target = this.controller.update((config) => {
        config[field] = modelId;
      });
      this.snapshot = target;
      this.configured = configuredModalities(target, this.ownership);
      return target;
    });
    const previous = this.supervisors.take(modality);
    await previous?.shutdown();
    this.supervisors.add(modality, this.factory.create(modality, target));
    this.barriers[modality].attach();
    this.logger.event({
      severity: "info",
      eventName: "model.switched",
      category: "runtime",
      component:
        modality === "llm"
          ? "llama-server"
          : modality === "stt"
            ? "whisper-server"
            : "sd-server",
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
    for (const modality of runtimeModalities) {
      if (plan.modalities[modality].action === "unchanged") continue;
      void this.exclusiveModality(modality, async () => {
        await this.applyModality(modality, plan, target);
      });
    }
    return Object.freeze({
      snapshot: target,
      transitions: Object.freeze({ ...this.modalityTransitions }),
    });
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
        return;
      }

      this.supervisors.markDraining(modality);
      await this.barriers[modality].drain();
      const previous = this.supervisors.take(modality);
      await previous?.shutdown();

      if (action.action === "drain-and-replace") {
        this.supervisors.add(modality, this.factory.create(modality, target));
        this.barriers[modality].attach();
      }
    } catch (error) {
      this.supervisors.markFailed(modality);
      this.logger.event({
        severity: "error",
        eventName: "runtime.reconciliation-failed",
        category: "runtime",
        component:
          modality === "llm"
            ? "llama-server"
            : modality === "stt"
              ? "whisper-server"
              : "sd-server",
        runtime: modality,
        message: "Runtime reconciliation failed.",
        error: {
          type: error instanceof Error ? error.name : "Error",
          message: error instanceof Error ? error.message : String(error),
        },
        attributes: { revision: target.revision, action: action.action },
      });
    }
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
