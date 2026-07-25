import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createOpenAICompatible } from "@ai-sdk/openai-compatible";
import { generateText, stepCountIs, tool } from "ai";
import { join } from "node:path";
import { z } from "zod";
import { byId, primaryArtifact } from "../../catalog";
import {
  startGatewayFixture,
  type GatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";
import { httpBaseUrl, withResponseLease } from "./commands/serve";

type ValidationCase = {
  name: string;
  path: string;
  init: RequestInit;
  expectedPath: string;
};

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

  test("GET /health reports the enabled modalities", async () => {
    const response = await request("/health");
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      status: "ok",
      enabled: { llm: true, stt: true, image: true },
    });
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

  test("rejects every removed text-completions route", async () => {
    for (const path of [
      "/v1/completions",
      "/v1/completions/",
      "/v1/completions/legacy",
    ]) {
      const response = await request(path, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: "unused", prompt: "Raw completion" }),
      });
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
  });

  test("round-trips AI SDK tool calls and results", async () => {
    const localbase = createOpenAICompatible({
      baseURL: `${gateway.baseUrl}/v1`,
      name: "localbase-test",
    });

    const rawResponse = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "tool-round-trip",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "user", content: "Call the weather tool." }],
      }),
    });
    expect(rawResponse.status).toBe(200);
    const rawBody = (await rawResponse.json()) as {
      choices: Array<{ message: unknown }>;
    };
    expect(rawBody.choices[0]?.message).toEqual({
      role: "assistant",
      content: null,
      reasoning_content: "Looking up the weather.",
      tool_calls: [
        {
          id: "call_weather",
          type: "function",
          function: {
            name: "weather",
            arguments: '{"city":"Austin"}',
          },
          extra_content: {
            google: { thought_signature: "signature" },
          },
        },
      ],
    });

    const requestOffset = gateway.upstreamRequests.length;

    const result = await generateText({
      model: localbase.chatModel("qwen2.5-coder-1.5b-instruct-q4_k_m"),
      headers: { "x-test-upstream": "tool-round-trip" },
      prompt: "What is the weather in Austin?",
      tools: {
        weather: tool({
          description: "Gets current weather by city.",
          inputSchema: z.object({ city: z.string() }),
          execute: async ({ city }) => ({ city, temperature: 73 }),
        }),
      },
      stopWhen: stepCountIs(2),
    });

    expect(result.text).toBe("73°F");
    const requests = gateway.upstreamRequests
      .slice(requestOffset)
      .map((upstream) => JSON.parse(upstream.body));
    expect(requests).toHaveLength(2);
    expect(requests[0].tools).toEqual([
      expect.objectContaining({
        type: "function",
        function: expect.objectContaining({ name: "weather" }),
      }),
    ]);
    expect(requests[1].messages).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          role: "assistant",
          content: null,
          tool_calls: [
            expect.objectContaining({
              id: "call_weather",
              type: "function",
              function: {
                name: "weather",
                arguments: '{"city":"Austin"}',
              },
            }),
          ],
        }),
        expect.objectContaining({
          role: "tool",
          tool_call_id: "call_weather",
          content: '{"city":"Austin","temperature":73}',
        }),
      ]),
    );
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

  test("passes SSE responses through without buffering or schema gating", async () => {
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
    expect(response.headers.get("content-type")).toContain("text/event-stream");
    expect(await response.text()).toContain("[DONE]");
  });

  test("rejects removed raw backend namespaces", async () => {
    for (const pathname of ["/llm/health", "/stt/health", "/image/health"]) {
      const response = await request(pathname);
      expect(response.status).toBe(404);
    }
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
    expect((await firstReader.read()).done).toBe(true);
    const second = await within(secondResponse, "second model response");
    expect(second.status).toBe(200);
    const secondReader = second.body!.getReader();
    expect((await secondReader.read()).done).toBe(false);
    await gateway.waitForLlmRuntimeLaunches(launchOffset, 2);
    await expectPromiseBlocked(thirdResponse);
    expect(loadGatewayConfig().activeLlmModel).toBe(firstModel);

    gateway.closeControlledStream("runtime-pairing-second");
    expect((await secondReader.read()).done).toBe(true);
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
    expect(
      (await within(slowAReader.read(), "EOF stream completion")).done,
    ).toBe(true);
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
    expect(
      (await within(slowAReader.read(), "queued abort stream completion")).done,
    ).toBe(true);
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
