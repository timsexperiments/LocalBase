import { expect, test } from "bun:test";
import { parseEnvironmentOverrides, resolveEffectiveRoot } from "./context";

test("validates environment overrides without mutating process state", () => {
  expect(() =>
    parseEnvironmentOverrides({ LOCALBASE_PORT: "not-a-port" }),
  ).toThrow("LOCALBASE_PORT: LOCALBASE_PORT must be an integer");

  expect(
    parseEnvironmentOverrides({
      LOCALBASE_HOST: "127.0.0.1",
      LOCALBASE_PORT: "2274",
      LOCALBASE_STT_HOST: "localhost",
      LOCALBASE_STT_PORT: "8080",
      LOCALBASE_CTX_SIZE: "8192",
    }),
  ).toEqual({
    host: "127.0.0.1",
    port: 2274,
    sttHost: "localhost",
    sttPort: 8080,
    ctxSize: 8192,
  });
});

test("resolves data roots with CLI, environment, and configured precedence", () => {
  expect(
    resolveEffectiveRoot("/tmp/cli", "/tmp/environment", "/tmp/configured"),
  ).toBe("/tmp/cli");
  expect(
    resolveEffectiveRoot(undefined, "/tmp/environment", "/tmp/configured"),
  ).toBe("/tmp/environment");
  expect(resolveEffectiveRoot(undefined, undefined, "/tmp/configured")).toBe(
    "/tmp/configured",
  );
});
