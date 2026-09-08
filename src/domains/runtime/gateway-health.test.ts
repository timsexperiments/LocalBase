import { expect, test } from "bun:test";
import { LOCALBASE_VERSION } from "../../version";
import { composeGatewayHealth } from "./gateway-health";
import { createRuntimeLifecycleSnapshot } from "./lifecycle-snapshot";
import type { SupervisorLifecycleReader } from "./supervisor-registry";

const supervisors: SupervisorLifecycleReader = {
  lifecycleSnapshot({ modality, configured }) {
    return createRuntimeLifecycleSnapshot({
      modality,
      configured,
      state: modality === "llm" && configured ? "running" : "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    });
  },
};

test("composes health from configured modalities and supervisor state", () => {
  expect(
    composeGatewayHealth({
      startedAtMs: 10_000,
      nowMs: 12_900,
      stopping: false,
      configured: { llm: true, stt: false, image: false },
      supervisors,
    }),
  ).toEqual({
    status: "ok",
    version: LOCALBASE_VERSION,
    uptimeSeconds: 2,
    modalities: {
      llm: { configured: true, state: "running" },
      stt: { configured: false, state: "disabled" },
      image: { configured: false, state: "disabled" },
    },
  });
});

test("reports stopping without a negative uptime", () => {
  expect(
    composeGatewayHealth({
      startedAtMs: 10_000,
      nowMs: 9_000,
      stopping: true,
      configured: { llm: true, stt: true, image: true },
      supervisors,
    }),
  ).toMatchObject({
    status: "error",
    error: "gateway_stopping",
    uptimeSeconds: 0,
  });
});
