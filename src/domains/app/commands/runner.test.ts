import { expect, test } from "bun:test";
import { resolveCommand, runCli } from "./runner";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { DatabaseSession } from "../../../db/client";

test("rejects unknown flags, invalid kinds, and positional misuse", () => {
  expect(resolveCommand(["catalog", "--unknown"])).toEqual({
    kind: "error",
    message: "Unknown flag for catalog: --unknown",
  });
  expect(resolveCommand(["catalog", "--kind", "vision"])).toEqual({
    kind: "error",
    message: "Invalid value for --kind: vision",
  });
  expect(resolveCommand(["doctor", "extra"])).toEqual({
    kind: "error",
    message:
      "Invalid positional arguments for doctor; expected no positional arguments",
  });
});

test("accepts serve root selection and supported memory-check overrides", () => {
  expect(
    resolveCommand([
      "serve",
      "--root",
      "/tmp/localbase-test",
      "--bypass-memory-check",
    ]).kind,
  ).toBe("command");
  expect(
    resolveCommand(["serve", "--root", "/tmp/localbase-test", "--unknown"]),
  ).toEqual({
    kind: "error",
    message: "Unknown flag for serve: --unknown",
  });
});

test("routes help and invalid commands before context creation", async () => {
  let contextsCreated = 0;
  const createContext = async () => {
    contextsCreated += 1;
    throw new Error("context should not be created");
  };
  const originalLog = console.log;
  const originalError = console.error;
  console.log = () => {};
  console.error = () => {};

  try {
    await expect(runCli(["--help"], createContext)).resolves.toBe(0);
    await expect(runCli(["missing-command"], createContext)).resolves.toBe(2);
  } finally {
    console.log = originalLog;
    console.error = originalError;
  }

  expect(contextsCreated).toBe(0);
});

test("closes command-scoped database resources", async () => {
  let closes = 0;
  let databaseInitialized = true;
  class TestDatabaseSession extends DatabaseSession {
    override close(): void {
      closes += 1;
      super.close();
    }
  }
  const context = {
    config: defaultConfig("/tmp/local-base-runner-test"),
    database: new TestDatabaseSession(),
    specs: { gpuVramGb: 0 },
    logger: {},
  } as AppContext;
  const originalError = console.error;
  console.error = () => {};
  try {
    await expect(
      runCli(["reset"], async (_args, initializeDatabase) => {
        databaseInitialized = initializeDatabase;
        return context;
      }),
    ).resolves.toBe(2);
  } finally {
    console.error = originalError;
  }

  expect(databaseInitialized).toBe(false);
  expect(closes).toBe(1);
});
