import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { byId, primaryArtifact } from "../../catalog";
import {
  startGatewayFixture,
  type GatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";
import { decodeOtlpTraceSpans } from "../../test/otlp-fixture";
import { LocalBaseLogger, readLogSnapshot } from "../observability/logging";
import { gatewayHealthSchema } from "./health";
import { ensureLocalBaseRootMarker } from "../../utils/root";
import {
  finalizeGatewayShutdown,
  httpBaseUrl,
  internalGatewayFailure,
  resourceUnavailable,
  withResponseLease,
} from "./commands/serve";

type ValidationCase = {
  name: string;
  path: string;
  init: RequestInit;
  expectedPath: string;
};

const STREAM_VALIDATION_FAILURE = `data: ${JSON.stringify({
  error: {
    message: "The upstream service returned an invalid event stream.",
    type: "server_error",
    param: null,
    code: "upstream_error",
  },
})}\n\ndata: [DONE]\n\n`;

function modelArtifactFile(modelId: string): string {
  const model = byId(modelId);
  if (!model) throw new Error(`Unknown catalog model: ${modelId}`);
  return primaryArtifact(model).filename;
}

test("formats IPv4, hostnames, and IPv6 literals as HTTP base URLs", () => {
  expect(httpBaseUrl("127.0.0.1", 2273)).toBe("http://127.0.0.1:2273");
  expect(httpBaseUrl("localhost", 2273)).toBe("http://localhost:2273");
  expect(httpBaseUrl("::1", 2273)).toBe("http://[::1]:2273");
});

test("normalizes unexpected gateway errors into an OpenAI error envelope", async () => {
  const response = internalGatewayFailure();
  expect(response.status).toBe(500);
  await expect(response.json()).resolves.toEqual({
    error: {
      message: "The gateway encountered an unexpected error.",
      type: "server_error",
      param: null,
      code: "gateway_error",
    },
  });
});

test("formats memory admission rejection as a retryable OpenAI error", async () => {
  const response = resourceUnavailable();

  expect(response.status).toBe(503);
  expect(response.headers.get("Retry-After")).toBe("5");
  await expect(response.json()).resolves.toEqual({
    error: {
      message:
        "Insufficient available memory to start the requested runtime. Please try again shortly.",
      type: "api_error",
      param: null,
      code: "insufficient_memory",
    },
  });
});

test("releases a response lease exactly once when the stream or request is cancelled", async () => {
  const createLeasedResponse = () => {
    let resolveCancelled: () => void;
    const cancelled = new Promise<void>((resolve) => {
      resolveCancelled = resolve;
    });
    const response = new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(new Uint8Array([1]));
        },
        cancel() {
          resolveCancelled!();
        },
      }),
    );
    return { cancelled, response };
  };

  const streamCancellation = createLeasedResponse();
  const streamAbort = new AbortController();
  let streamReleases = 0;
  const leasedStream = withResponseLease(
    streamCancellation.response,
    () => {
      streamReleases += 1;
    },
    streamAbort.signal,
  );
  const streamReader = leasedStream.body!.getReader();
  await streamReader.read();
  await streamReader.cancel();
  await streamCancellation.cancelled;
  expect(streamReleases).toBe(1);

  const requestCancellation = createLeasedResponse();
  const requestAbort = new AbortController();
  let requestReleases = 0;
  withResponseLease(
    requestCancellation.response,
    () => {
      requestReleases += 1;
    },
    requestAbort.signal,
  );
  requestAbort.abort();
  await requestCancellation.cancelled;
  expect(requestReleases).toBe(1);
});

test("shutdown records lease release failure, flushes stopped, and resolves", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-shutdown-log-"));
  ensureLocalBaseRootMarker(root);
  const logger = new LocalBaseLogger("json");
  const originalLog = console.log;
  console.log = () => {};
  try {
    await logger.enableFileLogging(root);
    const status = await finalizeGatewayShutdown(
      logger,
      async () => {
        throw new Error("lease release failed");
      },
      0,
    );
    expect(status).toBe(1);
    expect(
      (await readLogSnapshot(root)).map((event) => event.eventName),
    ).toEqual(["gateway.lease-release-failed", "gateway.stopped"]);
  } finally {
    console.log = originalLog;
    await logger.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("gateway returns service unavailable without proxying a failed backend", async () => {
  const gateway = await startGatewayFixture({
    llmBackendHealthy: false,
    llmRuntimeExitOnStart: true,
  });
  try {
    const response = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toMatchObject({
      error: { code: "service_unavailable" },
    });
    expect(gateway.upstreamRequests).toEqual([]);
  } finally {
    await gateway.stop();
  }
});

test("health is public while instance identity requires its private token", async () => {
  const gateway = await startGatewayFixture({ managedIdentity: true });
  try {
    const publicResponse = await fetch(`${gateway.baseUrl}/health`);
    expect(publicResponse.status).toBe(200);
    expect(await publicResponse.json()).toMatchObject({
      status: "ok",
      modalities: {
        llm: { configured: true, state: "idle" },
      },
    });

    const owner = (await Bun.file(
      join(gateway.root, "runtime", "gateway.lock", "owner.json"),
    ).json()) as {
      instanceToken: string;
      instanceId: string;
      rootHash: string;
    };
    const authenticated = await fetch(
      `${gateway.baseUrl}/_localbase/instance`,
      {
        headers: { "x-localbase-instance-token": owner.instanceToken },
      },
    );
    expect(await authenticated.json()).toMatchObject({
      instanceId: owner.instanceId,
      rootHash: owner.rootHash,
    });
    expect(
      (
        await fetch(`${gateway.baseUrl}/_localbase/instance`, {
          headers: { "x-localbase-instance-token": crypto.randomUUID() },
        })
      ).status,
    ).toBe(404);
  } finally {
    await gateway.stop({ preserveRoot: true });
    const stoppedEvents = await readLogSnapshot(gateway.root);
    expect(
      stoppedEvents.some((event) => event.eventName === "gateway.stopped"),
    ).toBe(true);
    rmSync(gateway.root, { recursive: true, force: true });
  }
});

test("compiled managed gateway writes redacted root-bound operational logs", async () => {
  const gateway = await startGatewayFixture({ managedIdentity: true });
  try {
    const response = await fetch(`${gateway.baseUrl}/health`, {
      headers: {
        "x-request-id": "compiled-managed-request",
        authorization: "Bearer private-token",
      },
    });
    expect(response.status).toBe(200);

    const deadline = Date.now() + 3_000;
    let events = await readLogSnapshot(gateway.root);
    while (
      Date.now() < deadline &&
      !events.some(
        (event) =>
          event.eventName === "http.request" &&
          event.requestId === "compiled-managed-request",
      )
    ) {
      await Bun.sleep(25);
      events = await readLogSnapshot(gateway.root);
    }
    expect(events.some((event) => event.eventName === "gateway.started")).toBe(
      true,
    );
    expect(
      events.some(
        (event) =>
          event.eventName === "http.request" &&
          event.requestId === "compiled-managed-request",
      ),
    ).toBe(true);
    expect(JSON.stringify(events)).not.toContain("private-token");
  } finally {
    await gateway.stop();
  }
});

test("compiled gateway continues W3C context and exports correlated telemetry", async () => {
  const received: Array<{ path: string; body: Uint8Array }> = [];
  const collector = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      received.push({
        path: new URL(request.url).pathname,
        body: new Uint8Array(await request.arrayBuffer()),
      });
      return new Response(new Uint8Array(), {
        headers: { "content-type": "application/x-protobuf" },
      });
    },
  });
  const gateway = await startGatewayFixture({
    otelEndpoint: `http://127.0.0.1:${collector.port}`,
  });
  const traceId = "0af7651916cd43dd8448eb211c80319c";
  const parentId = "b7ad6b7169203331";
  try {
    const response = await fetch(
      `${gateway.baseUrl}/v1/chat/completions?api_key=never-export-query`,
      {
        method: "POST",
        headers: {
          "content-type": "application/json",
          traceparent: `00-${traceId}-${parentId}-01`,
          tracestate: "localbase=test",
          baggage: "private=never-proxy-baggage",
          "x-request-id": "otel-compiled-request",
        },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "never-export-this-prompt" }],
        }),
      },
    );
    expect(response.status).toBe(200);
    const upstream = gateway.upstreamRequests.find(
      (request) => request.path === "/v1/chat/completions",
    );
    expect(upstream?.headers.get("traceparent")).toStartWith(`00-${traceId}-`);
    expect(upstream?.headers.get("tracestate")).toBe("localbase=test");
    expect(upstream?.headers.has("baggage")).toBe(false);

    const failed = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-test-upstream": "server-error",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "user", content: "failure probe" }],
      }),
    });
    expect(failed.status).toBeGreaterThanOrEqual(500);
  } finally {
    await gateway.stop();
    collector.stop(true);
  }
  const tracePayload = received.find(
    (request) => request.path === "/v1/traces",
  )?.body;
  const logPayload = received.find(
    (request) => request.path === "/v1/logs",
  )?.body;
  expect(tracePayload).toBeDefined();
  expect(logPayload).toBeDefined();
  expect(Buffer.from(tracePayload!).includes(Buffer.from(traceId, "hex"))).toBe(
    true,
  );
  expect(Buffer.from(logPayload!).includes(Buffer.from(traceId, "hex"))).toBe(
    true,
  );
  expect(new TextDecoder().decode(logPayload)).not.toContain(
    "never-export-this-prompt",
  );
  const exportedTraceText = new TextDecoder().decode(tracePayload);
  expect(exportedTraceText).toContain("POST /v1/chat/completions");
  expect(exportedTraceText).not.toContain("never-export-query");
  expect(exportedTraceText).not.toContain("never-proxy-baggage");
  const inferenceError = received
    .filter((request) => request.path === "/v1/traces")
    .flatMap((request) => decodeOtlpTraceSpans(request.body))
    .find(
      (span) =>
        span.name === "localbase.backend.inference" &&
        span.attributes["http.response.status_code"] === 503,
    );
  expect(inferenceError).toMatchObject({
    statusCode: 2,
    attributes: { "http.response.status_code": 503 },
  });
});

describe("API gateway integration", () => {
  let gateway: GatewayFixture;

  beforeAll(
    async () => {
      gateway = await startGatewayFixture();
    },
    { timeout: 30_000 },
  );

  afterAll(
    async () => {
      await gateway?.stop();
    },
    { timeout: 10_000 },
  );

  const request = (path: string, init?: RequestInit) =>
    fetch(`${gateway.baseUrl}${path}`, init);

  async function expectPromiseBlocked(promise: Promise<unknown>) {
    const completed = await Promise.race([
      promise.then(
        () => true,
        () => true,
      ),
      Bun.sleep(100).then(() => false),
    ]);
    expect(completed).toBe(false);
  }

  async function drainReader(
    reader: ReadableStreamDefaultReader<Uint8Array>,
  ): Promise<void> {
    while (!(await reader.read()).done) {}
  }

  async function within<T>(promise: Promise<T>, label: string): Promise<T> {
    return await Promise.race([
      promise,
      Bun.sleep(2_000).then(() => {
        throw new Error(`${label} did not complete within two seconds.`);
      }),
    ]);
  }

  function launchedModelPath(args: string[]): string {
    const modelIndex = args.indexOf("-m");
    if (modelIndex === -1 || !args[modelIndex + 1]) {
      throw new Error(`Runtime launch did not include a model path: ${args}`);
    }
    return args[modelIndex + 1];
  }

  function loadGatewayConfig() {
    return gateway.readConfig();
  }

  function saveGatewayConfig(
    config: ReturnType<GatewayFixture["readConfig"]>,
  ): void {
    gateway.saveConfig(config);
  }

  async function expectValidationFailure(
    path: string,
    init: RequestInit,
    expectedPath: string,
  ): Promise<void> {
    const response = await request(path, init);
    expect(response.status).toBe(400);

    const body = (await response.json()) as {
      error: { code: string; message: string; type: string };
    };
    expect(body.error).toMatchObject({
      type: "invalid_request_error",
      code: "validation_failed",
    });
    expect(body.error.message).toContain(`${expectedPath}:`);
  }

  test("health allows GET and HEAD without starting models", async () => {
    expect(await gateway.readLlmRuntimeLaunches()).toEqual([]);
    const response = await request("/health");
    expect(response.status).toBe(200);
    expect(gatewayHealthSchema.parse(await response.json())).toMatchObject({
      status: "ok",
      modalities: {
        llm: { configured: true, state: "idle" },
        stt: { configured: true, state: "idle" },
        image: { configured: true, state: "idle" },
      },
    });
    const head = await request("/health", { method: "HEAD" });
    expect(head.status).toBe(response.status);
    expect(
      [...head.headers].filter(([name]) => name !== "date").sort(),
    ).toEqual([...response.headers].filter(([name]) => name !== "date").sort());
    expect(await head.text()).toBe("");
    expect(await gateway.readLlmRuntimeLaunches()).toEqual([]);

    const post = await request("/health", { method: "POST" });
    expect(post.status).toBe(405);
    expect(post.headers.get("allow")).toBe("GET, HEAD");
    await expect(post.json()).resolves.toEqual({
      error: {
        message: "Method not allowed.",
        type: "invalid_request_error",
        param: null,
        code: "method_not_allowed",
      },
    });
    expect(
      gatewayHealthSchema.safeParse({
        status: "ok",
        version: "0.1.0",
        uptimeSeconds: 0,
        modalities: {
          llm: { configured: true, state: "idle" },
          stt: { configured: false, state: "disabled" },
          image: { configured: false, state: "disabled" },
        },
        error: "gateway_stopping",
      }).success,
    ).toBe(false);
    expect(
      gatewayHealthSchema.safeParse({
        status: "error",
        version: "0.1.0",
        uptimeSeconds: 0,
        modalities: {
          llm: { configured: true, state: "idle" },
          stt: { configured: false, state: "disabled" },
          image: { configured: false, state: "disabled" },
        },
      }).success,
    ).toBe(false);
  });

  test("rejects a second serve process for the same canonical root", async () => {
    const contender = Bun.spawn(
      [
        gateway.cliPath,
        "serve",
        "--root",
        gateway.root,
        "--host",
        "127.0.0.1",
        "--port",
        "2274",
        "--no-auth",
        "--bypass-memory-check",
      ],
      { stdout: "pipe", stderr: "pipe" },
    );
    const [exitCode, stdout, stderr] = await Promise.all([
      contender.exited,
      new Response(contender.stdout).text(),
      new Response(contender.stderr).text(),
    ]);

    expect(exitCode).toBe(1);
    expect(`${stdout}\n${stderr}`).toContain("already owns");
  });

  test("GET /v1/models lists the configured active model", async () => {
    const response = await request("/v1/models");
    expect(response.status).toBe(200);

    const body = (await response.json()) as {
      object: string;
      data: Array<{
        id: string;
        object: string;
        created: number;
        owned_by: string;
      }>;
    };
    expect(body.object).toBe("list");
    expect(body.data).toContainEqual({
      id: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      object: "model",
      created: 1670000000,
      owned_by: "local-base",
    });
  });

  test("proxies validated chat requests without gateway credentials", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: "Bearer gateway-secret",
        "x-api-key": "gateway-key",
        "x-test-header": "retained",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [
          { role: "developer", content: "hello" },
          {
            role: "user",
            content: [
              { type: "text", text: "inspect this image" },
              {
                type: "image_url",
                image_url: {
                  url: "https://example.test/image.png",
                  detail: "high",
                  provider_option: "preserved",
                },
                provider_part_option: "preserved",
              },
              { type: "file", file: { file_id: "file_123" } },
              {
                type: "file",
                file: {
                  filename: "notes.txt",
                  file_data: "data:text/plain;base64,bm90ZXM=",
                },
                provider_part_option: "preserved",
              },
            ],
          },
        ],
        provider_option: "preserved",
      }),
    });
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      object: "chat.completion",
      choices: [{ message: { content: "ok" } }],
    });

    const upstream = gateway.upstreamRequests.at(-1);
    expect(upstream?.headers.get("authorization")).toBeNull();
    expect(upstream?.headers.get("x-api-key")).toBeNull();
    expect(upstream?.headers.get("x-test-header")).toBe("retained");
    expect(JSON.parse(upstream?.body ?? "{}")).toMatchObject({
      model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      provider_option: "preserved",
      messages: [
        { role: "developer", content: "hello" },
        {
          role: "user",
          content: [
            { type: "text", text: "inspect this image" },
            {
              type: "image_url",
              image_url: {
                url: "https://example.test/image.png",
                detail: "high",
                provider_option: "preserved",
              },
              provider_part_option: "preserved",
            },
            { type: "file", file: { file_id: "file_123" } },
            {
              type: "file",
              file: {
                filename: "notes.txt",
                file_data: "data:text/plain;base64,bm90ZXM=",
              },
              provider_part_option: "preserved",
            },
          ],
        },
      ],
    });
    const health = gatewayHealthSchema.parse(
      await (await request("/health")).json(),
    );
    expect(health.modalities.llm.state).toBe("running");
  });

  test("forwards user-only chat requests without injected instructions", async () => {
    const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: modelId,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    expect(
      JSON.parse(gateway.upstreamRequests.at(-1)?.body ?? "{}").messages,
    ).toEqual([{ role: "user", content: "hello" }]);
  });

  test("rejects unsupported routes without proxying them", async () => {
    const upstreamRequests = gateway.upstreamRequests.length;
    const routes: Array<{ path: string; init?: RequestInit }> = [
      {
        path: "/v1/completions",
        init: {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ model: "unused", prompt: "Raw completion" }),
        },
      },
      { path: "/v1/slots" },
      { path: "/v1/metrics" },
      { path: "/v1/props" },
      { path: "/v1/system_info" },
      { path: "/llm/health" },
      { path: "/stt/health" },
      { path: "/image/health" },
      { path: "/tts/speech" },
      { path: "/video/generations" },
      { path: "/not-a-route" },
    ];

    for (const { path, init } of routes) {
      const response = await request(path, init);
      expect(response.status).toBe(404);
      await expect(response.json()).resolves.toEqual({
        error: {
          message: "This endpoint is not exposed by this gateway.",
          type: "invalid_request_error",
          param: null,
          code: "route_disabled",
        },
      });
    }

    expect(gateway.upstreamRequests).toHaveLength(upstreamRequests);
  });

  test("rejects malformed and unsupported non-streaming upstream responses", async () => {
    for (const mode of [
      "malformed",
      "invalid-schema",
      "custom-tool-response",
      "deprecated-function-call-response",
      "null-refusal-response",
    ]) {
      const response = await request("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-test-upstream": mode,
        },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
        }),
      });
      expect(response.status).toBe(502);
      expect(await response.json()).toMatchObject({
        error: { type: "server_error", code: "upstream_error" },
      });
    }
  });

  test("preserves valid upstream OpenAI errors without upstream headers", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "openai-error",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "user", content: "hello" }],
      }),
    });

    expect(response.status).toBe(429);
    expect(response.headers.get("x-upstream-secret")).toBeNull();
    await expect(response.json()).resolves.toEqual({
      error: {
        message: "Fixture rate limit exceeded.",
        type: "rate_limit_error",
        param: null,
        code: "rate_limit_exceeded",
      },
    });
  });

  test("normalizes malformed upstream error responses", async () => {
    for (const mode of ["malformed-error", "non-json-error"]) {
      const response = await request("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-test-upstream": mode,
        },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
        }),
      });

      expect(response.status).toBe(502);
      expect(response.headers.get("x-upstream-secret")).toBeNull();
      await expect(response.json()).resolves.toEqual({
        error: {
          message: "The upstream service returned an invalid error response.",
          type: "server_error",
          param: null,
          code: "upstream_error",
        },
      });
    }
  });

  test("validates streamed chat events without changing the SSE response", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")?.toLowerCase()).toBe(
      "text/event-stream; charset=utf-8",
    );
    expect(response.headers.get("x-stream-fixture")).toBe("preserved");
    await expect(response.text()).resolves.toContain("[DONE]");
  });

  test("accepts strict llama.cpp reasoning, usage, and tool-call chunks", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "llama-wire-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "Use a tool." }],
      }),
    });
    expect(response.status).toBe(200);
    const stream = await response.text();
    expect(stream).toContain('"reasoning_content":"Considering tools."');
    expect(stream).toContain('"tool_calls"');
    expect(stream).toContain(
      '"id":1234,"token":"weather","bytes":[119,101,97,116,104,101,114]',
    );
    expect(stream).toContain(
      '"id":5678,"token":"forecast","bytes":[102,111,114,101,99,97,115,116]',
    );
    expect(stream).toContain('"choices":[],"usage":{"prompt_tokens":3');
    expect(stream).toContain('"prompt_tokens_details":{"cached_tokens":1}');
    expect(stream).toContain('"timings"');
    expect(stream.endsWith("data: [DONE]\n\n")).toBe(true);
  });

  test("terminates invalid streamed chat events before they reach clients", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "invalid-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    await expect(response.text()).resolves.toBe(STREAM_VALIDATION_FAILURE);
  });

  test("rejects duplicate terminators and events after the terminator", async () => {
    for (const mode of ["duplicate-done-stream", "post-done-stream"]) {
      const response = await request("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-test-upstream": mode,
        },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          stream: true,
          messages: [{ role: "user", content: "hello" }],
        }),
      });
      expect(response.status).toBe(200);
      const stream = await response.text();
      expect(stream).toContain('"finish_reason":"stop"');
      expect(stream.endsWith(STREAM_VALIDATION_FAILURE)).toBe(true);
      expect(stream).not.toContain("too late");
    }
  });

  test("rejects an unterminated done event at end of stream", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "unterminated-done-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    const stream = await response.text();
    expect(stream).toContain('"finish_reason":"stop"');
    expect(stream.endsWith(STREAM_VALIDATION_FAILURE)).toBe(true);
  });

  test("requires every observed choice to finish before done", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "multi-choice-unfinished-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        n: 2,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    const stream = await response.text();
    expect(stream).toContain('"index":0,"delta":{},"finish_reason":"stop"');
    expect(stream).not.toContain(
      '"index":1,"delta":{},"finish_reason":"length"',
    );
    expect(stream.endsWith(STREAM_VALIDATION_FAILURE)).toBe(true);
  });

  test("accepts done after every observed choice has finished", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "multi-choice-complete-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        n: 2,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    const stream = await response.text();
    expect(stream).toContain('"index":0,"delta":{},"finish_reason":"stop"');
    expect(stream).toContain('"index":1,"delta":{},"finish_reason":"length"');
    expect(stream).toContain('"choices":[],"usage"');
    expect(stream.endsWith("data: [DONE]\n\n")).toBe(true);
    expect(stream).not.toContain('"code":"upstream_error"');
  });

  test("rejects additional choice data after that choice has finished", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "post-finish-choice-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    const stream = await response.text();
    expect(stream).toContain('"finish_reason":"stop"');
    expect(stream).not.toContain("too late for this choice");
    expect(stream.endsWith(STREAM_VALIDATION_FAILURE)).toBe(true);
  });

  test("forwards one backend error event and terminates before later events", async () => {
    for (const [mode, code] of [
      ["backend-error-stream", 500],
      ["backend-string-error-stream", "backend_error"],
    ] as const) {
      const response = await request("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-test-upstream": mode,
        },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          stream: true,
          messages: [{ role: "user", content: "hello" }],
        }),
      });
      expect(response.status).toBe(200);
      await expect(response.text()).resolves.toBe(
        `data: ${JSON.stringify({
          error: {
            message: "Backend rejected the request.",
            type: "server_error",
            param: null,
            code,
          },
        })}\n\n`,
      );
    }
  });

  test("does not classify partial media-type matches as event streams", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "invalid-media-type-stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(502);
    await expect(response.json()).resolves.toMatchObject({
      error: { type: "server_error", code: "upstream_error" },
    });
  });

  test("keeps concurrent LLM requests paired with their selected models", async () => {
    const initialConfig = loadGatewayConfig();
    const firstModel = initialConfig.activeLlmModel;
    const secondModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    initialConfig.selectedLlmModels = [firstModel, secondModel];
    saveGatewayConfig(initialConfig);
    await writeCompleteCatalogArtifact(initialConfig.llmModelsDir, secondModel);

    const invalid = await request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: secondModel }),
    });
    expect(invalid.status).toBe(400);
    expect(loadGatewayConfig().activeLlmModel).toBe(
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
    );

    const requestOffset = gateway.upstreamRequests.length;
    const launchOffset = (await gateway.readLlmRuntimeLaunches()).length;
    const eventOffset = (await readLogSnapshot(gateway.root)).length;

    const streamId = "runtime-pairing";
    const firstResponse = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "controlled-stream",
        "x-test-stream-id": streamId,
      },
      body: JSON.stringify({
        model: secondModel,
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    const firstReader = firstResponse.body!.getReader();
    expect((await firstReader.read()).done).toBe(false);
    await gateway.waitForLlmRuntimeLaunches(launchOffset, 1);

    const secondResponse = request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "controlled-stream",
        "x-test-stream-id": "runtime-pairing-second",
      },
      body: JSON.stringify({
        model: firstModel,
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    const thirdResponse = request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: secondModel,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    await Promise.all([
      expectPromiseBlocked(secondResponse),
      expectPromiseBlocked(thirdResponse),
    ]);

    gateway.closeControlledStream(streamId);
    await within(
      drainReader(firstReader),
      "first controlled stream completion",
    );
    const second = await within(secondResponse, "second model response");
    expect(second.status).toBe(200);
    const secondReader = second.body!.getReader();
    expect((await secondReader.read()).done).toBe(false);
    await gateway.waitForLlmRuntimeLaunches(launchOffset, 2);
    await expectPromiseBlocked(thirdResponse);
    expect(loadGatewayConfig().activeLlmModel).toBe(firstModel);

    gateway.closeControlledStream("runtime-pairing-second");
    await within(
      drainReader(secondReader),
      "second controlled stream completion",
    );
    const third = await within(thirdResponse, "third model response");
    expect(third.status).toBe(200);
    await third.text();

    const requests = gateway.upstreamRequests
      .slice(requestOffset)
      .map((upstream) => JSON.parse(upstream.body));
    const requestedModels = [secondModel, firstModel, secondModel];
    expect(requests.map((body) => body.model)).toEqual(requestedModels);
    expect(requests.map((body) => body.messages)).toEqual(
      requests.map(() => [{ role: "user", content: "hello" }]),
    );

    const launches = await gateway.waitForLlmRuntimeLaunches(launchOffset, 3);
    expect(launches.map(launchedModelPath)).toEqual(
      requestedModels.map((modelId) =>
        join(initialConfig.llmModelsDir, modelArtifactFile(modelId)),
      ),
    );
    expect(
      (await readLogSnapshot(gateway.root))
        .slice(eventOffset)
        .filter((event) => event.eventName === "backend.restart-backoff"),
    ).toEqual([]);
  });

  test("holds model switches until streamed LLM responses end", async () => {
    const config = loadGatewayConfig();
    const firstCatalogModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const secondCatalogModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    const modelA = config.activeLlmModel;
    const modelB =
      modelA === firstCatalogModel ? secondCatalogModel : firstCatalogModel;
    saveGatewayConfig({
      ...config,
      selectedLlmModels: [modelA, modelB],
    });
    await writeCompleteCatalogArtifact(config.llmModelsDir, modelB);

    const completeA = await request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: modelA,
        messages: [{ role: "user", content: "complete" }],
      }),
    });
    expect(completeA.status).toBe(200);
    await completeA.text();

    const eofStreamId = "llm-lease-eof";
    const slowA = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "controlled-stream",
        "x-test-stream-id": eofStreamId,
      },
      body: JSON.stringify({
        model: modelA,
        stream: true,
        messages: [{ role: "user", content: "hold" }],
      }),
    });
    const slowAReader = slowA.body!.getReader();
    expect((await slowAReader.read()).done).toBe(false);

    const switchToB = request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: modelB,
        messages: [{ role: "user", content: "switch" }],
      }),
    });
    await expectPromiseBlocked(switchToB);

    gateway.closeControlledStream(eofStreamId);
    await within(drainReader(slowAReader), "EOF stream completion");
    const completeB = await within(switchToB, "switch after EOF");
    expect(completeB.status).toBe(200);
    await completeB.text();
  });

  test("forwards aborts while waiting for rewritten LLM response headers", async () => {
    const config = loadGatewayConfig();
    const firstCatalogModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const secondCatalogModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    const modelA = config.activeLlmModel;
    const modelB =
      modelA === firstCatalogModel ? secondCatalogModel : firstCatalogModel;
    saveGatewayConfig({
      ...config,
      selectedLlmModels: [modelA, modelB],
    });
    await writeCompleteCatalogArtifact(config.llmModelsDir, modelB);

    const headerWaitId = "llm-header-abort";
    const controller = new AbortController();
    const abortedRequest = request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "controlled-headers",
        "x-test-stream-id": headerWaitId,
      },
      body: JSON.stringify({
        model: modelA,
        messages: [{ role: "user", content: "wait" }],
      }),
      signal: controller.signal,
    }).then(
      () => "response",
      () => "aborted",
    );

    await gateway.waitForUpstreamRequest(headerWaitId);
    controller.abort();
    expect(await within(abortedRequest, "header wait abort")).toBe("aborted");
    await within(
      gateway.waitForControlledHeaderAbort(headerWaitId),
      "upstream header wait abort",
    );

    const switched = await within(
      request("/v1/chat/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: modelB,
          messages: [{ role: "user", content: "continue" }],
        }),
      }),
      "model switch after header abort",
    );
    expect(switched.status).toBe(200);
    await switched.text();
  });

  test("abandons queued LLM model switches before dispatch", async () => {
    const config = loadGatewayConfig();
    const firstCatalogModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const secondCatalogModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    const modelA = config.activeLlmModel;
    const modelB =
      modelA === firstCatalogModel ? secondCatalogModel : firstCatalogModel;
    saveGatewayConfig({
      ...config,
      selectedLlmModels: [modelA, modelB],
    });
    await writeCompleteCatalogArtifact(config.llmModelsDir, modelB);

    const completeA = await request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: modelA,
        messages: [{ role: "user", content: "complete" }],
      }),
    });
    expect(completeA.status).toBe(200);
    await completeA.text();

    const streamId = "llm-lease-queued-abort";
    const slowA = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "controlled-stream",
        "x-test-stream-id": streamId,
      },
      body: JSON.stringify({
        model: modelA,
        stream: true,
        messages: [{ role: "user", content: "hold" }],
      }),
    });
    const slowAReader = slowA.body!.getReader();
    expect((await slowAReader.read()).done).toBe(false);

    const requestOffset = gateway.upstreamRequests.length;
    const controller = new AbortController();
    const abandonedRequest = request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: modelB,
        messages: [{ role: "user", content: "switch" }],
      }),
      signal: controller.signal,
    }).then(
      () => "response",
      () => "aborted",
    );
    await expectPromiseBlocked(abandonedRequest);
    controller.abort();
    expect(await within(abandonedRequest, "queued request abort")).toBe(
      "aborted",
    );

    gateway.closeControlledStream(streamId);
    await within(drainReader(slowAReader), "queued abort stream completion");
    await Bun.sleep(100);
    expect(loadGatewayConfig().activeLlmModel).toBe(modelA);
    expect(
      gateway.upstreamRequests
        .slice(requestOffset)
        .some(
          (upstream) =>
            (JSON.parse(upstream.body) as { model?: string }).model === modelB,
        ),
    ).toBe(false);

    const switched = await within(
      request("/v1/chat/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: modelB,
          messages: [{ role: "user", content: "continue" }],
        }),
      }),
      "model switch after queued abort",
    );
    expect(switched.status).toBe(200);
    await switched.text();
  });

  const jsonValidationCases: ValidationCase[] = [
    {
      name: "chat completions require messages",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        }),
      },
      expectedPath: "messages",
    },
    {
      name: "chat completions validate nested message roles",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "invalid-role", content: "hello" }],
        }),
      },
      expectedPath: "messages.0.role",
    },
    {
      name: "tool messages require a tool call id",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "tool", content: "73°F" }],
        }),
      },
      expectedPath: "messages.0.tool_call_id",
    },
    {
      name: "chat completions reject unsupported content parts",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [
            {
              role: "user",
              content: [{ type: "video_url", video_url: { url: "test" } }],
            },
          ],
        }),
      },
      expectedPath: "messages.0.content",
    },
    {
      name: "chat completions reject custom tool declarations",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
          tools: [{ type: "custom", custom: { name: "code_execution" } }],
        }),
      },
      expectedPath: "tools.0.type",
    },
    {
      name: "chat completions require JSON-schema-like tool parameters",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
          tools: [
            {
              type: "function",
              function: { name: "weather", parameters: "not-a-schema" },
            },
          ],
        }),
      },
      expectedPath: "tools.0.function.parameters",
    },
    {
      name: "chat completions require a complete JSON-schema response format",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
          response_format: {
            type: "json_schema",
            json_schema: { name: "answer" },
          },
        }),
      },
      expectedPath: "response_format.json_schema.schema",
    },
    {
      name: "chat completions reject invalid JSON-schema response formats",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
          response_format: {
            type: "json_schema",
            json_schema: {
              name: "answer",
              schema: "not-a-schema",
            },
          },
        }),
      },
      expectedPath: "response_format.json_schema.schema",
    },
    {
      name: "chat completions reject unsupported tool choices",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
          tool_choice: {
            type: "allowed_tools",
            allowed_tools: ["weather"],
          },
        }),
      },
      expectedPath: "tool_choice",
    },
    {
      name: "chat completions reject arbitrary tool choices",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "user", content: "hello" }],
          tool_choice: "force",
        }),
      },
      expectedPath: "tool_choice",
    },
    {
      name: "chat completions reject custom tool calls",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [
            {
              role: "assistant",
              content: null,
              tool_calls: [
                {
                  id: "custom_1",
                  type: "custom",
                  custom: { name: "code_execution" },
                },
              ],
            },
          ],
        }),
      },
      expectedPath: "messages.0.tool_calls.0.type",
    },
    {
      name: "chat completions require complete file payloads",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [
            {
              role: "user",
              content: [{ type: "file", file: { filename: "notes.txt" } }],
            },
          ],
        }),
      },
      expectedPath: "messages.0.content",
    },
    {
      name: "chat completions reject ambiguous file payloads",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [
            {
              role: "user",
              content: [
                {
                  type: "file",
                  file: {
                    file_id: "file_123",
                    filename: "notes.txt",
                    file_data: "data:text/plain;base64,bm90ZXM=",
                  },
                },
              ],
            },
          ],
        }),
      },
      expectedPath: "messages.0.content",
    },
    {
      name: "chat completions reject deprecated assistant function calls",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [
            {
              role: "assistant",
              content: null,
              function_call: { name: "weather", arguments: "{}" },
            },
          ],
        }),
      },
      expectedPath: "messages.0.function_call",
    },
    {
      name: "chat completions require assistant content, tool calls, or refusal",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "assistant", content: null, refusal: null }],
        }),
      },
      expectedPath: "messages.0",
    },
    {
      name: "chat completions require nonempty assistant tool calls",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "assistant", content: null, tool_calls: [] }],
        }),
      },
      expectedPath: "messages.0",
    },
    {
      name: "image generations require a prompt",
      path: "/v1/images/generations",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ n: 1 }),
      },
      expectedPath: "prompt",
    },
    {
      name: "embeddings require input",
      path: "/v1/embeddings",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: "embeddings-model" }),
      },
      expectedPath: "input",
    },
  ];

  for (const validationCase of jsonValidationCases) {
    test(validationCase.name, async () => {
      await expectValidationFailure(
        validationCase.path,
        validationCase.init,
        validationCase.expectedPath,
      );
    });
  }

  test("audio transcriptions require a multipart file", async () => {
    const formData = new FormData();
    formData.append("model", "whisper-large-v3-turbo");

    await expectValidationFailure(
      "/v1/audio/transcriptions",
      { method: "POST", body: formData },
      "file",
    );
  });

  test("audio transcriptions reject repeated scalar multipart fields", async () => {
    const formData = new FormData();
    formData.append("file", new Blob(["audio"]), "audio.wav");
    formData.append("model", "whisper-a");
    formData.append("model", "whisper-b");

    await expectValidationFailure(
      "/v1/audio/transcriptions",
      { method: "POST", body: formData },
      "model",
    );
  });
});
