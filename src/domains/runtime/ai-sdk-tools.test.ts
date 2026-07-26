import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  generateText,
  InvalidToolInputError,
  NoObjectGeneratedError,
  NoSuchToolError,
  Output,
  stepCountIs,
  streamText,
  tool,
} from "ai";
import { z } from "zod";
import {
  createLocalBaseAiSdkProvider,
  latestUpstreamRequestBody,
} from "../../test/ai-sdk-conformance";
import {
  startGatewayFixture,
  type GatewayFixture,
} from "../../test/gateway-fixture";

const MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";

function upstreamBodies(
  gateway: GatewayFixture,
  offset: number,
): Array<Record<string, unknown>> {
  return gateway.upstreamRequests
    .slice(offset)
    .map((request) => JSON.parse(request.body) as Record<string, unknown>);
}

function weatherTool(execute?: (input: { city: string }) => unknown) {
  return tool({
    description: "Gets the current weather for a city.",
    inputSchema: z.object({ city: z.string().min(1) }),
    strict: true,
    execute: async (input) => execute?.(input) ?? { temperature: 73 },
  });
}

describe("Vercel AI SDK tools and structured output conformance", () => {
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

  test("forwards strict tool schemas and every supported tool choice", async () => {
    const localbase = createLocalBaseAiSdkProvider(gateway);
    const tools = {
      weather: tool({
        description: "Gets a forecast for a city and number of days.",
        inputSchema: z.object({
          city: z.string().min(1).describe("City name"),
          days: z.number().int().min(1).max(7),
        }),
        strict: true,
      }),
    };
    const choices = [
      { choice: "auto" as const, expected: "auto" },
      { choice: "none" as const, expected: "none" },
      { choice: "required" as const, expected: "required" },
      {
        choice: { type: "tool" as const, toolName: "weather" as const },
        expected: { type: "function", function: { name: "weather" } },
      },
    ];
    const offset = gateway.upstreamRequests.length;

    for (const { choice } of choices) {
      await generateText({
        model: localbase.chatModel(MODEL),
        prompt: "Choose a weather tool.",
        tools,
        toolChoice: choice,
      });
    }

    const requests = upstreamBodies(gateway, offset);
    expect(requests).toHaveLength(choices.length);
    expect(requests.map((request) => request.tool_choice)).toEqual(
      choices.map(({ expected }) => expected),
    );
    expect(requests[0]?.tools).toEqual([
      expect.objectContaining({
        type: "function",
        function: expect.objectContaining({
          name: "weather",
          description: "Gets a forecast for a city and number of days.",
          strict: true,
          parameters: expect.objectContaining({
            type: "object",
            properties: expect.objectContaining({
              city: expect.objectContaining({
                type: "string",
                description: "City name",
              }),
              days: expect.objectContaining({ type: "integer" }),
            }),
            required: ["city", "days"],
          }),
        }),
      }),
    ]);
  });

  test("executes a two-step function call and returns its result", async () => {
    const calls: string[] = [];
    const offset = gateway.upstreamRequests.length;
    const result = await generateText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(MODEL),
      headers: { "x-test-upstream": "tool-round-trip" },
      prompt: "What is the weather in Austin?",
      tools: {
        weather: weatherTool(({ city }) => {
          calls.push(city);
          return { city, temperature: 73 };
        }),
      },
      stopWhen: stepCountIs(2),
    });

    expect(result.text).toBe("73°F");
    expect(calls).toEqual(["Austin"]);
    const requests = upstreamBodies(gateway, offset);
    expect(requests).toHaveLength(2);
    expect(requests[1]?.messages).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          role: "assistant",
          tool_calls: [
            expect.objectContaining({
              id: "call_weather",
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

  test("executes every function in a multiple-call response", async () => {
    const calls: string[] = [];
    const offset = gateway.upstreamRequests.length;
    const result = await generateText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(MODEL),
      headers: { "x-test-upstream": "multiple-tool-round-trip" },
      prompt: "Get Austin weather and Chicago time.",
      tools: {
        weather: weatherTool(({ city }) => {
          calls.push(`weather:${city}`);
          return { temperature: 73 };
        }),
        time: tool({
          description: "Gets the local time.",
          inputSchema: z.object({ timezone: z.string() }),
          execute: async ({ timezone }) => {
            calls.push(`time:${timezone}`);
            return { time: "noon" };
          },
        }),
      },
      stopWhen: stepCountIs(2),
    });

    expect(result.text).toBe("73°F at noon");
    expect(calls.sort()).toEqual(["time:America/Chicago", "weather:Austin"]);
    const requests = upstreamBodies(gateway, offset);
    expect(requests).toHaveLength(2);
    expect(requests[1]?.messages).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ role: "tool", tool_call_id: "call_weather" }),
        expect.objectContaining({ role: "tool", tool_call_id: "call_time" }),
      ]),
    );
  });

  test("marks malformed, schema-invalid, and unknown tool calls invalid", async () => {
    let executions = 0;
    const localbase = createLocalBaseAiSdkProvider(gateway);
    const malformed = await generateText({
      model: localbase.chatModel(MODEL),
      headers: { "x-test-upstream": "malformed-tool-input" },
      prompt: "Call weather.",
      tools: {
        weather: weatherTool(() => {
          executions += 1;
        }),
      },
    });
    const unknown = await generateText({
      model: localbase.chatModel(MODEL),
      headers: { "x-test-upstream": "unknown-tool" },
      prompt: "Call a tool.",
      tools: {
        weather: weatherTool(() => {
          executions += 1;
        }),
      },
    });
    const schemaInvalid = await generateText({
      model: localbase.chatModel(MODEL),
      headers: { "x-test-upstream": "schema-invalid-tool-input" },
      prompt: "Call weather.",
      tools: {
        weather: weatherTool(() => {
          executions += 1;
        }),
      },
    });

    expect(executions).toBe(0);
    expect(malformed.toolCalls[0]).toMatchObject({
      toolName: "weather",
      invalid: true,
    });
    expect(
      InvalidToolInputError.isInstance(
        (malformed.toolCalls[0] as { error?: unknown }).error,
      ),
    ).toBe(true);
    expect(schemaInvalid.toolCalls[0]).toMatchObject({
      toolName: "weather",
      invalid: true,
    });
    expect(
      InvalidToolInputError.isInstance(
        (schemaInvalid.toolCalls[0] as { error?: unknown }).error,
      ),
    ).toBe(true);
    expect(unknown.toolCalls[0]).toMatchObject({
      toolName: "unknown",
      invalid: true,
    });
    expect(
      NoSuchToolError.isInstance(
        (unknown.toolCalls[0] as { error?: unknown }).error,
      ),
    ).toBe(true);
  });

  test("returns a tool-error content part when an executor throws", async () => {
    const result = await generateText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(MODEL),
      headers: { "x-test-upstream": "tool-execution-error" },
      prompt: "Call weather.",
      tools: {
        weather: weatherTool(() => {
          throw new Error("Weather service failed.");
        }),
      },
    });

    expect(result.content).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          type: "tool-error",
          toolCallId: "call_weather",
          toolName: "weather",
          error: expect.objectContaining({
            message: "Weather service failed.",
          }),
        }),
      ]),
    );
  });

  test("uses OpenAI JSON and JSON Schema response formats", async () => {
    const localbase = createLocalBaseAiSdkProvider(gateway, undefined, {
      supportsStructuredOutputs: true,
    });
    const jsonResult = await generateText({
      model: localbase.chatModel(MODEL),
      headers: { "x-test-upstream": "structured-json" },
      prompt: "Return JSON.",
      output: Output.json(),
    });
    expect(jsonResult.output).toEqual({ answer: "hello" });
    expect(latestUpstreamRequestBody(gateway).response_format).toEqual({
      type: "json_object",
    });

    const objectResult = await generateText({
      model: localbase.chatModel(MODEL),
      headers: { "x-test-upstream": "structured-json" },
      prompt: "Return an answer.",
      output: Output.object({
        name: "answer",
        description: "A concise answer.",
        schema: z.object({ answer: z.string() }),
      }),
    });
    expect(objectResult.output).toEqual({ answer: "hello" });
    expect(latestUpstreamRequestBody(gateway).response_format).toEqual({
      type: "json_schema",
      json_schema: expect.objectContaining({
        name: "answer",
        description: "A concise answer.",
        strict: true,
        schema: expect.objectContaining({
          type: "object",
          properties: expect.objectContaining({
            answer: expect.objectContaining({ type: "string" }),
          }),
          required: ["answer"],
        }),
      }),
    });
  });

  test("rejects malformed and schema-invalid structured output", async () => {
    const output = Output.object({ schema: z.object({ answer: z.string() }) });
    const model = createLocalBaseAiSdkProvider(gateway, undefined, {
      supportsStructuredOutputs: true,
    }).chatModel(MODEL);

    for (const mode of [
      "structured-invalid-json",
      "structured-invalid-schema",
    ]) {
      await expect(
        generateText({
          model,
          headers: { "x-test-upstream": mode },
          prompt: "Return an answer.",
          output,
        }),
      ).rejects.toBeInstanceOf(NoObjectGeneratedError);
    }
  });

  test("streams partial typed output and validates the completed object", async () => {
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway, undefined, {
        supportsStructuredOutputs: true,
      }).chatModel(MODEL),
      headers: { "x-test-upstream": "structured-stream" },
      prompt: "Return an answer.",
      output: Output.object({ schema: z.object({ answer: z.string() }) }),
    });
    const partials: Array<{ answer?: string }> = [];
    for await (const partial of result.partialOutputStream)
      partials.push(partial);

    expect(partials.at(-1)).toEqual({ answer: "hello" });
    expect(await result.output).toEqual({ answer: "hello" });
  });

  test("preserves tool results before validating a typed final output", async () => {
    const offset = gateway.upstreamRequests.length;
    const result = await generateText({
      model: createLocalBaseAiSdkProvider(gateway, undefined, {
        supportsStructuredOutputs: true,
      }).chatModel(MODEL),
      headers: { "x-test-upstream": "tool-then-structured" },
      prompt: "Get the Austin forecast.",
      tools: { weather: weatherTool() },
      output: Output.object({
        schema: z.object({ forecast: z.string(), temperature: z.number() }),
      }),
      stopWhen: stepCountIs(2),
    });

    expect(result.output).toEqual({ forecast: "sunny", temperature: 73 });
    const requests = upstreamBodies(gateway, offset);
    expect(requests).toHaveLength(2);
    expect(requests[1]?.messages).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ role: "tool", tool_call_id: "call_weather" }),
      ]),
    );
  });

  test("reconstructs fragmented interleaved streamed tool calls", async () => {
    const calls: string[] = [];
    const result = streamText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(MODEL),
      headers: { "x-test-upstream": "streamed-tool-round-trip" },
      prompt: "Get Austin weather and Chicago time.",
      tools: {
        weather: weatherTool(({ city }) => {
          calls.push(`weather:${city}`);
          return { temperature: 73 };
        }),
        time: tool({
          description: "Gets the local time.",
          inputSchema: z.object({ timezone: z.string() }),
          execute: async ({ timezone }) => {
            calls.push(`time:${timezone}`);
            return { time: "noon" };
          },
        }),
      },
      stopWhen: stepCountIs(2),
    });
    const toolCalls = [];
    for await (const part of result.stream) {
      if (part.type === "tool-call") toolCalls.push(part);
    }

    expect(await result.text).toBe("complete");
    expect(calls.sort()).toEqual(["time:America/Chicago", "weather:Austin"]);
    expect(toolCalls).toHaveLength(2);
    expect(toolCalls).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          toolCallId: "call_weather",
          input: { city: "Austin" },
        }),
        expect.objectContaining({
          toolCallId: "call_time",
          input: { timezone: "America/Chicago" },
        }),
      ]),
    );
  });
});
