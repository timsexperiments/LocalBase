import Ajv, { type ValidateFunction } from "ajv";
import { z } from "zod";

type JsonValue =
  string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue };
type JsonObject = { [key: string]: JsonValue };

export const jsonSchemaValueSchema: z.ZodType<JsonValue> = z.lazy(() =>
  z.union([
    z.string(),
    z.number(),
    z.boolean(),
    z.null(),
    z.array(jsonSchemaValueSchema),
    z.record(z.string(), jsonSchemaValueSchema),
  ]),
);

const jsonSchemaObjectSchema: z.ZodType<JsonObject> = z.record(
  z.string(),
  jsonSchemaValueSchema,
);

export const chatResponseFormatSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("text") }).strict(),
  z.object({ type: z.literal("json_object") }).strict(),
  z
    .object({
      type: z.literal("json_schema"),
      json_schema: z
        .object({
          name: z
            .string()
            .min(1)
            .max(64)
            .regex(/^[A-Za-z0-9_-]+$/),
          description: z.string().optional(),
          schema: jsonSchemaObjectSchema,
          strict: z.boolean().optional(),
        })
        .strict(),
    })
    .strict(),
]);

export type ChatResponseFormat = z.output<typeof chatResponseFormatSchema>;
export type StructuredOutputValidator = ValidateFunction<unknown>;
export type StructuredOutputErrorCode =
  "invalid_json_schema" | "unsupported_json_schema";
export type StructuredOutputPreparation =
  | { kind: "none" }
  | { kind: "ready"; validator: StructuredOutputValidator }
  | {
      kind: "rejected";
      code: StructuredOutputErrorCode;
      message: string;
    };

// OpenAI strict schemas allow at most ten nested schema levels.
const MAX_SCHEMA_DEPTH = 10;
// OpenAI strict schemas allow at most 5,000 object properties.
const MAX_SCHEMA_PROPERTIES = 5_000;
// LocalBase caps the JSON text to bound synchronous compilation before admission.
const MAX_SCHEMA_BYTES = 256 * 1024;
// LocalBase bounds reference resolution and native grammar repetition work.
const MAX_SCHEMA_REFERENCES = 1_000;
const MAX_NATIVE_REPETITION = 10_000;

const ANNOTATION_KEYWORDS = new Set([
  "$comment",
  "$schema",
  "description",
  "title",
]);
const SUPPORTED_KEYWORDS = new Set([
  "$defs",
  "$ref",
  "additionalProperties",
  "anyOf",
  "const",
  "definitions",
  "enum",
  "exclusiveMaximum",
  "exclusiveMinimum",
  "format",
  "items",
  "maxItems",
  "maxLength",
  "maximum",
  "minItems",
  "minLength",
  "minimum",
  "properties",
  "required",
  "type",
]);
function isObject(value: JsonValue | undefined): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function childSchemas(schema: JsonObject): JsonObject[] | null {
  const children: JsonObject[] = [];
  for (const container of [
    schema.properties,
    schema.$defs,
    schema.definitions,
  ]) {
    if (container === undefined) continue;
    // b10419 mistakes dictionary entries named "$ref" for schema references.
    if (!isObject(container) || Object.hasOwn(container, "$ref")) return null;
    for (const child of Object.values(container as JsonObject)) {
      if (!isObject(child)) return null;
      children.push(child);
    }
  }
  if (schema.items !== undefined) {
    if (!isObject(schema.items)) return null;
    children.push(schema.items);
  }
  if (schema.anyOf !== undefined) {
    if (!Array.isArray(schema.anyOf)) return null;
    for (const child of schema.anyOf as JsonValue[]) {
      if (!isObject(child)) return null;
      children.push(child);
    }
  }
  return children;
}

function hasValidationSiblings(
  schema: JsonObject,
  primary: string,
  allowType: boolean,
): boolean {
  return Object.keys(schema).some(
    (keyword) =>
      keyword !== primary &&
      !(allowType && keyword === "type") &&
      !ANNOTATION_KEYWORDS.has(keyword),
  );
}

function schemaTypes(schema: JsonObject): string[] {
  return typeof schema.type === "string"
    ? [schema.type]
    : ((schema.type as string[] | undefined) ?? []);
}

function hasOnlyType(schema: JsonObject, expected: string): boolean {
  const nonNullTypes = schemaTypes(schema).filter((type) => type !== "null");
  return nonNullTypes.length === 1 && nonNullTypes[0] === expected;
}

function isSupportedReference(value: JsonValue | undefined): value is string {
  return (
    typeof value === "string" &&
    /^#\/(?:\$defs|definitions)\/[A-Za-z0-9_-]+$/.test(value)
  );
}

function precompileBudgetIssue(root: JsonObject): string | null {
  let propertyCount = 0;
  let referenceCount = 0;
  const pending = [{ schema: root, depth: 1 }];

  while (pending.length > 0) {
    const current = pending.pop();
    if (!current) break;
    if (current.depth > MAX_SCHEMA_DEPTH) {
      return "Structured output schema exceeds OpenAI's nesting limit.";
    }
    if (isObject(current.schema.properties)) {
      propertyCount += Object.keys(current.schema.properties).length;
      if (propertyCount > MAX_SCHEMA_PROPERTIES) {
        return "Structured output schema exceeds OpenAI's property limit.";
      }
    }
    if (current.schema.$ref !== undefined) {
      referenceCount += 1;
      if (referenceCount > MAX_SCHEMA_REFERENCES) {
        return "Structured output schema exceeds LocalBase's reference limit.";
      }
      if (
        typeof current.schema.$ref === "string" &&
        !isSupportedReference(current.schema.$ref)
      ) {
        return "Structured output schema references must target direct local definitions.";
      }
    }
    const children = childSchemas(current.schema);
    if (!children) continue;
    for (const child of children) {
      pending.push({ schema: child, depth: current.depth + 1 });
    }
  }
  return null;
}

function isScalar(value: JsonValue): boolean {
  return value === null || typeof value !== "object";
}

function literalsMatchDeclaredType(schema: JsonObject, ajv: Ajv): boolean {
  const values = (
    schema.enum !== undefined ? schema.enum : [schema.const]
  ) as JsonValue[];
  if (!values.every(isScalar)) return false;
  if (schema.type === undefined) return true;
  const validatesType = ajv.compile({ type: schema.type });
  return values.every((value) => validatesType(value));
}

function repetitionIssue(
  schema: JsonObject,
  minimumKey: "minItems" | "minLength",
  maximumKey: "maxItems" | "maxLength",
): string | null {
  const minimum = schema[minimumKey] as number | undefined;
  const maximum = schema[maximumKey] as number | undefined;
  if (
    (minimum !== undefined && minimum > MAX_NATIVE_REPETITION) ||
    (maximum !== undefined && maximum > MAX_NATIVE_REPETITION)
  ) {
    return `Structured output schema ${minimumKey}/${maximumKey} exceeds LocalBase's native repetition limit.`;
  }
  if (minimum !== undefined && maximum !== undefined && minimum > maximum) {
    return `Structured output schema ${minimumKey} cannot exceed ${maximumKey}.`;
  }
  return null;
}

function inspectNativeCompatibility(
  schema: JsonObject,
  ajv: Ajv,
): string | null {
  for (const keyword of Object.keys(schema)) {
    if (!SUPPORTED_KEYWORDS.has(keyword) && !ANNOTATION_KEYWORDS.has(keyword)) {
      return `Structured output schema uses unsupported keyword '${keyword}'.`;
    }
  }

  if (schema.$ref !== undefined) {
    if (!isSupportedReference(schema.$ref)) {
      return "Structured output schema references must target direct local definitions.";
    }
    return hasValidationSiblings(schema, "$ref", false)
      ? "Structured output schema reference siblings are unsupported."
      : null;
  }
  if (
    schema.anyOf !== undefined &&
    hasValidationSiblings(schema, "anyOf", false)
  ) {
    return "Structured output schema anyOf siblings are unsupported.";
  }
  if (schema.enum !== undefined || schema.const !== undefined) {
    if (schema.enum !== undefined && schema.const !== undefined) {
      return "Structured output schema cannot combine enum and const.";
    }
    const primary = schema.enum !== undefined ? "enum" : "const";
    if (hasValidationSiblings(schema, primary, true)) {
      return `Structured output schema ${primary} cannot be combined with other constraints.`;
    }
    if (!literalsMatchDeclaredType(schema, ajv)) {
      return `Structured output schema ${primary} values must be scalar and satisfy the declared type.`;
    }
  }

  if (schema.format !== undefined) {
    return "Structured output schema formats are unsupported by the managed runtime contract.";
  }

  const objectKeywords = [
    schema.properties,
    schema.required,
    schema.additionalProperties,
  ].some((value) => value !== undefined);
  if (objectKeywords && !hasOnlyType(schema, "object")) {
    return "Structured output object constraints require an object or nullable object type.";
  }

  const arrayKeywords = [schema.items, schema.minItems, schema.maxItems].some(
    (value) => value !== undefined,
  );
  if (arrayKeywords && !hasOnlyType(schema, "array")) {
    return "Structured output array constraints require an array or nullable array type.";
  }
  if (schemaTypes(schema).includes("array") && !isObject(schema.items)) {
    return "Structured output arrays require an object-valued items schema.";
  }
  const arrayRepetitionIssue = repetitionIssue(schema, "minItems", "maxItems");
  if (arrayRepetitionIssue) return arrayRepetitionIssue;

  const stringLengthKeywords = [schema.minLength, schema.maxLength].some(
    (value) => value !== undefined,
  );
  if (stringLengthKeywords && !hasOnlyType(schema, "string")) {
    return "Structured output string lengths require a string or nullable string type.";
  }
  if (schema.minLength !== undefined) {
    return "Structured output schema minLength is unsupported by the managed runtime contract.";
  }
  const stringRepetitionIssue = repetitionIssue(
    schema,
    "minLength",
    "maxLength",
  );
  if (stringRepetitionIssue) return stringRepetitionIssue;

  const hasNumericBounds = [
    schema.minimum,
    schema.maximum,
    schema.exclusiveMinimum,
    schema.exclusiveMaximum,
  ].some((value) => value !== undefined);
  if (hasNumericBounds) {
    if (!hasOnlyType(schema, "integer")) {
      return "The managed runtime supports numeric bounds only for integer schemas.";
    }
    const bounds = [
      schema.minimum,
      schema.maximum,
      schema.exclusiveMinimum,
      schema.exclusiveMaximum,
    ] as Array<number | undefined>;
    if (
      !bounds.every(
        (value) => value === undefined || Number.isSafeInteger(value),
      )
    ) {
      return "Structured output integer bounds must be safe integers.";
    }
    if (
      (schema.minimum !== undefined && schema.exclusiveMinimum !== undefined) ||
      (schema.maximum !== undefined && schema.exclusiveMaximum !== undefined)
    ) {
      return "Structured output integer bounds cannot combine inclusive and exclusive limits on the same side.";
    }
    const inclusiveMinimum =
      schema.minimum !== undefined
        ? (schema.minimum as number)
        : schema.exclusiveMinimum !== undefined
          ? (schema.exclusiveMinimum as number) + 1
          : undefined;
    const inclusiveMaximum =
      schema.maximum !== undefined
        ? (schema.maximum as number)
        : schema.exclusiveMaximum !== undefined
          ? (schema.exclusiveMaximum as number) - 1
          : undefined;
    if (
      (inclusiveMinimum !== undefined &&
        !Number.isSafeInteger(inclusiveMinimum)) ||
      (inclusiveMaximum !== undefined &&
        !Number.isSafeInteger(inclusiveMaximum))
    ) {
      return "Structured output exclusive integer bounds must remain within the safe integer range.";
    }
    if (
      inclusiveMinimum !== undefined &&
      inclusiveMaximum !== undefined &&
      inclusiveMinimum > inclusiveMaximum
    ) {
      return "Structured output integer bounds must contain at least one value.";
    }
  }

  if (schemaTypes(schema).includes("object")) {
    if (
      schema.properties === undefined ||
      schema.additionalProperties !== false
    ) {
      return "Structured output objects require properties and additionalProperties: false.";
    }
    const properties = Object.keys(schema.properties as JsonObject);
    const required = schema.required;
    if (
      !Array.isArray(required) ||
      required.length !== properties.length ||
      properties.some((name) => !required.includes(name))
    ) {
      return "Structured output objects must require every declared property.";
    }
  }

  const children = childSchemas(schema);
  if (!children) {
    return "The managed runtime requires object-valued subschemas.";
  }
  for (const child of children) {
    const issue = inspectNativeCompatibility(child, ajv);
    if (issue) return issue;
  }
  return null;
}

function rejected(
  code: StructuredOutputErrorCode,
  message: string,
): StructuredOutputPreparation {
  return { kind: "rejected", code, message };
}

function compileSchema(schema: JsonObject): StructuredOutputPreparation {
  if (
    schema.$schema !== undefined &&
    schema.$schema !== "http://json-schema.org/draft-07/schema#"
  ) {
    return rejected(
      "unsupported_json_schema",
      "Structured output schemas must use JSON Schema draft 7.",
    );
  }
  let validator: ValidateFunction<unknown>;
  try {
    const ajv = new Ajv({
      allErrors: false,
      allowUnionTypes: true,
      inlineRefs: false,
      logger: false,
      strict: true,
      strictRequired: true,
      validateFormats: false,
    });
    validator = ajv.compile<unknown>(schema);
    const issue = inspectNativeCompatibility(schema, ajv);
    if (issue) return rejected("unsupported_json_schema", issue);
  } catch {
    return rejected(
      "invalid_json_schema",
      "Structured output schema is not valid JSON Schema draft 7.",
    );
  }

  if (schema.type !== "object") {
    return rejected(
      "unsupported_json_schema",
      "Structured output schema root must be an object type.",
    );
  }
  return { kind: "ready", validator };
}

export function prepareStructuredOutput(
  responseFormat: ChatResponseFormat | undefined,
): StructuredOutputPreparation {
  if (!responseFormat || responseFormat.type !== "json_schema") {
    return { kind: "none" };
  }
  const schema = responseFormat.json_schema.schema;
  if (
    new TextEncoder().encode(JSON.stringify(schema)).byteLength >
    MAX_SCHEMA_BYTES
  ) {
    return rejected(
      "unsupported_json_schema",
      "Structured output schema exceeds LocalBase's 256 KiB compile limit.",
    );
  }
  const budgetIssue = precompileBudgetIssue(schema);
  if (budgetIssue) return rejected("unsupported_json_schema", budgetIssue);
  return compileSchema(schema);
}

type ChatCompletionForStructuredOutput = {
  choices: Array<{
    finish_reason?: string | null;
    message: {
      content?: unknown;
      refusal?: string | null;
      tool_calls?: unknown[];
    };
  }>;
};

export type StructuredOutputSkipReason =
  "refusal" | "tool_calls" | "truncation";

export type CompletedStructuredOutputValidation = Readonly<{
  outcome: "passed" | "failed" | "skipped";
  skipReasons: readonly StructuredOutputSkipReason[];
}>;

export function validateCompletedStructuredOutput(
  response: ChatCompletionForStructuredOutput,
  validator: StructuredOutputValidator,
): CompletedStructuredOutputValidation {
  const skipReasons = new Set<StructuredOutputSkipReason>();
  let validated = false;
  const result = (
    outcome: CompletedStructuredOutputValidation["outcome"],
  ): CompletedStructuredOutputValidation => ({
    outcome,
    skipReasons: [...skipReasons].sort(),
  });
  for (const choice of response.choices) {
    if (choice.message.refusal != null) {
      skipReasons.add("refusal");
      continue;
    }
    if ((choice.message.tool_calls?.length ?? 0) > 0) {
      skipReasons.add("tool_calls");
      continue;
    }
    if (choice.finish_reason !== "stop") {
      skipReasons.add("truncation");
      continue;
    }
    if (typeof choice.message.content !== "string") return result("failed");
    let value: unknown;
    try {
      value = JSON.parse(choice.message.content);
    } catch {
      return result("failed");
    }
    if (!validator(value)) return result("failed");
    validated = true;
  }
  return result(validated ? "passed" : "skipped");
}
