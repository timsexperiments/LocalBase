import type { ILogger } from "../observability/logging";
import type { LocalBaseConfig } from "../../manager";
import {
  type RuntimeConfigController,
  type RuntimeConfigSnapshot,
} from "./config-snapshot";
import { ModalityAdmissionBarrier } from "./modality-admission";
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
  }

  read(): RuntimeConfigSnapshot {
    return this.snapshot;
  }

  configuredModalities(): Readonly<ConfiguredModalities> {
    return Object.freeze({ ...this.configured });
  }

  async refresh(): Promise<RuntimeConfigSnapshot> {
    if (!this.sharedRefresh) {
      const refresh = this.exclusive(async () => {
        await this.reconcile(await this.controller.refresh());
        return this.snapshot;
      });
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
    await this.exclusive(async () => {
      const evictions = runtimeModalities.flatMap((modality) => {
        const supervisor = this.supervisors.get(modality);
        if (!supervisor || supervisor.state() !== "running") return [];
        const barrier = this.barriers[modality];
        if (!barrier.detachIfIdle()) return [];
        return [{ barrier, supervisor }];
      });

      await Promise.all(
        evictions.map(async ({ barrier, supervisor }) => {
          try {
            await supervisor.kill();
          } finally {
            barrier.attach();
          }
        }),
      );
    });
  }

  async evictAllRuntimes(): Promise<void> {
    await this.exclusive(async () => {
      const runtimes = runtimeModalities.flatMap((modality) => {
        const supervisor = this.supervisors.get(modality);
        if (!supervisor) return [];
        this.supervisors.markDraining(modality);
        return [
          {
            barrier: this.barriers[modality],
            modality,
            supervisor,
            drain: this.barriers[modality].drainWithoutCancellation(),
          },
        ];
      });

      const kills = await Promise.allSettled(
        runtimes.map(async ({ supervisor }) => await supervisor.kill()),
      );
      await Promise.all(runtimes.map(async ({ drain }) => await drain));

      for (const { barrier, modality } of runtimes) {
        this.supervisors.clearDraining(modality);
        if (this.configured[modality]) barrier.attach();
      }

      const failedKill = kills.find(
        (result): result is PromiseRejectedResult =>
          result.status === "rejected",
      );
      if (failedKill) throw failedKill.reason;
    });
  }

  async admit(
    modality: RuntimeModality,
  ): Promise<RuntimeAdmission | undefined> {
    return await this.exclusive(async () => {
      await this.reconcile(await this.controller.refresh());
      const admission = this.acquire(modality);
      return admission ? this.prepare(admission) : undefined;
    });
  }

  async admitModel(
    modality: RuntimeModality,
    requestedModel: string | undefined,
    signal?: AbortSignal,
  ): Promise<ModelAdmissionResult> {
    const admitted = this.exclusive<ModelAdmissionResult>(async () => {
      this.throwIfAborted(signal);
      await this.reconcile(await this.controller.refresh());
      this.throwIfAborted(signal);
      if (!this.configured[modality]) return { kind: "not-configured" };
      const modelId = this.resolveRequestedModel(modality, requestedModel);
      if (!modelId) return { kind: "model-not-found" };
      if (
        modelId !== activeModel(modality, this.snapshot.config) &&
        !this.ownedFields.has(activeModelField(modality))
      ) {
        await this.activateModel(modality, modelId, signal);
      }
      this.throwIfAborted(signal);
      const admission = this.acquire(modality);
      if (!admission) return { kind: "unavailable" };
      return {
        kind: "admitted",
        value: { modelId, admission: this.prepare(admission) },
      };
    });
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

  private async exclusive<Value>(work: () => Promise<Value>): Promise<Value> {
    const next = this.transitions.then(work, work);
    this.transitions = next.then(
      () => undefined,
      () => undefined,
    );
    return await next;
  }

  private acquire(modality: RuntimeModality): RuntimeLease | undefined {
    if (!this.configured[modality]) return undefined;
    const supervisor = this.supervisors.get(modality);
    if (!supervisor) return undefined;
    const lease = this.barriers[modality].acquire({
      snapshot: this.snapshot,
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
      stop(true);
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
  ): string | undefined {
    const active = activeModel(modality, this.snapshot.config);
    if (requestedModel === undefined) return active;
    return [active, ...selectedModels(modality, this.snapshot.config)].find(
      (modelId) => modelId === requestedModel,
    );
  }

  private async activateModel(
    modality: RuntimeModality,
    modelId: string,
    signal?: AbortSignal,
  ): Promise<void> {
    const field = activeModelField(modality);
    const previousModel = activeModel(modality, this.snapshot.config);
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
    this.controller.update((config) => {
      config[field] = modelId;
    });
    await this.reconcile(this.controller.read());
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
  }

  private async reconcile(target: RuntimeConfigSnapshot): Promise<void> {
    if (target.revision === this.snapshot.revision) return;
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
      return;
    }

    for (const modality of runtimeModalities) {
      await this.applyModality(modality, plan, target);
    }
    this.snapshot = target;
  }

  private async applyModality(
    modality: RuntimeModality,
    plan: ReturnType<typeof createRuntimeReconciliationPlan>,
    target: RuntimeConfigSnapshot,
  ): Promise<void> {
    const action = plan.modalities[modality];
    this.configured[modality] = action.targetConfigured;
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
