import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { generateText, streamText } from "ai";
import { join } from "node:path";
import { byId, primaryArtifact } from "../../catalog";
import {
  createLocalBaseAiSdkProvider,
  latestUpstreamRequestBody,
} from "../../test/ai-sdk-conformance";
import {
  startGatewayFixture,
  type GatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";

const PRIMARY_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const SWITCHED_MODEL = "qwen2.5-coder-7b-instruct-q4_k_m";

function modelArtifactPath(modelsDir: string, modelId: string): string {
  const model = byId(modelId);
  if (!model) throw new Error(`Unknown catalog model: ${modelId}`);
  return join(modelsDir, primaryArtifact(model).filename);
}

describe("Vercel AI SDK language conformance", () => {
  let gateway: GatewayFixture;
  let authenticatedGateway: GatewayFixture;

  beforeAll(
    async () => {
      [gateway, authenticatedGateway] = await Promise.all([
        startGatewayFixture(),
        startGatewayFixture({ auth: { mode: "bearer" } }),
      ]);
    },
    { timeout: 30_000 },
  );

  afterAll(
    async () => {
      await Promise.all([gateway?.stop(), authenticatedGateway?.stop()]);
    },
    { timeout: 10_000 },
  );

  test("generateText forwards prompt, messages, and client instructions exactly", async () => {
    const localbase = createLocalBaseAiSdkProvider(gateway);
    const promptResult = await generateText({
      model: localbase.chatModel(PRIMARY_MODEL),
      system: "SDK system instruction",
      prompt: "Prompt input",
    });
    expect(promptResult.text).toBe("ok");
    expect(promptResult.finishReason).toBe("stop");
    expect(promptResult.usage).toMatchObject({
      inputTokens: 3,
      outputTokens: 2,
      totalTokens: 5,
    });
    expect(latestUpstreamRequestBody(gateway).messages).toEqual([
      { role: "system", content: "SDK system instruction" },
      { role: "user", content: "Prompt input" },
    ]);

    const messagesResult = await generateText({
      model: localbase.chatModel(PRIMARY_MODEL),
      messages: [{ role: "user", content: "Message input" }],
    });
    expect(messagesResult.text).toBe("ok");
    expect(latestUpstreamRequestBody(gateway).messages).toEqual([
      { role: "user", content: "Message input" },
    ]);

    const developerResponse = await fetch(
      `${gateway.baseUrl}/v1/chat/completions`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: PRIMARY_MODEL,
          messages: [
            {
              role: "system",
              content: [
                {
                  type: "text",
                  text: "Client system",
                  cache_control: { type: "ephemeral" },
                },
              ],
            },
            {
              role: "developer",
              content: [{ type: "text", text: "Client developer" }],
            },
            { role: "user", content: "Client user" },
          ],
        }),
      },
    );
    expect(developerResponse.status).toBe(200);
    expect(latestUpstreamRequestBody(gateway).messages).toEqual([
      {
        role: "system",
        content: [
          {
            type: "text",
            text: "Client system",
            cache_control: { type: "ephemeral" },
          },
        ],
      },
      {
        role: "developer",
        content: [{ type: "text", text: "Client developer" }],
      },
      { role: "user", content: "Client user" },
    ]);
  });

  test("streamText assembles OpenAI SSE chunks with usage and finish metadata", async () => {
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
      prompt: "Stream input",
      headers: { "x-test-upstream": "ai-sdk-stream" },
    });

    expect(await result.text).toBe("hello");
    expect(await result.finishReason).toBe("stop");
    expect(await result.usage).toMatchObject({
      inputTokens: 3,
      outputTokens: 2,
      totalTokens: 5,
    });
    expect(latestUpstreamRequestBody(gateway)).toMatchObject({
      model: PRIMARY_MODEL,
      stream: true,
      stream_options: { include_usage: true },
      messages: [{ role: "user", content: "Stream input" }],
    });

    const messagesResult = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
      messages: [{ role: "user", content: "Stream message input" }],
      headers: { "x-test-upstream": "ai-sdk-stream" },
    });
    expect(await messagesResult.text).toBe("hello");
    expect(latestUpstreamRequestBody(gateway).messages).toEqual([
      { role: "user", content: "Stream message input" },
    ]);
  });

  test("propagates AI SDK stream cancellation to the upstream connection", async () => {
    const controller = new AbortController();
    const streamId = "ai-sdk-abort";
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
      prompt: "Wait for cancellation",
      headers: {
        "x-test-upstream": "ai-sdk-controlled-stream",
        "x-test-stream-id": streamId,
      },
      abortSignal: controller.signal,
    });
    const text = Promise.resolve(result.text);

    await gateway.waitForUpstreamRequest(streamId);
    controller.abort();
    await expect(text).rejects.toBeDefined();
    await gateway.waitForControlledStreamAbort(streamId);
  });

  test("surfaces a truncated upstream stream through the AI SDK", async () => {
    const errors: unknown[] = [];
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
      prompt: "Reject a stream without its terminator.",
      headers: { "x-test-upstream": "truncated-ai-sdk-stream" },
      onError: ({ error }) => {
        errors.push(error);
      },
    });

    let received = "";
    for await (const text of result.textStream) received += text;
    expect(received).toBe("partial");
    expect(errors).toEqual([
      {
        message: "The upstream service returned an invalid event stream.",
        type: "server_error",
        param: null,
        code: "upstream_error",
      },
    ]);
  });

  test("rejects a completed stream without a terminal finish reason", async () => {
    const errors: unknown[] = [];
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
      prompt: "Reject a stream without a finish reason.",
      headers: { "x-test-upstream": "missing-finish-stream" },
      onError: ({ error }) => {
        errors.push(error);
      },
    });

    let received = "";
    for await (const text of result.textStream) received += text;
    expect(received).toBe("partial");
    expect(errors).toEqual([
      {
        message: "The upstream service returned an invalid event stream.",
        type: "server_error",
        param: null,
        code: "upstream_error",
      },
    ]);
  });

  test("reports one backend stream error without requiring a terminator", async () => {
    const errors: unknown[] = [];
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
      prompt: "Surface one backend error.",
      headers: { "x-test-upstream": "backend-error-stream" },
      onError: ({ error }) => {
        errors.push(error);
      },
    });

    let received = "";
    for await (const text of result.textStream) received += text;
    expect(received).toBe("");
    expect(errors).toEqual([
      {
        message: "Backend rejected the request.",
        type: "server_error",
        param: null,
        code: 500,
      },
    ]);
  });

  test("uses exact model IDs to select the requested local model", async () => {
    const config = gateway.readConfig();
    gateway.saveConfig({
      ...config,
      selectedLlmModels: [PRIMARY_MODEL, SWITCHED_MODEL],
    });
    await writeCompleteCatalogArtifact(config.llmModelsDir, SWITCHED_MODEL);

    const localbase = createLocalBaseAiSdkProvider(gateway);
    const canonical = await generateText({
      model: localbase.chatModel(PRIMARY_MODEL),
      prompt: "Use the canonical model ID",
    });
    expect(canonical.text).toBe("ok");

    const launchOffset = (await gateway.readLlmRuntimeLaunches()).length;
    const selected = await generateText({
      model: localbase.chatModel(SWITCHED_MODEL),
      prompt: "Use the selected model ID",
    });
    expect(selected.text).toBe("ok");
    expect(gateway.readConfig().activeLlmModel).toBe(SWITCHED_MODEL);
    expect(latestUpstreamRequestBody(gateway)).toMatchObject({
      model: SWITCHED_MODEL,
    });

    const launches = await gateway.waitForLlmRuntimeLaunches(launchOffset, 1);
    const modelArgument = launches[0]?.indexOf("-m");
    expect(modelArgument).toBeGreaterThanOrEqual(0);
    expect(launches[0]?.[modelArgument! + 1]).toBe(
      modelArtifactPath(config.llmModelsDir, SWITCHED_MODEL),
    );
  });

  test("rejects blank and unknown model IDs before dispatching to the backend", async () => {
    const requestOffset = gateway.upstreamRequests.length;
    const launchOffset = (await gateway.readLlmRuntimeLaunches()).length;

    for (const model of ["", "   "]) {
      const response = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model,
          messages: [{ role: "user", content: "Do not dispatch this." }],
        }),
      });
      expect(response.status).toBe(400);
      await expect(response.json()).resolves.toMatchObject({
        error: {
          type: "invalid_request_error",
          param: null,
          code: "validation_failed",
        },
      });
    }

    for (const model of [
      "definitely-not-selected",
      ` ${PRIMARY_MODEL} `,
      PRIMARY_MODEL.toUpperCase(),
      ...["localbase", "openai", "ollama"].map(
        (provider) => `${provider}/${PRIMARY_MODEL}`,
      ),
    ]) {
      const response = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model,
          messages: [{ role: "user", content: "Do not dispatch this." }],
        }),
      });

      expect(response.status).toBe(404);
      await expect(response.json()).resolves.toEqual({
        error: {
          message: `The model '${model}' does not exist.`,
          type: "invalid_request_error",
          param: "model",
          code: "model_not_found",
        },
      });
    }
    expect(gateway.upstreamRequests.slice(requestOffset)).toEqual([]);
    expect(
      (await gateway.readLlmRuntimeLaunches()).slice(launchOffset),
    ).toEqual([]);
  });

  test("maps gateway authentication, validation, and upstream failures to OpenAI-shaped errors", async () => {
    const unauthenticated = createLocalBaseAiSdkProvider(authenticatedGateway);
    await expect(
      generateText({
        model: unauthenticated.chatModel(PRIMARY_MODEL),
        prompt: "Unauthorized request",
        maxRetries: 0,
      }),
    ).rejects.toMatchObject({ statusCode: 401 });

    const missingKeyResponse = await fetch(
      `${authenticatedGateway.baseUrl}/v1/chat/completions`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: PRIMARY_MODEL,
          messages: [{ role: "user", content: "Missing key" }],
        }),
      },
    );
    expect(missingKeyResponse.status).toBe(401);
    expect(await missingKeyResponse.json()).toMatchObject({
      error: {
        type: "invalid_request_error",
        param: null,
        code: "invalid_api_key",
      },
    });

    if (!authenticatedGateway.apiKey) {
      throw new Error("Authenticated gateway did not provide a test API key.");
    }
    const authenticated = await generateText({
      model: createLocalBaseAiSdkProvider(
        authenticatedGateway,
        authenticatedGateway.apiKey,
      ).chatModel(PRIMARY_MODEL),
      prompt: "Authorized request",
    });
    expect(authenticated.text).toBe("ok");
    expect(
      authenticatedGateway.upstreamRequests
        .at(-1)
        ?.headers.get("authorization"),
    ).toBeNull();

    const validationResponse = await fetch(
      `${gateway.baseUrl}/v1/chat/completions`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: PRIMARY_MODEL }),
      },
    );
    expect(validationResponse.status).toBe(400);
    expect(await validationResponse.json()).toMatchObject({
      error: {
        type: "invalid_request_error",
        param: null,
        code: "validation_failed",
      },
    });

    await expect(
      generateText({
        model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_MODEL),
        prompt: "Malformed upstream response",
        headers: { "x-test-upstream": "malformed" },
        maxRetries: 0,
      }),
    ).rejects.toMatchObject({ statusCode: 502 });
  });
});
