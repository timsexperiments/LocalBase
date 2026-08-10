import type { ModalityLifecycleState } from "./health";
import { runtimeModalities, type RuntimeModality } from "./modality";

export type RuntimeSupervisor = {
  runtimeId(): string;
  state(): ModalityLifecycleState;
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

/** Owns configured modality supervisors for one gateway instance. */
export class SupervisorRegistry implements SupervisorStateReader {
  private readonly services: Partial<
    Record<RuntimeModality, RuntimeSupervisor>
  >;
  private readonly draining = new Set<RuntimeModality>();
  private readonly failed = new Set<RuntimeModality>();

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

  state(
    modality: RuntimeModality,
    configured: boolean,
  ): ModalitySupervisorState {
    const service = this.get(modality);
    return {
      configured,
      state: this.draining.has(modality)
        ? "draining"
        : this.failed.has(modality)
          ? "failed"
          : configured && service
            ? service.state()
            : "disabled",
    };
  }

  async shutdown(): Promise<void> {
    await Promise.all(
      runtimeModalities.map(
        async (modality) => await this.get(modality)?.shutdown(),
      ),
    );
  }
}
