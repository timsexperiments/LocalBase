import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ensureLocalBaseRootMarker } from "../../../utils/root";
import { RotatingLogWriter, createLogEvent, logEventSchema } from "../logging";

const projectRoot = join(import.meta.dirname, "../../../..");
const directory = mkdtempSync(join(tmpdir(), "local-base-logs-cli-"));
const executable = join(directory, "local-base");

async function compileCli(): Promise<void> {
  const child = Bun.spawn(
    [
      process.execPath,
      "build",
      "src/cli.ts",
      "--compile",
      "--no-compile-autoload-dotenv",
      "--no-compile-autoload-bunfig",
      "--asset-naming=[dir]/[name].[ext]",
      `--outfile=${executable}`,
    ],
    { cwd: projectRoot, stdout: "pipe", stderr: "pipe" },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    throw new Error(`Could not compile CLI:\n${stdout}${stderr}`);
  }
}

async function runCli(
  args: string[],
  env: Record<string, string | undefined> = process.env,
) {
  const child = Bun.spawn([executable, ...args], {
    cwd: projectRoot,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    env,
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { exitCode, stdout, stderr };
}

function event(sequence: number, requestId = "req-42") {
  return createLogEvent({
    severity: sequence === 2 ? "error" : "info",
    eventName: "http.request",
    category: "http",
    component: "gateway",
    runtime: "gateway",
    message: `request ${sequence}`,
    requestId,
    http: {
      method: "POST",
      path: "/v1/chat/completions",
      status: 200,
      durationMs: 4.2,
    },
    attributes: { sequence },
  });
}

async function waitFor(condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    if (condition()) return;
    await Bun.sleep(20);
  }
  throw new Error("Timed out waiting for log output.");
}

beforeAll(async () => {
  await compileCli();
}, 30_000);

afterAll(() => {
  rmSync(directory, { recursive: true, force: true });
});

test("reads a root-bound JSON envelope without opening the database", async () => {
  const root = join(directory, "snapshot-root");
  ensureLocalBaseRootMarker(root);
  const writer = new RotatingLogWriter(root);
  await writer.open();
  writer.enqueue(event(1));
  await writer.close();

  const result = await runCli([
    "--root",
    root,
    "logs",
    "--json",
    "--request-id",
    "req-42",
  ]);
  expect(result.exitCode).toBe(0);
  expect(result.stderr).toBe("");
  const lines = result.stdout.trim().split("\n").filter(Boolean);
  expect(lines).toHaveLength(1);
  const document = JSON.parse(lines[0]) as {
    ok: boolean;
    data: { events: unknown[] };
  };
  expect(document.ok).toBe(true);
  expect(document.data.events).toHaveLength(1);
  expect(logEventSchema.parse(document.data.events[0]).requestId).toBe(
    "req-42",
  );
  expect(await Bun.file(join(root, "local-base.db")).exists()).toBe(false);

  const unrelatedEnvironment = await runCli(
    ["--root", root, "logs", "--json"],
    { ...process.env, LOCALBASE_PORT: "not-a-port" },
  );
  expect(unrelatedEnvironment.exitCode).toBe(0);
});

test("streams pure JSONL and exits cleanly when follow receives a signal", async () => {
  const root = join(directory, "follow-root");
  ensureLocalBaseRootMarker(root);
  const writer = new RotatingLogWriter(root, { maxActiveBytes: 300 });
  await writer.open();
  writer.enqueue(event(1));
  await writer.flush();

  const child = Bun.spawn(
    [executable, "--root", root, "--json", "logs", "--follow"],
    {
      cwd: projectRoot,
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    },
  );
  if (!(child.stdout instanceof ReadableStream)) {
    throw new Error("Follow command did not expose stdout.");
  }
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let streamedOutput = "";
  const stdout = (async () => {
    while (true) {
      const { done, value } = await reader.read();
      if (done) return streamedOutput;
      streamedOutput += decoder.decode(value, { stream: true });
    }
  })();
  await Bun.sleep(100);
  writer.enqueue(event(2));
  await writer.flush();
  await waitFor(() => streamedOutput.includes('"sequence":2'));
  child.kill("SIGTERM");
  const [exitCode, output, stderr] = await Promise.all([
    child.exited,
    stdout,
    new Response(child.stderr).text(),
  ]);
  await writer.close();

  expect(exitCode).toBe(0);
  expect(stderr).toBe("");
  const events = output
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => logEventSchema.parse(JSON.parse(line)));
  expect(events.map((entry) => entry.attributes?.sequence)).toEqual([1, 2]);
});
