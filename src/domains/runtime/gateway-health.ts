import { LOCALBASE_VERSION } from "../../version";
import {
  gatewayHealthSchema,
  type GatewayHealth,
  type ModalityLifecycleState,
} from "./health";
import type { RuntimeModality } from "./modality";
import type { SupervisorStateReader } from "./supervisor-registry";

export type GatewayModalityConfiguration = Readonly<
  Record<RuntimeModality, boolean>
>;

export type GatewayHealthInput = Readonly<{
  startedAtMs: number;
  nowMs: number;
  stopping: boolean;
  configured: GatewayModalityConfiguration;
  supervisors: SupervisorStateReader;
}>;

/** Composes the externally exposed gateway health payload from supervisor state. */
export function composeGatewayHealth(input: GatewayHealthInput): GatewayHealth {
  const modality = (
    name: RuntimeModality,
  ): {
    configured: boolean;
    state: ModalityLifecycleState;
  } => input.supervisors.state(name, input.configured[name]);

  return gatewayHealthSchema.parse({
    status: input.stopping ? "error" : "ok",
    version: LOCALBASE_VERSION,
    uptimeSeconds: Math.max(
      0,
      Math.floor((input.nowMs - input.startedAtMs) / 1_000),
    ),
    modalities: {
      llm: modality("llm"),
      stt: modality("stt"),
      image: modality("image"),
    },
    ...(input.stopping ? { error: "gateway_stopping" } : {}),
  });
}
