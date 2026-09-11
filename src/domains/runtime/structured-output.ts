import Ajv, { MissingRefError, type ValidateFunction } from "ajv";
import addFormats from "ajv-formats";
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
// LocalBase caps the JSON text to bound synchronous compilation before admission.
const MAX_SCHEMA_BYTES = 256 * 1024;

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
const SUPPORTED_FORMATS = new Set(["date", "date-time", "time", "uuid"]);

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

function inspectNativeCompatibility(
  schema: JsonObject,
  depth: number,
): string | null {
  if (depth > MAX_SCHEMA_DEPTH) {
    return "Structured output schema exceeds OpenAI's nesting limit.";
  }
  for (const keyword of Object.keys(schema)) {
    if (!SUPPORTED_KEYWORDS.has(keyword) && !ANNOTATION_KEYWORDS.has(keyword)) {
      return `Structured output schema uses unsupported keyword '${keyword}'.`;
    }
  }

  if (schema.$ref !== undefined) {
    const reference = schema.$ref as string;
    if (!reference.startsWith("#/") || reference.includes("~")) {
      return "Structured output schema references must be simple local JSON pointers.";
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
  }

  if (schema.format !== undefined) {
    if (!SUPPORTED_FORMATS.has(schema.format as string)) {
      return "Structured output schema uses a string format unsupported by the managed runtime.";
    }
    if (schema.minLength !== undefined || schema.maxLength !== undefined) {
      return "Structured output schema cannot combine format with string-length constraints.";
    }
  }

  const hasNumericBounds = [
    schema.minimum,
    schema.maximum,
    schema.exclusiveMinimum,
    schema.exclusiveMaximum,
  ].some((value) => value !== undefined);
  if (hasNumericBounds) {
    const nonNullTypes = schemaTypes(schema).filter((type) => type !== "null");
    if (nonNullTypes.length !== 1 || nonNullTypes[0] !== "integer") {
      return "The managed runtime supports numeric bounds only for integer schemas.";
    }
    if (
      (schema.minimum !== undefined && schema.exclusiveMinimum !== undefined) ||
      (schema.maximum !== undefined && schema.exclusiveMaximum !== undefined)
    ) {
      return "Structured output integer bounds cannot combine inclusive and exclusive limits on the same side.";
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
    const issue = inspectNativeCompatibility(child, depth + 1);
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
      strict: true,
      strictRequired: true,
      validateFormats: true,
    });
    addFormats(ajv);
    validator = ajv.compile<unknown>(schema);
  } catch (error) {
    if (error instanceof MissingRefError && !error.missingRef.startsWith("#")) {
      return rejected(
        "unsupported_json_schema",
        "Structured output schema references must be simple local JSON pointers.",
      );
    }
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
  const issue = inspectNativeCompatibility(schema, 1);
  return issue
    ? rejected("unsupported_json_schema", issue)
    : { kind: "ready", validator };
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

export function completedStructuredOutputMatchesSchema(
  response: ChatCompletionForStructuredOutput,
  validator: StructuredOutputValidator,
): boolean {
  for (const choice of response.choices) {
    if (
      choice.finish_reason !== "stop" ||
      choice.message.refusal != null ||
      (choice.message.tool_calls?.length ?? 0) > 0
    ) {
      continue;
    }
    if (typeof choice.message.content !== "string") return false;
    let value: unknown;
    try {
      value = JSON.parse(choice.message.content);
    } catch {
      return false;
    }
    if (!validator(value)) return false;
  }
  return true;
}
