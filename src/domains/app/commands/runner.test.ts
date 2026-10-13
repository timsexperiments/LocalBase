import { expect, test } from "bun:test";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { parseEnvironmentOverrides } from "../../../context";
import { DatabaseSession } from "../../../db/client";
import {
  commandHelpText,
  resolveCli,
  rootCommandDefinition,
} from "./framework";
import { runCli } from "./runner";
import { LocalBaseLogger } from "../../observability/logging";

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
  expect((await resolveCli(["access"])).kind).toBe("help");
  expect((await resolveCli(["access", "oidc"])).kind).toBe("help");
  expect((await resolveCli(["access", "github"])).kind).toBe("help");
  expect((await resolveCli(["access", "users"])).kind).toBe("help");
  expect((await resolveCli(["--help"])).kind).toBe("help");

  await expect(resolveCli(["models", "catalog"])).resolves.toMatchObject({
    kind: "command",
    command: { requiresDatabase: false },
  });
  await expect(resolveCli(["models", "recommend"])).resolves.toMatchObject({
    kind: "command",
    command: { requiresDatabase: false },
  });
  await expect(resolveCli(["doctor"])).resolves.toMatchObject({
    kind: "command",
    command: { requiresDatabase: false, readOnlyConfiguration: true },
  });
  await expect(
    resolveCli(["access", "oidc", "remove", "company"]),
  ).resolves.toMatchObject({
    kind: "command",
    input: { id: "company" },
  });
  await expect(
    resolveCli(["access", "github", "remove", "github"]),
  ).resolves.toMatchObject({
    kind: "command",
    input: { id: "github" },
  });
  await expect(
    resolveCli([
      "access",
      "users",
      "invite",
      "--email",
      "Person@Example.com",
      "--roles",
      "member,admin",
    ]),
  ).resolves.toMatchObject({
    kind: "command",
    input: { email: "person@example.com", roles: ["member", "admin"] },
  });

  const serve = await resolveCli(["serve", "--no-auth"]);
  expect(serve).toMatchObject({ kind: "command", input: { auth: false } });
  await expect(
    resolveCli([
      "serve",
      "--inference-queue-capacity",
      "24",
      "--inference-queue-timeout-ms",
      "45000",
    ]),
  ).resolves.toMatchObject({
    kind: "command",
    input: { inferenceQueueCapacity: 24, inferenceQueueTimeoutMs: 45_000 },
  });

  const emptyModelList = await resolveCli(["configure", "--stt-models", ""]);
  expect(emptyModelList).toMatchObject({
    kind: "command",
    input: { sttModels: [] },
  });
});

test("CLI help includes the license and source notice", async () => {
  const help = await commandHelpText(rootCommandDefinition());
  expect(help).toContain("AGPL-3.0-or-later");
  expect(help).toContain("https://github.com/timsexperiments/LocalBase");
});

test("rejects invalid CLI structure and contradictory interaction options", async () => {
  for (const args of [
    ["configure", "--stt-models"],
    ["configure", "--stt-models", "--no-create-key"],
  ]) {
    await expect(resolveCli(args)).resolves.toMatchObject({
      kind: "error",
      message: "--stt-models requires a value",
    });
  }
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
  for (const [flag, value] of [
    ["--llm-model-file", "../outside.gguf"],
    ["--stt-model-file", "/tmp/model.bin"],
    ["--image-model-file", "nested/model.safetensors"],
  ]) {
    await expect(resolveCli(["serve", flag, value])).resolves.toMatchObject({
      kind: "error",
      message: expect.stringContaining("safe basename"),
    });
  }
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
  const originalWrite = process.stderr.write;
  process.stderr.write = (() => true) as typeof process.stderr.write;
  try {
    await expect(
      runCli(["reset"], async (_options, initializeDatabase) => {
        databaseInitialized = initializeDatabase;
        return context;
      }),
    ).resolves.toBe(2);
  } finally {
    process.stderr.write = originalWrite;
  }

  expect(databaseInitialized).toBe(false);
  expect(closes).toBe(1);
});

test("JSON commands keep logger output on the JSON line stream", async () => {
  const stdout: string[] = [];
  const originalWrite = process.stdout.write;
  const originalTestLogs = process.env.LOCALBASE_TEST_LOGS;
  process.env.LOCALBASE_TEST_LOGS = "1";
  process.stdout.write = ((value: string | Uint8Array) => {
    stdout.push(String(value));
    return true;
  }) as typeof process.stdout.write;
  const context = {
    config: defaultConfig("/tmp/local-base-runner-json-test"),
    database: new DatabaseSession(),
    specs: { gpuVramGb: 0 },
    logger: {},
  } as AppContext;
  try {
    await runCli(["--json", "reset"], async () => {
      context.logger = new LocalBaseLogger();
      context.logger.info("runtime", "logger marker");
      return context;
    });
  } finally {
    process.stdout.write = originalWrite;
    if (originalTestLogs === undefined) delete process.env.LOCALBASE_TEST_LOGS;
    else process.env.LOCALBASE_TEST_LOGS = originalTestLogs;
    context.database.close();
  }
  expect(stdout.length).toBeGreaterThan(0);
  for (const line of stdout) {
    try {
      JSON.parse(line);
    } catch {
      throw new Error(`Non-JSON stdout write: ${JSON.stringify(line)}`);
    }
  }
  expect(stdout.join("")).toContain('"message":"logger marker"');
});

test("reports environment input failures as concise syntax errors", async () => {
  const errors: string[] = [];
  const originalWrite = process.stderr.write;
  process.stderr.write = ((value: string | Uint8Array) => {
    errors.push(String(value).trimEnd());
    return true;
  }) as typeof process.stderr.write;

  try {
    await expect(
      runCli(["doctor"], async () => {
        parseEnvironmentOverrides({ LOCALBASE_PORT: "invalid" });
        throw new Error("unreachable");
      }),
    ).resolves.toBe(2);
  } finally {
    process.stderr.write = originalWrite;
  }

  expect(errors[0]).toBe(
    "Error: LOCALBASE_PORT: LOCALBASE_PORT must be an integer",
  );
});
