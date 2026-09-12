import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { latestUpstreamRequestBody } from "../../test/ai-sdk-conformance";
import {
  startGatewayFixture,
  type GatewayFixture,
  waitForLogEvent,
} from "../../test/gateway-fixture";
import { readLogSnapshot } from "../observability/logging";
import { openAIErrorResponseSchema } from "./openai-error";

const MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";

const responseFormat = {
  type: "json_schema",
  json_schema: {
    name: "never_log_structured_schema",
    description: "never-log-structured-description",
    strict: true,
    schema: {
      $schema: "http://json-schema.org/draft-07/schema#",
      type: "object",
      properties: {
        answer: { $ref: "#/$defs/answer" },
      },
      required: ["answer"],
      additionalProperties: false,
      $defs: {
        answer: { type: "string", maxLength: 20 },
      },
    },
  },
};

function chatRequest(
  gateway: GatewayFixture,
  mode: string,
  body: Record<string, unknown>,
  signal?: AbortSignal,
): Promise<Response> {
  return fetch(`${gateway.baseUrl}/v1/chat/completions`, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-test-upstream": mode,
      ...(mode === "controlled-stream"
        ? { "x-test-stream-id": "structured-output-abort" }
        : {}),
    },
    body: JSON.stringify({
      model: MODEL,
      messages: [
        { role: "user", content: "never-log-structured-request-content" },
      ],
      ...body,
    }),
    signal,
  });
}

function responseRequestId(response: Response): string {
  const requestId = response.headers.get("x-localbase-request-id");
  if (!requestId) throw new Error("Gateway response omitted its request ID.");
  expect(requestId).toMatch(/^lbreq_/);
  return requestId;
}

describe.serial("native JSON Schema structured output conformance", () => {
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

  test("rejects invalid and unsupported schemas before launching a model", async () => {
    expect(await gateway.readLlmRuntimeLaunches()).toHaveLength(0);
    const upstreamOffset = gateway.upstreamRequests.length;

    const invalid = await chatRequest(gateway, "structured-json", {
      response_format: {
        type: "json_schema",
        json_schema: {
          name: "missing_ref",
          strict: true,
          schema: {
            type: "object",
            properties: { answer: { $ref: "#/$defs/missing" } },
            required: ["answer"],
            additionalProperties: false,
          },
        },
      },
    });
    expect(invalid.status).toBe(400);
    expect(openAIErrorResponseSchema.parse(await invalid.json()).error).toEqual(
      expect.objectContaining({
        param: "response_format.json_schema.schema",
        code: "invalid_json_schema",
      }),
    );

    const unsupported = await chatRequest(gateway, "structured-json", {
      response_format: {
        type: "json_schema",
        json_schema: {
          name: "unsupported_pattern",
          strict: true,
          schema: {
            type: "object",
            properties: {
              answer: { type: "string", pattern: "^yes$" },
            },
            required: ["answer"],
            additionalProperties: false,
          },
        },
      },
    });
    expect(unsupported.status).toBe(400);
    expect(
      openAIErrorResponseSchema.parse(await unsupported.json()).error,
    ).toEqual(
      expect.objectContaining({
        param: "response_format.json_schema.schema",
        code: "unsupported_json_schema",
        message: expect.stringContaining("unsupported keyword 'pattern'"),
      }),
    );

    expect(await gateway.readLlmRuntimeLaunches()).toHaveLength(0);
    expect(gateway.upstreamRequests).toHaveLength(upstreamOffset);
  });

  test("forwards a supported local-ref schema unchanged and accepts valid output", async () => {
    const response = await chatRequest(gateway, "structured-json", {
      response_format: responseFormat,
    });

    expect(response.status).toBe(200);
    const requestId = responseRequestId(response);
    expect(await response.json()).toMatchObject({
      choices: [
        {
          message: { content: '{"answer":"hello"}' },
          finish_reason: "stop",
        },
      ],
    });
    expect(latestUpstreamRequestBody(gateway).response_format).toEqual(
      responseFormat,
    );
    const event = await waitForLogEvent(
      gateway,
      (candidate) =>
        candidate.eventName === "inference.completed" &&
        candidate.requestId === requestId,
    );
    const preparationMs = event.attributes?.json_schema_preparation_ms;
    if (typeof preparationMs !== "number") {
      throw new Error("Inference event omitted JSON Schema preparation time.");
    }
    expect(event.attributes).toMatchObject({
      json_schema_requested: true,
      json_schema_native_mode: "requested",
      json_schema_preparation_ms: expect.any(Number),
      json_schema_validation: "passed",
    });
    expect(preparationMs).toBeGreaterThanOrEqual(0);
    const serializedEvents = JSON.stringify(
      await readLogSnapshot(gateway.root),
    );
    for (const privateValue of [
      "never_log_structured_schema",
      "never-log-structured-description",
      "never-log-structured-request-content",
    ]) {
      expect(serializedEvents).not.toContain(privateValue);
    }
  });

  test("returns a dedicated 502 when completed content misses the schema", async () => {
    const response = await chatRequest(gateway, "structured-invalid-schema", {
      response_format: responseFormat,
    });

    expect(response.status).toBe(502);
    const requestId = responseRequestId(response);
    expect(
      openAIErrorResponseSchema.parse(await response.json()).error,
    ).toEqual(
      expect.objectContaining({
        type: "server_error",
        code: "structured_output_validation_failed",
      }),
    );
    const event = await waitForLogEvent(
      gateway,
      (candidate) =>
        candidate.eventName === "inference.completed" &&
        candidate.requestId === requestId,
    );
    expect(event.attributes).toMatchObject({
      json_schema_native_mode: "requested",
      json_schema_validation: "failed",
      outcome: "error",
    });
    await Bun.sleep(25);
    expect(
      (await readLogSnapshot(gateway.root)).filter(
        (candidate) =>
          candidate.eventName === "inference.completed" &&
          candidate.requestId === requestId,
      ),
    ).toHaveLength(1);
  });

  test("preserves refusals, truncation, and tool calls outside final validation", async () => {
    const cases = [
      {
        mode: "structured-refusal",
        skipReason: "refusal",
        expected: {
          message: { refusal: "I cannot help with that request." },
          finish_reason: "stop",
        },
      },
      {
        mode: "structured-length",
        skipReason: "truncation",
        expected: { message: { content: "{" }, finish_reason: "length" },
      },
      {
        mode: "structured-tool-call",
        skipReason: "tool_calls",
        expected: {
          message: {
            tool_calls: [
              {
                type: "function",
                function: { name: "weather" },
              },
            ],
          },
          finish_reason: "tool_calls",
        },
      },
    ];

    for (const { mode, expected, skipReason } of cases) {
      const response = await chatRequest(gateway, mode, {
        response_format: responseFormat,
      });
      expect(response.status).toBe(200);
      const requestId = responseRequestId(response);
      expect(await response.json()).toMatchObject({ choices: [expected] });
      expect(
        (
          await waitForLogEvent(
            gateway,
            (candidate) =>
              candidate.eventName === "inference.completed" &&
              candidate.requestId === requestId,
          )
        ).attributes,
      ).toMatchObject({
        json_schema_validation: "skipped",
        json_schema_skip_reasons: skipReason,
      });
    }
  });

  test("streams incrementally and propagates client abort without final buffering", async () => {
    const controller = new AbortController();
    const response = await chatRequest(
      gateway,
      "controlled-stream",
      { response_format: responseFormat, stream: true },
      controller.signal,
    );

    expect(response.status).toBe(200);
    const requestId = responseRequestId(response);
    const first = await response.body?.getReader().read();
    expect(new TextDecoder().decode(first?.value)).toContain("waiting");
    controller.abort();
    await gateway.waitForControlledStreamAbort("structured-output-abort");
    expect(
      (
        await waitForLogEvent(
          gateway,
          (candidate) =>
            candidate.eventName === "inference.completed" &&
            candidate.requestId === requestId,
        )
      ).attributes,
    ).toMatchObject({
      json_schema_native_mode: "requested",
      json_schema_validation: "not_performed_streaming",
    });
  });
});
