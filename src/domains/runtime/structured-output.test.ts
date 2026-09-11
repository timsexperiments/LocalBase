import { describe, expect, test } from "bun:test";
import {
  chatResponseFormatSchema,
  completedStructuredOutputMatchesSchema,
  prepareStructuredOutput,
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

    for (const property of [
      { type: "string", pattern: "^ok$" },
      { type: "number", minimum: 0 },
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
      completedStructuredOutputMatchesSchema(
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
    ).toBe(true);
    expect(
      completedStructuredOutputMatchesSchema(
        {
          choices: [
            { finish_reason: "stop", message: { content: '{"answer":1}' } },
          ],
        },
        prepared.validator,
      ),
    ).toBe(false);

    for (const choice of [
      {
        finish_reason: "length",
        message: { content: "{" },
      },
      {
        finish_reason: "stop",
        message: { content: null, refusal: "refused" },
      },
      {
        finish_reason: "tool_calls",
        message: { content: null, tool_calls: [{}] },
      },
    ]) {
      expect(
        completedStructuredOutputMatchesSchema(
          { choices: [choice] },
          prepared.validator,
        ),
      ).toBe(true);
    }
  });
});
