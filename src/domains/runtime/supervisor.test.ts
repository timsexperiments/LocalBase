import { expect, test } from "bun:test";
import type { ILogger } from "../observability/logging";
import { LocalBaseLogger } from "../observability/logging";
import { createOtelRuntime } from "../observability/otel";
import { decodeOtlpTraceSpans } from "../../test/otlp-fixture";
import type { RuntimeLaunchPlan } from "./launch-plan";
import {
  RuntimeMemoryAdmissionError,
  type MemorySafetyController,
} from "./memory-controller";
import { ManagedService } from "./supervisor";

function testLaunchPlan(runtimeId: string): RuntimeLaunchPlan {
  return {
    runtimeId,
    modality: "llm",
    component: "llama-server",
    root: "/tmp",
    modelId: "test",
    modelFile: "test.gguf",
    modelPath: "/tmp/test.gguf",
    host: "127.0.0.1",
    port: 1,
    healthUrl: "http://127.0.0.1:1/health",
    ctxSize: 8192,
    parallel: 1,
    modelRequirementGb: 1,
    hardware: { memoryGb: 1 },
    memoryDemand: {
      unifiedBytes: 1,
      hostBytes: 1,
      acceleratorBytes: 1,
      confidence: "authoritative",
    },
  };
}

function testMemorySafety(): MemorySafetyController {
  return {
    async reserve({ runtimeId }: { runtimeId: string }) {
      return { runtimeId, materialize() {}, release() {} };
    },
  } as unknown as MemorySafetyController;
}

function recordingMemorySafety(): {
  controller: MemorySafetyController;
  reserves: string[];
  materializations: string[];
  releases: string[];
} {
  const reserves: string[] = [];
  const materializations: string[] = [];
  const releases: string[] = [];
  return {
    controller: {
      async reserve({ runtimeId }: { runtimeId: string }) {
        reserves.push(runtimeId);
        let released = false;
        return {
          runtimeId,
          materialize() {
            materializations.push(runtimeId);
          },
          release() {
            if (released) return;
            released = true;
            releases.push(runtimeId);
          },
        };
      },
    } as unknown as MemorySafetyController,
    reserves,
    materializations,
    releases,
  };
}

function recordingLogger(eventNames: string[]): ILogger {
  return {
    info() {},
    warn() {},
    error() {},
    event(input) {
      eventNames.push(input.eventName);
    },
    request() {},
    pipeStream() {},
    async enableFileLogging() {},
    async close() {},
  };
}

test("backend health failure exports an error model-load span", async () => {
  const traces: Uint8Array[] = [];
  const collector = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      if (new URL(request.url).pathname === "/v1/traces") {
        traces.push(new Uint8Array(await request.arrayBuffer()));
      }
      return new Response(new Uint8Array(), {
        headers: { "content-type": "application/x-protobuf" },
      });
    },
  });
  const endpoint = `http://127.0.0.1:${collector.port}`;
  const otel = createOtelRuntime({
    enabled: true,
    tracesEndpoint: `${endpoint}/v1/traces`,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: endpoint,
  });
  const logger = new LocalBaseLogger("json");
  const service = new ManagedService({
    runtimeId: "llm:test:1",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger,
    launch: async () => testLaunchPlan("llm:test:1"),
    start: async () => Bun.spawn(["/bin/sleep", "60"]),
    memorySafety: testMemorySafety(),
    otel,
    startupTimeoutMs: 10,
  });
  const originalLog = console.log;
  console.log = () => {};
  try {
    await expect(service.ensureRunning()).rejects.toThrow(
      "Backend health check timed out.",
    );
    expect(service.state()).toBe("failed");
    await otel.forceFlush();
    const modelLoad = traces
      .flatMap(decodeOtlpTraceSpans)
      .find((span) => span.name === "localbase.backend.model_load");
    expect(modelLoad?.statusCode).toBe(2);
  } finally {
    await service.shutdown();
    await otel.shutdown();
    await logger.close();
    console.log = originalLog;
    collector.stop(true);
  }
});

test("backend signal exit fails startup without waiting for the health timeout", async () => {
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  const logger = new LocalBaseLogger("json");
  const service = new ManagedService({
    runtimeId: "llm:test:2",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger,
    launch: async () => testLaunchPlan("llm:test:2"),
    start: async () => {
      const child = Bun.spawn([
        process.execPath,
        "-e",
        "setInterval(() => {}, 1000)",
      ]);
      setTimeout(() => child.kill(15), 25);
      return child;
    },
    memorySafety: testMemorySafety(),
    otel,
    startupTimeoutMs: 5_000,
  });
  const originalLog = console.log;
  console.log = () => {};
  try {
    await expect(service.ensureRunning()).rejects.toThrow("signal SIGTERM");
    expect(service.state()).toBe("failed");
  } finally {
    await service.shutdown();
    console.log = originalLog;
    await otel.shutdown();
    await logger.close();
  }
});

test("startup failure releases its reservation", async () => {
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  const memory = recordingMemorySafety();
  const service = new ManagedService({
    runtimeId: "llm:failure:1",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger: recordingLogger([]),
    launch: async () => testLaunchPlan("llm:failure:1"),
    start: async () => {
      throw new Error("spawn failed");
    },
    memorySafety: memory.controller,
    otel,
  });
  try {
    await expect(service.ensureRunning()).rejects.toThrow("spawn failed");
    expect(memory.reserves).toEqual(["llm:failure:1"]);
    expect(memory.releases).toEqual(["llm:failure:1"]);
  } finally {
    await service.shutdown();
    await otel.shutdown();
  }
});

test("memory admission rejection remains retryable", async () => {
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  const events: string[] = [];
  const rejection = new RuntimeMemoryAdmissionError({
    kind: "rejected",
    reason: "system-memory",
    poolId: "system",
  });
  let reservations = 0;
  let starts = 0;
  const service = new ManagedService({
    runtimeId: "llm:memory:1",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger: recordingLogger(events),
    launch: async () => testLaunchPlan("llm:memory:1"),
    start: async () => {
      starts += 1;
      return Bun.spawn(["/bin/sleep", "60"]);
    },
    memorySafety: {
      async reserve() {
        reservations += 1;
        throw rejection;
      },
    } as unknown as MemorySafetyController,
    otel,
  });

  try {
    await expect(service.ensureRunning()).rejects.toBe(rejection);
    await expect(service.ensureRunning()).rejects.toBe(rejection);
    expect(service.state()).toBe("idle");
    expect(reservations).toBe(2);
    expect(starts).toBe(0);
    expect(events).not.toContain("backend.start-failed");
  } finally {
    await service.shutdown();
    await otel.shutdown();
  }
});

test("cancelling while memory reservation is pending prevents backend start", async () => {
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  let markReservationStarted: () => void;
  const reservationStarted = new Promise<void>((resolve) => {
    markReservationStarted = resolve;
  });
  let completeReservation: (reservation: {
    runtimeId: string;
    materialize(): void;
    release(): void;
  }) => void;
  const reservationReady = new Promise<{
    runtimeId: string;
    materialize(): void;
    release(): void;
  }>((resolve) => {
    completeReservation = resolve;
  });
  let starts = 0;
  let releases = 0;
  const service = new ManagedService({
    runtimeId: "llm:cancellation:1",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger: recordingLogger([]),
    launch: async () => testLaunchPlan("llm:cancellation:1"),
    start: async () => {
      starts += 1;
      throw new Error("Backend must not start after cancellation.");
    },
    memorySafety: {
      async reserve() {
        markReservationStarted!();
        return await reservationReady;
      },
    } as unknown as MemorySafetyController,
    otel,
  });

  try {
    const starting = service.ensureRunning();
    await reservationStarted;
    const stopping = service.kill();
    completeReservation!({
      runtimeId: "llm:cancellation:1",
      materialize() {},
      release() {
        releases += 1;
      },
    });
    await stopping;
    await expect(starting).rejects.toThrow("startup was cancelled");
    expect(starts).toBe(0);
    expect(releases).toBe(1);
    expect(service.state()).toBe("idle");
  } finally {
    await service.shutdown();
    await otel.shutdown();
  }
});

test("managed processes reacquire and release reservations across stop and crash recovery", async () => {
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  const memory = recordingMemorySafety();
  const processes: Bun.Subprocess[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(null, { status: 200 })) as unknown as typeof globalThis.fetch;
  const service = new ManagedService({
    runtimeId: "llm:recovery:1",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger: recordingLogger([]),
    launch: async () => testLaunchPlan("llm:recovery:1"),
    start: async () => {
      const child = Bun.spawn([
        process.execPath,
        "-e",
        "setInterval(() => {}, 1000)",
      ]);
      processes.push(child);
      return child;
    },
    memorySafety: memory.controller,
    otel,
  });
  try {
    await service.ensureRunning();
    await service.kill();
    await service.ensureRunning();
    processes[1]?.kill(15);
    const deadline = Date.now() + 2_000;
    while (processes.length < 3 && Date.now() < deadline) {
      await Bun.sleep(10);
    }
    expect(processes).toHaveLength(3);
    await service.shutdown();
    expect(memory.reserves).toHaveLength(3);
    expect(memory.materializations).toHaveLength(3);
    expect(memory.releases).toHaveLength(3);
  } finally {
    await service.shutdown();
    globalThis.fetch = originalFetch;
    await otel.shutdown();
  }
});

test("intentional startup cancellation settles idle without recording a failure", async () => {
  let healthy = false;
  let markStarted: () => void;
  const started = new Promise<void>((resolve) => {
    markStarted = resolve;
  });
  const backend = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: () => new Response(null, { status: healthy ? 200 : 503 }),
  });
  const events: string[] = [];
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  let starts = 0;
  const service = new ManagedService({
    runtimeId: "stt:test:1",
    modality: "stt",
    component: "whisper-server",
    healthUrl: `http://127.0.0.1:${backend.port}/health`,
    logger: recordingLogger(events),
    launch: async () => testLaunchPlan("stt:test:1"),
    start: async () => {
      starts += 1;
      const child = Bun.spawn([
        process.execPath,
        "-e",
        "setInterval(() => {}, 1000)",
      ]);
      if (starts === 1) markStarted!();
      return child;
    },
    memorySafety: testMemorySafety(),
    otel,
  });
  try {
    const cancelled = service.ensureRunning();
    await started;
    await service.kill();
    await expect(cancelled).rejects.toThrow("startup was cancelled");
    expect(service.state()).toBe("idle");
    expect(events).not.toContain("backend.start-failed");
    expect(events).not.toContain("backend.crash");

    healthy = true;
    await service.ensureRunning();
    expect(service.state()).toBe("running");
    expect(starts).toBe(2);
  } finally {
    await service.shutdown();
    await otel.shutdown();
    backend.stop(true);
  }
});

test("a cancelled startup generation cannot publish a stale process", async () => {
  let healthy = false;
  let markStartEntered: () => void;
  const startEntered = new Promise<void>((resolve) => {
    markStartEntered = resolve;
  });
  let releaseFirstStart: () => void;
  const firstStartReleased = new Promise<void>((resolve) => {
    releaseFirstStart = resolve;
  });
  const backend = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: () => new Response(null, { status: healthy ? 200 : 503 }),
  });
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  let starts = 0;
  const service = new ManagedService({
    runtimeId: "image:test:1",
    modality: "image",
    component: "sd-server",
    healthUrl: `http://127.0.0.1:${backend.port}/health`,
    logger: recordingLogger([]),
    launch: async () => testLaunchPlan("image:test:1"),
    start: async () => {
      starts += 1;
      if (starts === 1) {
        markStartEntered!();
        await firstStartReleased;
      }
      return Bun.spawn([process.execPath, "-e", "setInterval(() => {}, 1000)"]);
    },
    memorySafety: testMemorySafety(),
    otel,
  });
  try {
    const staleStartup = service.ensureRunning();
    await startEntered;
    const killing = service.kill();
    releaseFirstStart!();
    await killing;
    await expect(staleStartup).rejects.toThrow("startup was cancelled");
    expect(service.state()).toBe("idle");

    healthy = true;
    await service.ensureRunning();
    expect(service.state()).toBe("running");
    expect(starts).toBe(2);
  } finally {
    await service.shutdown();
    await otel.shutdown();
    backend.stop(true);
  }
});

test("terminal crash limits reject future backend work", async () => {
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  const events: string[] = [];
  let starts = 0;
  const service = new ManagedService({
    runtimeId: "llm:test:3",
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger: recordingLogger(events),
    launch: async () => testLaunchPlan("llm:test:3"),
    start: async () => {
      starts += 1;
      return Bun.spawn([process.execPath, "-e", "setInterval(() => {}, 1000)"]);
    },
    memorySafety: testMemorySafety(),
    otel,
  });
  (service as unknown as { crashTimes: number[] }).crashTimes = Array.from(
    { length: 5 },
    () => Date.now(),
  );
  try {
    await expect(service.ensureRunning()).rejects.toThrow("repeated failures");
    expect(service.state()).toBe("failed");
    await expect(service.ensureRunning()).rejects.toThrow("repeated failures");
    expect(starts).toBe(0);
    expect(events).toContain("backend.crash");
    expect(events).not.toContain("backend.start-failed");
  } finally {
    await service.shutdown();
    await otel.shutdown();
  }
});
test("managed service reports startup, running, recovery, and stopping states", async () => {
  const backend = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: () => new Response(null, { status: 200 }),
  });
  const otel = createOtelRuntime({
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "always_on",
    source: "persistent",
    displayEndpoint: "disabled",
  });
  const logger = new LocalBaseLogger("json");
  const processes: Bun.Subprocess[] = [];
  const service = new ManagedService({
    runtimeId: "llm:test:4",
    modality: "llm",
    component: "llama-server",
    healthUrl: `http://127.0.0.1:${backend.port}/health`,
    logger,
    launch: async () => testLaunchPlan("llm:test:4"),
    start: async () => {
      const child = Bun.spawn([
        process.execPath,
        "-e",
        "setInterval(() => {}, 1000)",
      ]);
      processes.push(child);
      return child;
    },
    memorySafety: testMemorySafety(),
    otel,
  });
  const originalLog = console.log;
  console.log = () => {};
  try {
    const starting = service.ensureRunning();
    expect(service.state()).toBe("starting");
    await starting;
    expect(service.state()).toBe("running");

    processes[0]?.kill(15);
    await processes[0]?.exited;
    const recoveryDeadline = Date.now() + 2_000;
    while (service.state() !== "starting" && Date.now() < recoveryDeadline) {
      await Bun.sleep(10);
    }
    expect(service.state()).toBe("starting");

    const stopping = service.shutdown();
    expect(service.state()).toBe("stopping");
    await stopping;
    expect(service.state()).toBe("stopping");
  } finally {
    await service.shutdown();
    console.log = originalLog;
    await otel.shutdown();
    await logger.close();
    backend.stop(true);
  }
});
