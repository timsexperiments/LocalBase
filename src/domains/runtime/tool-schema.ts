import type { JsonValue } from "./structured-output";

/** Schema keywords whose values are data, not nested schemas. */
const dataKeywords = new Set(["enum", "const", "default", "examples"]);

/** Keywords whose values map arbitrary names to nested schemas. */
const schemaMapKeywords = new Set([
  "properties",
  "patternProperties",
  "$defs",
  "definitions",
  "dependentSchemas",
]);

/**
 * llama.cpp's grammar converter rejects any string `pattern` that is not
 * anchored with `^` and `$`. Clients (and JSON Schema itself) treat patterns
 * as unanchored, so anchor them before forwarding tool schemas to llama-server.
 */
export function anchorJsonSchemaPatterns(value: JsonValue): JsonValue {
  if (Array.isArray(value)) {
    return value.map((entry) => anchorJsonSchemaPatterns(entry));
  }
  if (value === null || typeof value !== "object") return value;
  const result: Record<string, JsonValue> = {};
  for (const [key, entry] of Object.entries(value)) {
    if (key === "pattern" && typeof entry === "string") {
      result[key] = anchorPattern(entry);
    } else if (dataKeywords.has(key)) {
      result[key] = entry;
    } else if (schemaMapKeywords.has(key) && isObject(entry)) {
      result[key] = Object.fromEntries(
        Object.entries(entry).map(([name, schema]) => [
          name,
          anchorJsonSchemaPatterns(schema),
        ]),
      );
    } else {
      result[key] = anchorJsonSchemaPatterns(entry);
    }
  }
  return result;
}

function isObject(value: JsonValue): value is { [key: string]: JsonValue } {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export function anchorPattern(pattern: string): string {
  const anchoredStart = pattern.startsWith("^");
  const anchoredEnd = pattern.endsWith("$") && !pattern.endsWith("\\$");
  if (anchoredStart && anchoredEnd) return pattern;
  // Wrap in a group so alternations keep their meaning once anchored.
  const needsGroup = pattern.includes("|");
  const body = needsGroup ? `(?:${pattern})` : pattern;
  return `${anchoredStart ? "" : "^"}${body}${anchoredEnd ? "" : "$"}`;
}
