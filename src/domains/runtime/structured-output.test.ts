import { describe, expect, test } from "bun:test";
import {
  chatResponseFormatSchema,
  prepareStructuredOutput,
  validateCompletedStructuredOutput,
  type StructuredOutputSkipReason,
} from "./structured-output";

function prepare(schema: unknown) {
  return prepareStructuredOutput(
    chatResponseFormatSchema.parse({
      type: "json_schema",
      json_schema: { name: "test_schema", strict: true, schema },
    }),
  );
}

describe("structured output schemas", () => {
  test("compiles common object, array, anyOf, enum, and local-ref schemas", () => {
    const prepared = prepare({
      type: "object",
      properties: {
        status: { type: "string", enum: ["ok", "failed"] },
        entries: {
          type: "array",
          minItems: 1,
          maxItems: 3,
          items: { $ref: "#/$defs/entry" },
        },
        note: {
          anyOf: [{ type: "string", maxLength: 40 }, { type: "null" }],
        },
      },
      required: ["status", "entries", "note"],
      additionalProperties: false,
      $defs: {
        entry: {
          type: "object",
          properties: { count: { type: "integer", minimum: 0 } },
          required: ["count"],
          additionalProperties: false,
        },
      },
    });

    expect(prepared.kind).toBe("ready");
    if (prepared.kind !== "ready") return;
    expect(
      prepared.validator({
        status: "ok",
        entries: [{ count: 2 }],
        note: null,
      }),
    ).toBe(true);
    expect(
      prepared.validator({
        status: "unknown",
        entries: [],
        note: null,
      }),
    ).toBe(false);
  });

  test("rejects invalid references and constraints llama b10419 would ignore", () => {
    const missingRef = prepare({
      type: "object",
      properties: { value: { $ref: "#/$defs/missing" } },
      required: ["value"],
      additionalProperties: false,
    });
    expect(missingRef).toMatchObject({ kind: "rejected" });

    for (const schema of [
      {
        type: "object",
        properties: { $ref: { type: "string" } },
        required: ["$ref"],
        additionalProperties: false,
      },
      {
        type: "object",
        properties: {},
        additionalProperties: false,
        $defs: { $ref: { type: "string" } },
      },
      {
        type: "object",
        properties: {},
        additionalProperties: false,
        definitions: { $ref: { type: "string" } },
      },
    ]) {
      expect(prepare(schema)).toMatchObject({
        kind: "rejected",
        code: "unsupported_json_schema",
      });
    }

    for (const property of [
      { type: "string", pattern: "^ok$" },
      { type: "number", minimum: 0 },
      { type: "integer", minimum: 1.5 },
      { type: "integer", minimum: 2, maximum: 1 },
      { type: "integer", exclusiveMinimum: 1, maximum: 1 },
      { type: "string", enum: [42] },
      { type: "string", format: "date" },
      { type: "string", minLength: 2, maxLength: 2 },
      { type: "array", maxItems: 0 },
      {
        type: "array",
        minItems: 4_294_967_296,
        items: { type: "integer" },
      },
      { type: "array", items: { type: "string" }, uniqueItems: true },
      {
        allOf: [{ type: "string" }, { type: "string", maxLength: 2 }],
      },
      { $ref: "https://example.test/schema.json" },
    ]) {
      expect(
        prepare({
          type: "object",
          properties: { value: property },
          required: ["value"],
          additionalProperties: false,
        }),
      ).toMatchObject({ kind: "rejected" });
    }

    expect(
      prepare({
        type: "object",
        properties: { value: { $ref: "#/$defs/payload/const" } },
        required: ["value"],
        additionalProperties: false,
        $defs: {
          payload: {
            const: {
              type: "array",
              uniqueItems: true,
              items: { type: "integer" },
            },
          },
        },
      }),
    ).toMatchObject({
      kind: "rejected",
      code: "unsupported_json_schema",
    });
    expect(
      prepare({
        type: "object",
        properties: { value: { $ref: "#/$defs/%61" } },
        required: ["value"],
        additionalProperties: false,
        $defs: {
          a: { type: "integer", minimum: 10 },
          "%61": { type: "string" },
        },
      }),
    ).toMatchObject({
      kind: "rejected",
      code: "unsupported_json_schema",
    });
    expect(
      prepare({
        type: "object",
        properties: {
          value: { const: { $ref: "https://example.invalid/remote" } },
        },
        required: ["value"],
        additionalProperties: false,
      }),
    ).toMatchObject({
      kind: "rejected",
      code: "unsupported_json_schema",
    });
  });

  test("compiles a distributed 5,000-property schema without inlining", async () => {
    const source = `
      import { chatResponseFormatSchema, prepareStructuredOutput } from "./src/domains/runtime/structured-output.ts";
      const definitions = Object.fromEntries(Array.from({ length: 50 }, (_, definitionIndex) => {
        const properties = Object.fromEntries(Array.from({ length: 99 }, (_, propertyIndex) => ["p" + propertyIndex, { type: "string" }]));
        return ["definition" + definitionIndex, {
          type: "object",
          properties,
          required: Object.keys(properties),
          additionalProperties: false,
        }];
      }));
      const properties = Object.fromEntries(Array.from({ length: 50 }, (_, index) => ["p" + index, { $ref: "#/$defs/definition" + index }]));
      const schema = {
        type: "object",
        properties,
        required: Object.keys(properties),
        additionalProperties: false,
        $defs: definitions,
      };
      const prepared = prepareStructuredOutput(chatResponseFormatSchema.parse({
        type: "json_schema",
        json_schema: { name: "bounded", schema },
      }));
      if (prepared.kind !== "ready") process.exit(1);
    `;
    const child = Bun.spawn([process.execPath, "-e", source], {
      cwd: `${import.meta.dir}/../../..`,
      stderr: "pipe",
      stdout: "pipe",
      signal: AbortSignal.timeout(3_000),
    });
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited, stderr).toBe(0);
  });

  test("bounds individual objects before validator compilation", () => {
    const properties = (count: number) =>
      Object.fromEntries(
        Array.from({ length: count }, (_, index) => [
          `p${index}`,
          { type: "string" },
        ]),
      );
    const acceptedProperties = properties(1_000);
    expect(
      prepare({
        type: "object",
        properties: acceptedProperties,
        required: Object.keys(acceptedProperties),
        additionalProperties: false,
      }),
    ).toMatchObject({ kind: "ready" });

    const rejectedProperties = properties(1_001);
    expect(
      prepare({
        type: "object",
        properties: rejectedProperties,
        required: Object.keys(rejectedProperties),
        additionalProperties: false,
      }),
    ).toEqual({
      kind: "rejected",
      code: "unsupported_json_schema",
      message:
        "Structured output schema object exceeds LocalBase's 1,000-property compilation limit.",
    });
  });

  test("validates only ordinary assistant content completed with stop", () => {
    const prepared = prepare({
      type: "object",
      properties: { answer: { type: "string" } },
      required: ["answer"],
      additionalProperties: false,
    });
    expect(prepared.kind).toBe("ready");
    if (prepared.kind !== "ready") return;

    expect(
      validateCompletedStructuredOutput(
        {
          choices: [
            {
              finish_reason: "stop",
              message: { content: '{"answer":"yes"}' },
            },
          ],
        },
        prepared.validator,
      ),
    ).toEqual({ outcome: "passed", skipReasons: [] });
    expect(
      validateCompletedStructuredOutput(
        {
          choices: [
            { finish_reason: "stop", message: { content: '{"answer":1}' } },
          ],
        },
        prepared.validator,
      ),
    ).toEqual({ outcome: "failed", skipReasons: [] });

    const skippedChoices: Array<{
      choice: Parameters<
        typeof validateCompletedStructuredOutput
      >[0]["choices"][number];
      reason: StructuredOutputSkipReason;
    }> = [
      {
        choice: { finish_reason: "length", message: { content: "{" } },
        reason: "truncation",
      },
      {
        choice: {
          finish_reason: "content_filter",
          message: { content: null },
        },
        reason: "content_filter",
      },
      {
        choice: { finish_reason: null, message: { content: null } },
        reason: "non_completed",
      },
      {
        choice: { message: { content: null } },
        reason: "non_completed",
      },
      {
        choice: {
          finish_reason: "stop",
          message: { content: null, refusal: "refused" },
        },
        reason: "refusal",
      },
      {
        choice: {
          finish_reason: "tool_calls",
          message: { content: null, tool_calls: [{}] },
        },
        reason: "tool_calls",
      },
    ];
    for (const { choice, reason } of skippedChoices) {
      expect(
        validateCompletedStructuredOutput(
          { choices: [choice] },
          prepared.validator,
        ),
      ).toEqual({ outcome: "skipped", skipReasons: [reason] });
    }
  });
});
