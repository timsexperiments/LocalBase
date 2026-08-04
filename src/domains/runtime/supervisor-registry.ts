import type { ModalityLifecycleState } from "./health";
import { runtimeModalities, type RuntimeModality } from "./modality";

export type RuntimeSupervisor = {
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
  }

  take(modality: RuntimeModality): RuntimeSupervisor | undefined {
    const service = this.get(modality);
    if (!service) return undefined;
    delete this.services[modality];
    return service;
  }

  state(
    modality: RuntimeModality,
    configured: boolean,
  ): ModalitySupervisorState {
    const service = this.get(modality);
    return {
      configured,
      state: configured && service ? service.state() : "disabled",
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
