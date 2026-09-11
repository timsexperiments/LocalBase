import type { ModalityLifecycleState } from "./health";
import type { RuntimeModality } from "./modality";
import type { InferenceQueueSnapshot } from "./inference-queue";

export type RuntimeAdmissionSnapshot =
  | Readonly<{
      kind: "known";
      accepting: boolean;
      activeCount: number;
    }>
  | Readonly<{ kind: "unknown" }>;

/**
 * Read-only lifecycle facts for one configured runtime.
 *
 * `configuredSlots` is available only after the runtime has resolved its own
 * launch plan. Queue facts report the separate request-admission owner.
 */
export type RuntimeLifecycleSnapshot = Readonly<{
  modality: RuntimeModality;
  configured: boolean;
  state: ModalityLifecycleState;
  modelId: string | null;
  runtimeId: string | null;
  admission: RuntimeAdmissionSnapshot;
  configuredSlots: number | null;
  queue: InferenceQueueSnapshot;
}>;

export function createRuntimeLifecycleSnapshot(
  input: Omit<RuntimeLifecycleSnapshot, "queue"> & {
    queue?: InferenceQueueSnapshot;
  },
): RuntimeLifecycleSnapshot {
  return Object.freeze({
    modality: input.modality,
    configured: input.configured,
    state: input.state,
    modelId: input.modelId,
    runtimeId: input.runtimeId,
    admission:
      input.admission.kind === "known"
        ? Object.freeze({ ...input.admission })
        : Object.freeze({ kind: "unknown" }),
    configuredSlots: input.configuredSlots,
    queue: Object.freeze({
      ...(input.queue ?? {
        waiting: 0,
        active: 0,
        capacity: 16,
        maxWaitMs: 60_000,
      }),
    }),
  });
}
