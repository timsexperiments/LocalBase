import { expect, test } from "bun:test";
import type { ILogger } from "../observability/logging";
import { LocalBaseLogger } from "../observability/logging";
import { createOtelRuntime } from "../observability/otel";
import { decodeOtlpTraceSpans } from "../../test/otlp-fixture";
import { ManagedService } from "./supervisor";

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
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger,
    start: async () => Bun.spawn(["/bin/sleep", "60"]),
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
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger,
    start: async () => {
      const child = Bun.spawn([
        process.execPath,
        "-e",
        "setInterval(() => {}, 1000)",
      ]);
      setTimeout(() => child.kill(15), 25);
      return child;
    },
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
    modality: "stt",
    component: "whisper-server",
    healthUrl: `http://127.0.0.1:${backend.port}/health`,
    logger: recordingLogger(events),
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
    modality: "image",
    component: "sd-server",
    healthUrl: `http://127.0.0.1:${backend.port}/health`,
    logger: recordingLogger([]),
    start: async () => {
      starts += 1;
      if (starts === 1) {
        markStartEntered!();
        await firstStartReleased;
      }
      return Bun.spawn([process.execPath, "-e", "setInterval(() => {}, 1000)"]);
    },
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
    modality: "llm",
    component: "llama-server",
    healthUrl: "http://127.0.0.1:1/health",
    logger: recordingLogger(events),
    start: async () => {
      starts += 1;
      return Bun.spawn([process.execPath, "-e", "setInterval(() => {}, 1000)"]);
    },
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
    modality: "llm",
    component: "llama-server",
    healthUrl: `http://127.0.0.1:${backend.port}/health`,
    logger,
    start: async () => {
      const child = Bun.spawn([
        process.execPath,
        "-e",
        "setInterval(() => {}, 1000)",
      ]);
      processes.push(child);
      return child;
    },
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
