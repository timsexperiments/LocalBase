import { z } from "zod";
import type { RuntimeLifecycleSnapshot } from "./lifecycle-snapshot";
import { runtimeModalities, type RuntimeModality } from "./modality";

const readinessModalitySchema = z.enum(runtimeModalities);

const readinessBaseSchema = z
  .object({
    modalities: z.array(readinessModalitySchema),
  })
  .strict();

export const gatewayReadinessSchema = z.discriminatedUnion("status", [
  readinessBaseSchema
    .extend({
      status: z.literal("ready"),
      reason: z.literal("request_admission_available"),
      modalities: z.array(readinessModalitySchema).min(1),
    })
    .strict(),
  readinessBaseSchema
    .extend({
      status: z.literal("unready"),
      reason: z.enum(["gateway_stopping", "no_request_admission"]),
      modalities: z.array(readinessModalitySchema).length(0),
    })
    .strict(),
]);
export type GatewayReadiness = z.infer<typeof gatewayReadinessSchema>;

export type GatewayReadinessInput = Readonly<{
  stopping: boolean;
  lifecycles: Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>>;
}>;

function canAdmitRequest(snapshot: RuntimeLifecycleSnapshot): boolean {
  if (!snapshot.configured) return false;
  if (
    snapshot.state !== "idle" &&
    snapshot.state !== "starting" &&
    snapshot.state !== "running"
  ) {
    return false;
  }
  if (snapshot.admission.kind !== "known" || !snapshot.admission.accepting)
    return false;
  const queue = snapshot.queue;
  return queue !== null && queue.accepting && queue.waiting < queue.capacity;
}

/**
 * Reports whether a request can enter a configured runtime without causing
 * configuration refresh, reconciliation, or runtime activity.
 */
export function composeGatewayReadiness(
  input: GatewayReadinessInput,
): GatewayReadiness {
  if (input.stopping) {
    return gatewayReadinessSchema.parse({
      status: "unready",
      reason: "gateway_stopping",
      modalities: [],
    });
  }
  const modalities = runtimeModalities.filter((modality) =>
    canAdmitRequest(input.lifecycles[modality]),
  );
  if (modalities.length === 0) {
    return gatewayReadinessSchema.parse({
      status: "unready",
      reason: "no_request_admission",
      modalities,
    });
  }
  return gatewayReadinessSchema.parse({
    status: "ready",
    reason: "request_admission_available",
    modalities,
  });
}
