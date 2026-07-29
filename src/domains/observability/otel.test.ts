import { afterEach, describe, expect, test } from "bun:test";
import { SpanKind } from "@opentelemetry/api";
import { createLogEvent, logEventSchema } from "./logging";
import {
  createOtelRuntime,
  normalizedOtelRoute,
  resolveOtelConfiguration,
  serverSpanName,
  serverSpanOptions,
  type OtelRuntime,
  type OtelConfiguration,
} from "./otel";
import { defaultConfig } from "../../manager";

const runtimes: OtelRuntime[] = [];

afterEach(async () => {
  await Promise.all(runtimes.splice(0).map((runtime) => runtime.shutdown()));
});

function containsBytes(haystack: Uint8Array, needle: Uint8Array): boolean {
  outer: for (
    let index = 0;
    index <= haystack.length - needle.length;
    index++
  ) {
    for (let offset = 0; offset < needle.length; offset++) {
      if (haystack[index + offset] !== needle[offset]) continue outer;
    }
    return true;
  }
  return false;
}

function hexBytes(value: string): Uint8Array {
  return Uint8Array.from(
    value.match(/../g)!.map((byte) => Number.parseInt(byte, 16)),
  );
}

function protobufFields(bytes: Uint8Array): Map<number, Uint8Array[]> {
  const fields = new Map<number, Uint8Array[]>();
  let offset = 0;
  const varint = () => {
    let value = 0;
    let shift = 0;
    while (offset < bytes.length) {
      const byte = bytes[offset++];
      value += (byte & 0x7f) * 2 ** shift;
      if ((byte & 0x80) === 0) return value;
      shift += 7;
    }
    throw new Error("Truncated protobuf varint.");
  };
  while (offset < bytes.length) {
    const tag = varint();
    const field = Math.floor(tag / 8);
    const wire = tag & 7;
    if (wire === 0) {
      varint();
      continue;
    }
    if (wire === 1) {
      offset += 8;
      continue;
    }
    if (wire === 5) {
      offset += 4;
      continue;
    }
    if (wire !== 2) throw new Error(`Unsupported protobuf wire type ${wire}.`);
    const length = varint();
    const value = bytes.slice(offset, offset + length);
    offset += length;
    const existing = fields.get(field) ?? [];
    existing.push(value);
    fields.set(field, existing);
  }
  return fields;
}

function protobufText(bytes: Uint8Array | undefined): string {
  return bytes ? new TextDecoder().decode(bytes) : "";
}

function decodedTraceStrings(payload: Uint8Array): string[] {
  const result: string[] = [];
  for (const resourceSpans of protobufFields(payload).get(1) ?? []) {
    for (const scopeSpans of protobufFields(resourceSpans).get(2) ?? []) {
      for (const span of protobufFields(scopeSpans).get(2) ?? []) {
        const spanFields = protobufFields(span);
        result.push(protobufText(spanFields.get(5)?.[0]));
        const status = spanFields.get(15)?.[0];
        if (status)
          result.push(protobufText(protobufFields(status).get(2)?.[0]));
        for (const event of spanFields.get(11) ?? []) {
          const eventFields = protobufFields(event);
          result.push(protobufText(eventFields.get(1)?.[0]));
          for (const attribute of eventFields.get(3) ?? []) {
            const attributeFields = protobufFields(attribute);
            result.push(protobufText(attributeFields.get(1)?.[0]));
            const anyValue = attributeFields.get(2)?.[0];
            if (anyValue) {
              result.push(protobufText(protobufFields(anyValue).get(1)?.[0]));
            }
          }
        }
      }
    }
  }
  return result.filter(Boolean);
}

describe("OpenTelemetry configuration", () => {
  test("is disabled without an endpoint and honors validated standard overrides", () => {
    const config = defaultConfig("/tmp/localbase-otel-config");
    expect(resolveOtelConfiguration(config, {}).enabled).toBe(false);

    config.otelEndpoint = "http://persistent.example:4318";
    config.otelHeaders = "x-tenant=persistent";
    config.otelSampleRatio = 25;
    expect(
      resolveOtelConfiguration(config, {
        OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector.example:4318",
        OTEL_EXPORTER_OTLP_TRACES_HEADERS: "x-signal=trace",
        OTEL_TRACES_SAMPLER: "parentbased_traceidratio",
        OTEL_TRACES_SAMPLER_ARG: "0.5",
      }),
    ).toEqual({
      enabled: true,
      tracesEndpoint: "http://collector.example:4318/v1/traces",
      logsEndpoint: "http://collector.example:4318/v1/logs",
      headers: { "x-tenant": "persistent" },
      tracesHeaders: { "x-signal": "trace" },
      logsHeaders: {},
      sampleRatio: 0.5,
      sampler: "parentbased_traceidratio",
      source: "environment",
      displayEndpoint: "http://collector.example:4318/",
    });

    expect(() =>
      resolveOtelConfiguration(config, {
        OTEL_EXPORTER_OTLP_ENDPOINT: "file:///tmp/collector",
      }),
    ).toThrow();
    expect(() =>
      resolveOtelConfiguration(config, {
        OTEL_EXPORTER_OTLP_HEADERS: "authorization=bad%0Aheader",
      }),
    ).toThrow();
    expect(
      resolveOtelConfiguration(config, {
        OTEL_EXPORTER_OTLP_ENDPOINT: "https://collector.example/base/",
      }).tracesEndpoint,
    ).toBe("https://collector.example/base/v1/traces");
    expect(() =>
      resolveOtelConfiguration(config, {
        OTEL_TRACES_SAMPLER: "always_on",
        OTEL_TRACES_SAMPLER_ARG: "0.5",
      }),
    ).toThrow();
    for (const endpoint of [
      "https://user:password@collector.example",
      "https://collector.example?authorization=Bearer-secret",
      "https://collector.example/#sk-proj-secret",
    ]) {
      expect(() =>
        resolveOtelConfiguration(config, {
          OTEL_EXPORTER_OTLP_ENDPOINT: endpoint,
        }),
      ).toThrow();
    }

    const headerOverride = resolveOtelConfiguration(config, {
      OTEL_EXPORTER_OTLP_HEADERS: "x-tenant=environment",
    });
    expect(headerOverride.source).toBe("environment");
    expect(headerOverride.displayEndpoint).toBe(
      "http://persistent.example:4318/",
    );
  });
});

test("normalizes server span routes through a closed allowlist", () => {
  expect(serverSpanName("post", "/v1/chat/completions")).toBe(
    "POST /v1/chat/completions",
  );
  expect(normalizedOtelRoute("/private/path?token=secret")).toBe(
    "unmatched-route",
  );
  expect(serverSpanName("get", "/private/path?token=secret")).toBe(
    "GET unmatched-route",
  );
});

test("redacts decoded exported trace exception and status fields", async () => {
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
  try {
    const runtime = createOtelRuntime({
      enabled: true,
      tracesEndpoint: `http://127.0.0.1:${collector.port}/v1/traces`,
      headers: {},
      tracesHeaders: {},
      logsHeaders: {},
      sampleRatio: 1,
      sampler: "always_on",
      source: "persistent",
      displayEndpoint: `http://127.0.0.1:${collector.port}/`,
    });
    runtimes.push(runtime);
    const bearer = "bearer-regression-secret";
    const cookie = "cookie-regression-secret";
    const apiToken = `sk-proj-${"a".repeat(24)}`;
    const prompt = "prompt-regression-content";
    const failure = Object.assign(
      new Error(`authorization=Bearer ${bearer}; prompt: ${prompt}`),
      {
        name: `Bearer ${bearer}`,
        stack: `Cookie: session=${cookie}\nAPI key=${apiToken}`,
        code: `ghp_${"b".repeat(32)}`,
      },
    );
    await expect(
      runtime.withSpan(
        "trace.redaction",
        serverSpanOptions("POST", "/v1/chat/completions"),
        () => {
          throw failure;
        },
      ),
    ).rejects.toBe(failure);
    await runtime.forceFlush();
    expect(traces).toHaveLength(1);
    const decoded = decodedTraceStrings(traces[0]);
    const exported = decoded.join("\n");
    for (const forbidden of [bearer, cookie, apiToken, prompt, failure.code]) {
      expect(exported).not.toContain(forbidden);
    }
    expect(exported).toContain("[REDACTED");
  } finally {
    collector.stop(true);
  }
});

test("implements each standard sampler's remote-parent semantics", async () => {
  const cases: Array<{
    sampler: OtelConfiguration["sampler"];
    ratio: number;
    sampledParent: boolean;
    unsampledParent: boolean;
  }> = [
    {
      sampler: "always_on",
      ratio: 1,
      sampledParent: true,
      unsampledParent: true,
    },
    {
      sampler: "always_off",
      ratio: 1,
      sampledParent: false,
      unsampledParent: false,
    },
    {
      sampler: "traceidratio",
      ratio: 1,
      sampledParent: true,
      unsampledParent: true,
    },
    {
      sampler: "parentbased_always_on",
      ratio: 1,
      sampledParent: true,
      unsampledParent: false,
    },
    {
      sampler: "parentbased_always_off",
      ratio: 1,
      sampledParent: true,
      unsampledParent: false,
    },
    {
      sampler: "parentbased_traceidratio",
      ratio: 1,
      sampledParent: true,
      unsampledParent: false,
    },
  ];
  const traceId = "0af7651916cd43dd8448eb211c80319c";
  for (const current of cases) {
    const runtime = createOtelRuntime({
      enabled: true,
      logsEndpoint: "http://127.0.0.1:1/v1/logs",
      headers: {},
      tracesHeaders: {},
      logsHeaders: {},
      sampleRatio: current.ratio,
      sampler: current.sampler,
      source: "environment",
      displayEndpoint: "http://127.0.0.1:1/",
    });
    runtimes.push(runtime);
    for (const [flags, expected] of [
      ["01", current.sampledParent],
      ["00", current.unsampledParent],
    ] as const) {
      const parent = runtime.extract(
        new Headers({
          traceparent: `00-${traceId}-b7ad6b7169203331-${flags}`,
        }),
      );
      await runtime.withSpan(
        `sampler.${current.sampler}.${flags}`,
        serverSpanOptions("GET", "/health"),
        (span) => expect(span.isRecording()).toBe(expected),
        parent,
      );
    }
  }
});

test("exports correlated OTLP logs and parented W3C spans to a collector", async () => {
  const requests: Array<{ path: string; headers: Headers; body: Uint8Array }> =
    [];
  const collector = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      requests.push({
        path: new URL(request.url).pathname,
        headers: request.headers,
        body: new Uint8Array(await request.arrayBuffer()),
      });
      return new Response(new Uint8Array(), {
        status: 200,
        headers: { "content-type": "application/x-protobuf" },
      });
    },
  });

  try {
    const endpoint = `http://127.0.0.1:${collector.port}`;
    const runtime = createOtelRuntime({
      enabled: true,
      tracesEndpoint: `${endpoint}/v1/traces`,
      logsEndpoint: `${endpoint}/v1/logs`,
      headers: { "x-test": "collector" },
      tracesHeaders: {},
      logsHeaders: {},
      sampleRatio: 1,
      sampler: "parentbased_traceidratio",
      source: "persistent",
      displayEndpoint: endpoint,
    });
    runtimes.push(runtime);

    const inboundTraceId = "0af7651916cd43dd8448eb211c80319c";
    const inboundParentId = "b7ad6b7169203331";
    const parent = runtime.extract(
      new Headers({
        traceparent: `00-${inboundTraceId}-${inboundParentId}-01`,
        tracestate: "vendor=value",
      }),
    );
    let serverSpanId = "";
    let childSpanId = "";
    await runtime.withSpan(
      "HTTP POST",
      serverSpanOptions("POST", "/v1/chat/completions"),
      async () => {
        const correlation = runtime.activeCorrelation()!;
        serverSpanId = correlation.spanId;
        const event = logEventSchema.parse(
          createLogEvent({
            severity: "info",
            eventName: "http.request",
            category: "http",
            component: "gateway",
            runtime: "gateway",
            message: "Request finished.",
            requestId: "safe-request",
            traceId: correlation.traceId,
            spanId: correlation.spanId,
            attributes: {
              authorization: "Bearer secret-value",
              prompt: "do not export",
            },
          }),
        );
        runtime.emit(event);
        await runtime.withSpan(
          "localbase.backend.inference",
          { kind: SpanKind.CLIENT },
          () => {
            childSpanId = runtime.activeCorrelation()!.spanId;
          },
        );
      },
      parent,
    );
    await runtime.forceFlush();

    expect(requests.map((request) => request.path).sort()).toEqual([
      "/v1/logs",
      "/v1/traces",
    ]);
    expect(
      requests.every(
        (request) => request.headers.get("x-test") === "collector",
      ),
    ).toBe(true);
    const tracePayload = requests.find(
      (request) => request.path === "/v1/traces",
    )!.body;
    const logPayload = requests.find(
      (request) => request.path === "/v1/logs",
    )!.body;
    expect(containsBytes(tracePayload, hexBytes(inboundTraceId))).toBe(true);
    expect(containsBytes(tracePayload, hexBytes(inboundParentId))).toBe(true);
    expect(containsBytes(tracePayload, hexBytes(serverSpanId))).toBe(true);
    expect(containsBytes(tracePayload, hexBytes(childSpanId))).toBe(true);
    expect(containsBytes(logPayload, hexBytes(inboundTraceId))).toBe(true);
    expect(containsBytes(logPayload, hexBytes(serverSpanId))).toBe(true);
    const exportedText = new TextDecoder().decode(logPayload);
    expect(exportedText).toContain("http.request");
    expect(exportedText).not.toContain("secret-value");
    expect(exportedText).not.toContain("do not export");

    const malformed = runtime.extract(
      new Headers({ traceparent: "not-a-traceparent", tracestate: "bad" }),
    );
    await runtime.withSpan(
      "malformed",
      serverSpanOptions("GET", "/health"),
      () => {
        expect(runtime.activeCorrelation()!.traceId).not.toBe(inboundTraceId);
      },
      malformed,
    );
  } finally {
    collector.stop(true);
  }
});

test("collector outage never rejects application work or shutdown", async () => {
  const diagnostics: string[] = [];
  const runtime = createOtelRuntime(
    {
      enabled: true,
      tracesEndpoint: "http://127.0.0.1:1/v1/traces",
      logsEndpoint: "http://127.0.0.1:1/v1/logs",
      headers: {},
      tracesHeaders: {},
      logsHeaders: {},
      sampleRatio: 1,
      sampler: "parentbased_traceidratio",
      source: "persistent",
      displayEndpoint: "http://127.0.0.1:1/",
    },
    (_severity, message) => diagnostics.push(message),
  );
  runtimes.push(runtime);
  await expect(
    runtime.withSpan("outage", serverSpanOptions("GET", "/health"), () => 42),
  ).resolves.toBe(42);
  runtime.emit(
    createLogEvent({
      severity: "warn",
      eventName: "observability.test",
      category: "logging",
      component: "otel",
      runtime: "gateway",
      message: "Collector unavailable.",
    }),
  );
  await expect(runtime.shutdown()).resolves.toBeUndefined();
  expect(diagnostics.length).toBeGreaterThan(0);
  expect(diagnostics.length).toBeLessThanOrEqual(4);
});

test(
  "bounds total shutdown time with a hung collector and saturated queues",
  async () => {
    let markRequestStarted!: () => void;
    const requestStarted = new Promise<void>((resolve) => {
      markRequestStarted = resolve;
    });
    const collector = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      async fetch() {
        markRequestStarted();
        return await new Promise<Response>(() => {});
      },
    });
    const diagnostics: string[] = [];
    const endpoint = `http://127.0.0.1:${collector.port}`;
    const runtime = createOtelRuntime(
      {
        enabled: true,
        tracesEndpoint: `${endpoint}/v1/traces`,
        logsEndpoint: `${endpoint}/v1/logs`,
        headers: {},
        tracesHeaders: {},
        logsHeaders: {},
        sampleRatio: 1,
        sampler: "always_on",
        source: "persistent",
        displayEndpoint: endpoint,
      },
      (_severity, message) => diagnostics.push(message),
    );
    runtimes.push(runtime);

    try {
      const event = createLogEvent({
        severity: "info",
        eventName: "observability.test",
        category: "logging",
        component: "otel",
        runtime: "gateway",
        message: "Queue saturation probe.",
      });
      for (let index = 0; index < 5_000; index++) {
        runtime.emit(event);
        await runtime.withSpan(
          "shutdown.saturation",
          serverSpanOptions("GET", "/health"),
          () => {},
        );
      }
      await Promise.race([
        requestStarted,
        Bun.sleep(2_000).then(() => {
          throw new Error("Hung collector did not receive an export.");
        }),
      ]);

      const startedAt = performance.now();
      await runtime.shutdown();
      const elapsedMs = performance.now() - startedAt;

      expect(elapsedMs).toBeGreaterThanOrEqual(4_500);
      expect(elapsedMs).toBeLessThan(5_750);
      expect(diagnostics).toContain(
        "OpenTelemetry shutdown deadline exceeded.",
      );
    } finally {
      collector.stop(true);
    }
  },
  { timeout: 10_000 },
);

test(
  "cancels hung exports so a saturated child exits naturally",
  async () => {
    const collector = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      async fetch() {
        return await new Promise<Response>(() => {});
      },
    });
    const startedAt = Date.now();
    const child = Bun.spawn(
      [
        process.execPath,
        "run",
        "src/test/otel-hung-shutdown.fixture.ts",
        `http://127.0.0.1:${collector.port}`,
      ],
      { cwd: process.cwd(), stdout: "pipe", stderr: "pipe" },
    );

    try {
      const exitCode = await Promise.race([
        child.exited,
        Bun.sleep(12_000).then(() => {
          throw new Error("Telemetry child retained event-loop resources.");
        }),
      ]);
      const exitedAt = Date.now();
      const stdout = await new Response(child.stdout).text();
      const stderr = await new Response(child.stderr).text();
      expect(exitCode, stderr).toBe(0);
      const timing = JSON.parse(stdout.trim()) as {
        shutdownMs: number;
        shutdownReturnedAt: number;
      };
      expect(timing.shutdownMs).toBeGreaterThanOrEqual(4_500);
      expect(timing.shutdownMs).toBeLessThan(5_750);
      expect(exitedAt - timing.shutdownReturnedAt).toBeLessThan(1_000);
      expect(exitedAt - startedAt).toBeLessThan(9_000);
    } finally {
      if (child.exitCode === null) child.kill();
      collector.stop(true);
    }
  },
  { timeout: 13_000 },
);
