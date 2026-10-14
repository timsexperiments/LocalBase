import { describe, expect, test } from "bun:test";
import { anchorJsonSchemaPatterns, anchorPattern } from "./tool-schema";

describe("anchorPattern", () => {
  test("anchors unanchored patterns", () => {
    expect(anchorPattern("[a-z]+")).toBe("^[a-z]+$");
  });

  test("keeps anchored patterns unchanged", () => {
    expect(anchorPattern("^[a-z]+$")).toBe("^[a-z]+$");
  });

  test("adds only the missing anchor", () => {
    expect(anchorPattern("^abc")).toBe("^abc$");
    expect(anchorPattern("abc$")).toBe("^abc$");
  });

  test("groups alternations before anchoring", () => {
    expect(anchorPattern("foo|bar")).toBe("^(?:foo|bar)$");
  });

  test("treats an escaped trailing dollar as unanchored", () => {
    expect(anchorPattern("price\\$")).toBe("^price\\$$");
  });
});

describe("anchorJsonSchemaPatterns", () => {
  test("anchors nested patterns while leaving data keywords alone", () => {
    const schema = {
      type: "object",
      properties: {
        path: { type: "string", pattern: "[./].*" },
        pattern: { type: "string", description: "glob" },
        items: {
          type: "array",
          items: { type: "string", pattern: "^x" },
        },
        mode: { enum: [{ pattern: "raw" }] },
        enum: { type: "string", pattern: "[a-z]+" },
      },
      anyOf: [{ pattern: "a" }, { pattern: "^b$" }],
    };
    expect(anchorJsonSchemaPatterns(schema)).toEqual({
      type: "object",
      properties: {
        path: { type: "string", pattern: "^[./].*$" },
        pattern: { type: "string", description: "glob" },
        items: {
          type: "array",
          items: { type: "string", pattern: "^x$" },
        },
        mode: { enum: [{ pattern: "raw" }] },
        enum: { type: "string", pattern: "^[a-z]+$" },
      },
      anyOf: [{ pattern: "^a$" }, { pattern: "^b$" }],
    });
  });
});
