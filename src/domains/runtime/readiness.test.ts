import { expect, test } from "bun:test";
import type { InferenceQueueSnapshot } from "./inference-queue";
import {
  createRuntimeLifecycleSnapshot,
  type RuntimeAdmissionSnapshot,
  type RuntimeLifecycleSnapshot,
} from "./lifecycle-snapshot";
import type { RuntimeModality } from "./modality";
import { composeGatewayReadiness, gatewayReadinessSchema } from "./readiness";

function lifecycle(
  modality: RuntimeModality,
  input: Readonly<{
    configured?: boolean;
    state?: RuntimeLifecycleSnapshot["state"];
    admission?: RuntimeAdmissionSnapshot;
    queue?: InferenceQueueSnapshot | null;
  }> = {},
): RuntimeLifecycleSnapshot {
  return createRuntimeLifecycleSnapshot({
    modality,
    configured: input.configured ?? false,
    state: input.state ?? "disabled",
    modelId: null,
    runtimeId: null,
    admission: input.admission ?? { kind: "unknown" },
    configuredSlots: null,
    queue: input.queue ?? null,
  });
}

function snapshots(
  llm: RuntimeLifecycleSnapshot,
  stt = lifecycle("stt"),
  image = lifecycle("image"),
): Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>> {
  return { llm, stt, image };
}

function openQueue(
  input: Readonly<Partial<InferenceQueueSnapshot>> = {},
): InferenceQueueSnapshot {
  return {
    waiting: input.waiting ?? 0,
    active: input.active ?? 0,
    capacity: input.capacity ?? 2,
    maxWaitMs: input.maxWaitMs ?? 60_000,
    accepting: input.accepting ?? true,
  };
}

function input(
  llm: RuntimeLifecycleSnapshot,
  stopping = false,
): Readonly<{
  stopping: boolean;
  lifecycles: Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>>;
}> {
  return { stopping, lifecycles: snapshots(llm) };
}

test("reports a configured idle modality with bounded queue capacity as ready", () => {
  const result = composeGatewayReadiness(
    input(
      lifecycle("llm", {
        configured: true,
        state: "idle",
        admission: { kind: "known", accepting: true, activeCount: 0 },
        queue: openQueue({ waiting: 1, capacity: 2 }),
      }),
    ),
  );

  expect(result).toEqual({
    status: "ready",
    reason: "request_admission_available",
    modalities: ["llm"],
  });
});

test("recovers readiness when a saturated queue regains bounded capacity", () => {
  const saturated = lifecycle("llm", {
    configured: true,
    state: "running",
    admission: { kind: "known", accepting: true, activeCount: 1 },
    queue: openQueue({ waiting: 2, capacity: 2 }),
  });
  const recovered = lifecycle("llm", {
    configured: true,
    state: "running",
    admission: { kind: "known", accepting: true, activeCount: 1 },
    queue: openQueue({ waiting: 1, capacity: 2 }),
  });

  expect(composeGatewayReadiness(input(saturated))).toEqual({
    status: "unready",
    reason: "no_request_admission",
    modalities: [],
  });
  expect(composeGatewayReadiness(input(recovered))).toEqual({
    status: "ready",
    reason: "request_admission_available",
    modalities: ["llm"],
  });
});

test("does not claim readiness for closed, unavailable, draining, or failed admission", () => {
  const unavailable = [
    lifecycle("llm", {
      configured: true,
      state: "running",
      admission: { kind: "known", accepting: true, activeCount: 0 },
      queue: openQueue({ accepting: false }),
    }),
    lifecycle("llm", {
      configured: true,
      state: "running",
      admission: { kind: "known", accepting: true, activeCount: 0 },
      queue: null,
    }),
    lifecycle("llm", {
      configured: true,
      state: "draining",
      admission: { kind: "known", accepting: false, activeCount: 1 },
      queue: openQueue(),
    }),
    lifecycle("llm", {
      configured: true,
      state: "failed",
      admission: { kind: "known", accepting: true, activeCount: 0 },
      queue: openQueue(),
    }),
  ];

  for (const snapshot of unavailable) {
    expect(composeGatewayReadiness(input(snapshot))).toEqual({
      status: "unready",
      reason: "no_request_admission",
      modalities: [],
    });
  }
});

test("reports stopping before inspecting configured admission", () => {
  const result = composeGatewayReadiness(
    input(
      lifecycle("llm", {
        configured: true,
        state: "running",
        admission: { kind: "known", accepting: true, activeCount: 0 },
        queue: openQueue(),
      }),
      true,
    ),
  );

  expect(result).toEqual({
    status: "unready",
    reason: "gateway_stopping",
    modalities: [],
  });
});

test("validates only the aggregate public readiness shape", () => {
  expect(
    gatewayReadinessSchema.safeParse({
      status: "ready",
      reason: "request_admission_available",
      modalities: ["llm"],
      modelId: "private-model",
    }).success,
  ).toBe(false);
});
