import { expect, test } from "bun:test";
import { chmodSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { defaultConfig, loadConfig, saveConfig } from "../../../manager";

const INITIAL_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const SWITCHED_MODEL = "qwen2.5-coder-7b-instruct-q4_k_m";
const PROJECT_ROOT = join(import.meta.dirname, "../../../..");
const textEncoder = new TextEncoder();
const textBytes = (value: string) => textEncoder.encode(value);

function reservePort(): number {
  for (let attempt = 0; attempt < 10; attempt++) {
    const value = new Uint32Array(1);
    crypto.getRandomValues(value);
    const port = 20_000 + (value[0] % 40_000);
    try {
      const reservation = Bun.serve({
        hostname: "127.0.0.1",
        port,
        fetch: () => new Response("reserved"),
      });
      reservation.stop(true);
      return port;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EADDRINUSE") throw error;
    }
  }
  throw new Error("Could not reserve a test port");
}

async function waitForGateway(
  process: Bun.Subprocess,
  baseUrl: string,
): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (process.exitCode !== null) {
      throw new Error(`Gateway exited with code ${process.exitCode}`);
    }
    try {
      const response = await fetch(`${baseUrl}/health`, {
        signal: AbortSignal.timeout(250),
      });
      if (response.ok) return;
    } catch {
      // The gateway socket may not be listening yet.
    }
    await Bun.sleep(50);
  }
  throw new Error("Gateway did not become ready");
}

async function readProcessOutput(
  stream: ReadableStream<Uint8Array> | number | undefined,
): Promise<string> {
  if (!stream || typeof stream === "number") return "";
  return new Response(stream).text();
}

async function stopProcess(process: Bun.Subprocess): Promise<void> {
  if (process.exitCode === null) process.kill(15);
  await Promise.race([process.exited, Bun.sleep(3_000)]);
  if (process.exitCode === null) process.kill(9);
  await process.exited;
}

async function waitForFile(path: string): Promise<void> {
  const deadline = Date.now() + 2_000;
  while (Date.now() < deadline) {
    if (await Bun.file(path).exists()) return;
    await Bun.sleep(20);
  }
  throw new Error(`Timed out waiting for ${path}`);
}

async function waitForProcessExit(pid: number): Promise<void> {
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    try {
      process.kill(pid, 0);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ESRCH") return;
      throw error;
    }
    await Bun.sleep(25);
  }
  throw new Error(`Process ${pid} did not exit`);
}

function isProcessRunning(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw error;
  }
}

async function findGuardianPid(
  gatewayPid: number,
  backendPid: number,
): Promise<number> {
  const deadline = Date.now() + 2_000;
  const marker = `__localbase_backend_guardian ${gatewayPid} ${backendPid}`;
  while (Date.now() < deadline) {
    const process = Bun.spawn(["ps", "-axww", "-o", "pid=", "-o", "command="], {
      stdout: "pipe",
      stderr: "ignore",
    });
    const output = await readProcessOutput(process.stdout);
    await process.exited;
    const line = output.split("\n").find((value) => value.includes(marker));
    if (line) {
      const pid = Number(line.trim().split(/\s+/, 1)[0]);
      if (Number.isInteger(pid)) return pid;
    }
    await Bun.sleep(25);
  }
  throw new Error("Backend guardian did not start");
}

function serveRunnerSource(): string {
  const catalogPath = join(PROJECT_ROOT, "src/catalog.ts");
  const contextPath = join(PROJECT_ROOT, "src/context.ts");
  const servePath = join(PROJECT_ROOT, "src/domains/runtime/commands/serve.ts");
  const guardianPath = join(
    PROJECT_ROOT,
    "src/domains/runtime/backend-guardian.ts",
  );
  return `
import { CATALOG } from ${JSON.stringify(catalogPath)};
import { createAppContext } from ${JSON.stringify(contextPath)};
import { runServe } from ${JSON.stringify(servePath)};
import { BACKEND_GUARDIAN_COMMAND, runBackendGuardian } from ${JSON.stringify(guardianPath)};

const cliArgs = Bun.argv.slice(2);
if (cliArgs[0] === BACKEND_GUARDIAN_COMMAND) {
  process.exit(await runBackendGuardian(cliArgs.slice(1)));
}

(CATALOG as any).push(JSON.parse(process.env.LOCALBASE_TEST_MODEL!));
const args = JSON.parse(process.env.LOCALBASE_TEST_ARGS!);
await runServe(args, await createAppContext(args));
`;
}

async function startArtifactServer(files: Record<string, Uint8Array>): Promise<{
  source: string;
  requests: string[];
  stop: () => Promise<void>;
}> {
  const requests: string[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const path = new URL(request.url).pathname;
      requests.push(path);
      const body = files[path];
      if (!body) {
        return new Response(null, { status: 404 });
      }
      const bodyCopy = new Uint8Array(body);
      return new Response(bodyCopy.buffer, {
        headers: { "Content-Length": String(body.byteLength) },
      });
    },
  });
  return {
    source: `http://127.0.0.1:${server.port}/repo`,
    requests,
    stop: async () => {
      server.stop(true);
    },
  };
}

test(
  "lazy llama startup reloads configured slots and preserves model switching",
  async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-lazy-parallel-"));
    const argsPath = join(root, "bin", "llama-server.args");
    const wrapperPort = reservePort();
    const backendPort = reservePort();
    const backend = Bun.serve({
      hostname: "127.0.0.1",
      port: backendPort,
      fetch: (request) => {
        if (new URL(request.url).pathname === "/health") {
          return Response.json({ status: "ok" });
        }
        return Response.json({
          id: "chatcmpl-test",
          object: "chat.completion",
          created: 0,
          model: SWITCHED_MODEL,
          choices: [
            {
              index: 0,
              message: { role: "assistant", content: "ok" },
              finish_reason: "stop",
            },
          ],
        });
      },
    });
    let gateway: Bun.Subprocess | undefined;
    let gatewayStdout: Promise<string> | undefined;
    let gatewayStderr: Promise<string> | undefined;

    try {
      const config = defaultConfig(root, 16);
      config.port = backendPort;
      config.ctxSize = 8192;
      config.parallel = 1;
      config.activeLlmModel = INITIAL_MODEL;
      config.selectedLlmModels = [INITIAL_MODEL, SWITCHED_MODEL];
      config.activeSttModel = "";
      config.selectedSttModels = [];
      config.activeImageModel = "";
      config.selectedImageModels = [];
      saveConfig(config);

      mkdirSync(join(root, "bin"), { recursive: true });
      await Promise.all([
        Bun.write(
          join(config.llmModelsDir, `${INITIAL_MODEL}.gguf`),
          "model placeholder",
        ),
        Bun.write(
          join(config.llmModelsDir, `${SWITCHED_MODEL}.gguf`),
          "model placeholder",
        ),
        Bun.write(
          join(root, "bin", "llama-server"),
          `#!/bin/sh
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
printf '%s\\n' "$@" > "$script_dir/llama-server.args"
exec sleep 600
`,
        ),
      ]);
      chmodSync(join(root, "bin", "llama-server"), 0o755);

      gateway = Bun.spawn(
        [
          process.execPath,
          "run",
          "src/cli.ts",
          "serve",
          "--root",
          root,
          "--host",
          "127.0.0.1",
          "--port",
          String(wrapperPort),
          "--llm-port",
          String(backendPort),
          "--ctx-size",
          "8192",
          "--llm",
          "true",
          "--stt",
          "false",
          "--image",
          "false",
          "--auth",
          "false",
          "--bypass-memory-check",
        ],
        {
          cwd: PROJECT_ROOT,
          env: {
            ...process.env,
            LOCALBASE_TEST_DISABLE_CONTINUE_SYNC: "1",
          } as Record<string, string>,
          stdout: "pipe",
          stderr: "pipe",
        },
      );
      gatewayStdout = readProcessOutput(gateway.stdout);
      gatewayStderr = readProcessOutput(gateway.stderr);

      const baseUrl = `http://127.0.0.1:${wrapperPort}`;
      await waitForGateway(gateway, baseUrl);

      const configure = Bun.spawn(
        [
          process.execPath,
          "run",
          "src/cli.ts",
          "configure",
          "--root",
          root,
          "--defaults",
          "--parallel",
          "3",
          "--create-key",
          "false",
        ],
        {
          cwd: PROJECT_ROOT,
          env: {
            ...process.env,
            LOCALBASE_TEST_DISABLE_CONTINUE_SYNC: "1",
          } as Record<string, string>,
          stdout: "ignore",
          stderr: "ignore",
        },
      );
      expect(await configure.exited).toBe(0);
      expect(loadConfig(root).parallel).toBe(3);

      const response = await fetch(`${baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: SWITCHED_MODEL,
          messages: [{ role: "user", content: "hello" }],
        }),
      });
      expect(response.status).toBe(200);

      await waitForFile(argsPath);
      const launchArgs = (await Bun.file(argsPath).text()).trim().split("\n");
      expect(launchArgs[launchArgs.indexOf("--parallel") + 1]).toBe("3");
      expect(launchArgs[launchArgs.indexOf("-m") + 1]).toBe(
        join(config.llmModelsDir, `${SWITCHED_MODEL}.gguf`),
      );
    } finally {
      if (gateway) await stopProcess(gateway);
      await Promise.all([gatewayStdout, gatewayStderr].filter(Boolean));
      backend.stop(true);
      rmSync(root, { recursive: true, force: true });
    }
  },
  { timeout: 20_000 },
);

test(
  "lazy llama startup repairs a missing shard before launching the primary shard",
  async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-lazy-shard-"));
    const argsPath = join(root, "bin", "llama-server.args");
    const wrapperPort = reservePort();
    const backendPort = reservePort();
    const modelId = "test-lazy-sharded-model";
    const primaryName = "test-lazy-00001-of-00002.gguf";
    const supplementaryName = "test-lazy-00002-of-00002.gguf";
    const primary = textBytes("primary shard");
    const supplementary = textBytes("supplementary shard");
    const artifactPath = `/repo/resolve/test-revision/${supplementaryName}`;
    const artifacts = await startArtifactServer({
      [`/repo/resolve/test-revision/${primaryName}`]: primary,
      [artifactPath]: supplementary,
    });
    const backend = Bun.serve({
      hostname: "127.0.0.1",
      port: backendPort,
      fetch: (request) => {
        if (new URL(request.url).pathname === "/health") {
          return Response.json({ status: "ok" });
        }
        return Response.json({
          id: "chatcmpl-test",
          object: "chat.completion",
          created: 0,
          model: modelId,
          choices: [
            {
              index: 0,
              message: { role: "assistant", content: "ok" },
              finish_reason: "stop",
            },
          ],
        });
      },
    });
    let gateway: Bun.Subprocess | undefined;
    let gatewayStdout: Promise<string> | undefined;
    let gatewayStderr: Promise<string> | undefined;

    try {
      const config = defaultConfig(root, 16);
      config.port = backendPort;
      config.activeLlmModel = modelId;
      config.selectedLlmModels = [modelId];
      config.activeSttModel = "";
      config.selectedSttModels = [];
      config.activeImageModel = "";
      config.selectedImageModels = [];
      saveConfig(config);

      mkdirSync(join(root, "bin"), { recursive: true });
      mkdirSync(config.llmModelsDir, { recursive: true });
      await Bun.write(join(config.llmModelsDir, primaryName), primary);
      await Bun.write(
        join(config.llmModelsDir, supplementaryName),
        supplementary,
      );
      await Bun.write(
        join(root, "bin", "llama-server"),
        `#!/bin/sh
test -f "$LOCALBASE_TEST_SUPPLEMENTARY_PATH" || exit 41
printf '%s\\n' "$@" > "$LOCALBASE_TEST_ARGS_PATH"
exec sleep 600
`,
      );
      chmodSync(join(root, "bin", "llama-server"), 0o755);

      const model = {
        modelId,
        kind: "llm",
        provider: "Test",
        family: "Test",
        version: "1",
        size: "1B",
        quant: "Q4_K_M",
        minVramGb: 1,
        storageGb: 1,
        source: artifacts.source,
        repositoryRevision: "test-revision",
        artifacts: [
          {
            sourcePath: primaryName,
            filename: primaryName,
            expectedSizeBytes: primary.byteLength,
            sha256: new Bun.CryptoHasher("sha256")
              .update(primary)
              .digest("hex"),
            role: "primary",
          },
          {
            sourcePath: supplementaryName,
            filename: supplementaryName,
            expectedSizeBytes: supplementary.byteLength,
            sha256: new Bun.CryptoHasher("sha256")
              .update(supplementary)
              .digest("hex"),
            role: "supplementary",
          },
        ],
        inputModalities: ["text"],
        outputModalities: ["text"],
        features: ["test"],
        commercialStatus: "open",
        catch: "Test only.",
        notes: "Test only.",
      };
      const serveArgs = [
        "--root",
        root,
        "--host",
        "127.0.0.1",
        "--port",
        String(wrapperPort),
        "--llm-port",
        String(backendPort),
        "--llm",
        "true",
        "--stt",
        "false",
        "--image",
        "false",
        "--auth",
        "false",
        "--bypass-memory-check",
      ];
      gateway = Bun.spawn([process.execPath, "--eval", serveRunnerSource()], {
        cwd: PROJECT_ROOT,
        env: {
          ...process.env,
          LOCALBASE_TEST_DISABLE_CONTINUE_SYNC: "1",
          LOCALBASE_TEST_MODEL: JSON.stringify(model),
          LOCALBASE_TEST_ARGS: JSON.stringify(serveArgs),
          LOCALBASE_TEST_SUPPLEMENTARY_PATH: join(
            config.llmModelsDir,
            supplementaryName,
          ),
          LOCALBASE_TEST_ARGS_PATH: argsPath,
        } as Record<string, string>,
        stdout: "pipe",
        stderr: "pipe",
      });
      gatewayStdout = readProcessOutput(gateway.stdout);
      gatewayStderr = readProcessOutput(gateway.stderr);

      const baseUrl = `http://127.0.0.1:${wrapperPort}`;
      await waitForGateway(gateway, baseUrl);
      await Bun.file(join(config.llmModelsDir, supplementaryName)).delete();

      const response = await fetch(`${baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: modelId,
          messages: [{ role: "user", content: "hello" }],
        }),
      });
      expect(response.status).toBe(200);
      await waitForFile(argsPath);

      expect(artifacts.requests).toEqual([artifactPath]);
      expect(
        await Bun.file(join(config.llmModelsDir, supplementaryName)).bytes(),
      ).toEqual(supplementary);
      const launchArgs = (await Bun.file(argsPath).text()).trim().split("\n");
      expect(launchArgs[launchArgs.indexOf("-m") + 1]).toBe(
        join(config.llmModelsDir, primaryName),
      );
      expect(launchArgs).not.toContain(
        join(config.llmModelsDir, supplementaryName),
      );
    } finally {
      if (gateway) await stopProcess(gateway);
      await Promise.all([gatewayStdout, gatewayStderr].filter(Boolean));
      backend.stop(true);
      await artifacts.stop();
      rmSync(root, { recursive: true, force: true });
    }
  },
  { timeout: 20_000 },
);

test(
  "gateway SIGKILL causes its guardian to reap a TERM-ignoring explicit LLM backend",
  async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-explicit-model-"));
    const argsPath = join(root, "bin", "llama-server.args");
    const pidPath = join(root, "bin", "llama-server.pid");
    const parentPidPath = join(root, "bin", "llama-server.parent.pid");
    const wrapperPort = reservePort();
    const backendPort = reservePort();
    const modelFile = "custom-model.gguf";
    const backend = Bun.serve({
      hostname: "127.0.0.1",
      port: backendPort,
      fetch: (request) => {
        if (new URL(request.url).pathname === "/health") {
          return Response.json({ status: "ok" });
        }
        return Response.json({
          id: "chatcmpl-test",
          object: "chat.completion",
          created: 0,
          model: "custom-model",
          choices: [
            {
              index: 0,
              message: { role: "assistant", content: "ok" },
              finish_reason: "stop",
            },
          ],
        });
      },
    });
    let gateway: Bun.Subprocess | undefined;
    let gatewayStdout: Promise<string> | undefined;
    let gatewayStderr: Promise<string> | undefined;

    try {
      const config = defaultConfig(root, 64);
      config.port = backendPort;
      config.activeLlmModel = "qwen3-coder-next-q4_k_m";
      config.selectedLlmModels = [config.activeLlmModel];
      config.activeSttModel = "";
      config.selectedSttModels = [];
      config.activeImageModel = "";
      config.selectedImageModels = [];
      saveConfig(config);

      mkdirSync(join(root, "bin"), { recursive: true });
      await Bun.write(join(config.llmModelsDir, modelFile), "custom model");
      await Bun.write(
        join(root, "bin", "llama-server"),
        `#!/bin/sh
trap '' TERM
printf '%s\\n' "$$" > "$LOCALBASE_TEST_PID_PATH"
printf '%s\\n' "$PPID" > "$LOCALBASE_TEST_PARENT_PID_PATH"
printf '%s\\n' "$@" > "$LOCALBASE_TEST_ARGS_PATH"
exec sleep 600
`,
      );
      chmodSync(join(root, "bin", "llama-server"), 0o755);

      gateway = Bun.spawn(
        [
          process.execPath,
          "run",
          "src/cli.ts",
          "serve",
          "--root",
          root,
          "--host",
          "127.0.0.1",
          "--port",
          String(wrapperPort),
          "--llm-port",
          String(backendPort),
          "--llm-model-file",
          modelFile,
          "--llm",
          "true",
          "--stt",
          "false",
          "--image",
          "false",
          "--auth",
          "false",
          "--bypass-memory-check",
        ],
        {
          cwd: PROJECT_ROOT,
          env: {
            ...process.env,
            LOCALBASE_TEST_DISABLE_CONTINUE_SYNC: "1",
            LOCALBASE_TEST_ARGS_PATH: argsPath,
            LOCALBASE_TEST_PID_PATH: pidPath,
            LOCALBASE_TEST_PARENT_PID_PATH: parentPidPath,
          } as Record<string, string>,
          stdout: "pipe",
          stderr: "pipe",
        },
      );
      gatewayStdout = readProcessOutput(gateway.stdout);
      gatewayStderr = readProcessOutput(gateway.stderr);

      const baseUrl = `http://127.0.0.1:${wrapperPort}`;
      await waitForGateway(gateway, baseUrl);
      const response = await fetch(`${baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: config.activeLlmModel,
          messages: [{ role: "user", content: "hello" }],
        }),
      });
      expect(response.status).toBe(200);
      await waitForFile(argsPath);

      const launchArgs = (await Bun.file(argsPath).text()).trim().split("\n");
      expect(launchArgs[launchArgs.indexOf("-m") + 1]).toBe(
        join(config.llmModelsDir, modelFile),
      );
      expect(
        await Bun.file(
          join(
            config.llmModelsDir,
            "Qwen3-Coder-Next-Q4_K_M-00002-of-00004.gguf",
          ),
        ).exists(),
      ).toBe(false);

      const backendPid = Number((await Bun.file(pidPath).text()).trim());
      expect(Number.isInteger(backendPid)).toBe(true);
      expect(Number((await Bun.file(parentPidPath).text()).trim())).toBe(
        gateway.pid,
      );
      const guardianPid = await findGuardianPid(gateway.pid, backendPid);
      await Bun.sleep(300);
      expect(isProcessRunning(guardianPid)).toBe(true);
      gateway.kill(9);
      await gateway.exited;
      await waitForProcessExit(backendPid);
      await waitForProcessExit(guardianPid);
    } finally {
      if (gateway) await stopProcess(gateway);
      await Promise.all([gatewayStdout, gatewayStderr].filter(Boolean));
      backend.stop(true);
      rmSync(root, { recursive: true, force: true });
    }
  },
  { timeout: 20_000 },
);
