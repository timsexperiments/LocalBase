import { expect, test } from "bun:test";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { parseEnvironmentOverrides } from "../../../context";
import { DatabaseSession } from "../../../db/client";
import { resolveCli } from "./framework";
import { runCli } from "./runner";

test("resolves nested commands and global options before context creation", async () => {
  const catalog = await resolveCli([
    "--root",
    "/tmp/local-base-cli-test",
    "models",
    "catalog",
    "--kind=llm",
  ]);

  expect(catalog).toMatchObject({
    kind: "command",
    global: { root: "/tmp/local-base-cli-test", nonInteractive: false },
  });
  expect((await resolveCli(["keys"])).kind).toBe("help");
  expect((await resolveCli(["--help"])).kind).toBe("help");

  const serve = await resolveCli(["serve", "--no-auth"]);
  expect(serve).toMatchObject({ kind: "command", input: { auth: false } });

  const emptyModelList = await resolveCli(["configure", "--stt-models", ""]);
  expect(emptyModelList).toMatchObject({
    kind: "command",
    input: { sttModels: [] },
  });
});

test("rejects invalid CLI structure and contradictory interaction options", async () => {
  await expect(
    resolveCli(["models", "catalog", "--unknown"]),
  ).resolves.toMatchObject({
    kind: "error",
    message: "Unknown option: --unknown",
  });
  await expect(
    resolveCli(["models", "catalog", "--kind", "vision"]),
  ).resolves.toMatchObject({
    kind: "error",
    message: expect.stringContaining("kind"),
  });
  await expect(resolveCli(["catalog"])).resolves.toMatchObject({
    kind: "error",
    message: "Unknown command: catalog",
  });
  await expect(resolveCli(["serve", "--no-auth=false"])).resolves.toMatchObject(
    {
      kind: "error",
      message: "--no-auth does not accept a value",
    },
  );
  await expect(
    resolveCli(["configure", "--all", "--non-interactive"]),
  ).resolves.toMatchObject({
    kind: "error",
    message: "--all cannot be used with --non-interactive",
  });
  await expect(
    resolveCli(["serve", "--host", "bad host"]),
  ).resolves.toMatchObject({
    kind: "error",
    message: expect.stringContaining("host"),
  });
  await expect(
    resolveCli(["configure", "--active-stt", ""]),
  ).resolves.toMatchObject({
    kind: "error",
    message: expect.stringContaining("activeStt"),
  });
});

test("help and syntax failures skip context creation", async () => {
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
    await expect(runCli(["models", "--help"], createContext)).resolves.toBe(0);
    await expect(runCli(["missing-command"], createContext)).resolves.toBe(2);
  } finally {
    console.log = originalLog;
    console.error = originalError;
  }

  expect(contextsCreated).toBe(0);
});

test("closes a command-scoped database session exactly once", async () => {
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
      runCli(["reset"], async (_options, initializeDatabase) => {
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

test("reports environment input failures as concise syntax errors", async () => {
  const errors: string[] = [];
  const originalError = console.error;
  console.error = (...values: unknown[]) => errors.push(values.join(" "));

  try {
    await expect(
      runCli(["doctor"], async () => {
        parseEnvironmentOverrides({ LOCALBASE_PORT: "invalid" });
        throw new Error("unreachable");
      }),
    ).resolves.toBe(2);
  } finally {
    console.error = originalError;
  }

  expect(errors[0]).toBe(
    "Error: LOCALBASE_PORT: LOCALBASE_PORT must be an integer",
  );
});
