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
 * Execution slots are available only after the runtime resolves its own launch
 * plan. Queue facts report the separate request-admission owner.
 */
export type RuntimeLifecycleSnapshot = Readonly<{
  modality: RuntimeModality;
  configured: boolean;
  state: ModalityLifecycleState;
  modelId: string | null;
  runtimeId: string | null;
  admission: RuntimeAdmissionSnapshot;
  execution: Readonly<{
    slots: number | null;
    activeAdmissions: number | null;
    available: number | null;
    immediateDispatchAvailable: boolean | null;
  }>;
  queue: InferenceQueueSnapshot | null;
}>;

function immediateDispatchAvailability(
  input: Readonly<{
    state: ModalityLifecycleState;
    admission: RuntimeAdmissionSnapshot;
    queue?: InferenceQueueSnapshot | null;
  }>,
): boolean | null {
  if (!input.queue) return null;
  if (
    !input.queue.accepting ||
    input.state === "draining" ||
    (input.admission.kind === "known" && !input.admission.accepting)
  ) {
    return false;
  }
  if (input.admission.kind === "unknown") return null;
  return input.queue.immediateDispatchAvailable;
}

export function createRuntimeLifecycleSnapshot(
  input: Omit<RuntimeLifecycleSnapshot, "execution" | "queue"> & {
    configuredSlots: number | null;
    queue?: InferenceQueueSnapshot | null;
  },
): RuntimeLifecycleSnapshot {
  const immediateDispatchAvailable = immediateDispatchAvailability(input);
  const queue = input.queue
    ? Object.freeze({
        ...input.queue,
        immediateDispatchAvailable,
      })
    : null;
  const activeAdmissions = queue?.active ?? null;
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
    execution: Object.freeze({
      slots: input.configuredSlots,
      activeAdmissions,
      available:
        input.configuredSlots === null || activeAdmissions === null
          ? null
          : Math.max(0, input.configuredSlots - activeAdmissions),
      immediateDispatchAvailable,
    }),
    queue,
  });
}
