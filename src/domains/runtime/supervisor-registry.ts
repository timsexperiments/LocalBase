import type { ModalityLifecycleState } from "./health";
import {
  createRuntimeLifecycleSnapshot,
  type RuntimeAdmissionSnapshot,
  type RuntimeLifecycleSnapshot,
} from "./lifecycle-snapshot";
import { runtimeModalities, type RuntimeModality } from "./modality";

export type RuntimeSupervisor = {
  runtimeId(): string;
  state(): ModalityLifecycleState;
  resolvedSlots?(): number | undefined;
  ensureRunning(): Promise<void>;
  kill(): Promise<void>;
  shutdown(): Promise<void>;
};

export type ModalitySupervisorState = {
  configured: boolean;
  state: ModalityLifecycleState;
};

export type SupervisorStateReader = {
  state(
    modality: RuntimeModality,
    configured: boolean,
  ): ModalitySupervisorState;
};

export type SupervisorLifecycleReader = {
  lifecycleSnapshot(
    input: Readonly<{
      modality: RuntimeModality;
      configured: boolean;
      modelId: string | null;
    }>,
  ): RuntimeLifecycleSnapshot;
};

/** Owns configured modality supervisors for one gateway instance. */
export class SupervisorRegistry implements SupervisorStateReader {
  private readonly services: Partial<
    Record<RuntimeModality, RuntimeSupervisor>
  >;
  private readonly draining = new Set<RuntimeModality>();
  private readonly failed = new Set<RuntimeModality>();
  private admissionReader:
    ((modality: RuntimeModality) => RuntimeAdmissionSnapshot) | undefined;

  constructor(services: Partial<Record<RuntimeModality, RuntimeSupervisor>>) {
    this.services = { ...services };
  }

  get(modality: RuntimeModality): RuntimeSupervisor | undefined {
    return this.services[modality];
  }

  add(modality: RuntimeModality, service: RuntimeSupervisor): void {
    if (this.get(modality)) {
      throw new Error(`${modality} supervisor is already configured.`);
    }
    this.services[modality] = service;
    this.draining.delete(modality);
    this.failed.delete(modality);
  }

  take(modality: RuntimeModality): RuntimeSupervisor | undefined {
    const service = this.get(modality);
    if (!service) return undefined;
    delete this.services[modality];
    this.draining.delete(modality);
    return service;
  }

  markDraining(modality: RuntimeModality): void {
    this.draining.add(modality);
    this.failed.delete(modality);
  }

  markFailed(modality: RuntimeModality): void {
    this.draining.delete(modality);
    this.failed.add(modality);
  }

  clearDraining(modality: RuntimeModality): void {
    this.draining.delete(modality);
  }

  clearFailure(modality: RuntimeModality): void {
    this.failed.delete(modality);
  }

  setAdmissionReader(
    reader: (modality: RuntimeModality) => RuntimeAdmissionSnapshot,
  ): void {
    this.admissionReader = reader;
  }

  state(
    modality: RuntimeModality,
    configured: boolean,
  ): ModalitySupervisorState {
    const snapshot = this.lifecycleSnapshot({
      modality,
      configured,
      modelId: null,
    });
    return { configured: snapshot.configured, state: snapshot.state };
  }

  lifecycleSnapshot(
    input: Readonly<{
      modality: RuntimeModality;
      configured: boolean;
      modelId: string | null;
      admission?: RuntimeAdmissionSnapshot;
    }>,
  ): RuntimeLifecycleSnapshot {
    const service = this.get(input.modality);
    const state = this.draining.has(input.modality)
      ? "draining"
      : this.failed.has(input.modality)
        ? "failed"
        : input.configured && service
          ? service.state()
          : "disabled";
    return createRuntimeLifecycleSnapshot({
      modality: input.modality,
      configured: input.configured,
      state,
      modelId: input.modelId,
      runtimeId: service?.runtimeId() ?? null,
      admission: input.admission ??
        this.admissionReader?.(input.modality) ?? { kind: "unknown" },
      configuredSlots: service?.resolvedSlots?.() ?? null,
    });
  }

  async shutdown(): Promise<void> {
    await Promise.all(
      runtimeModalities.map(
        async (modality) => await this.get(modality)?.shutdown(),
      ),
    );
  }
}
