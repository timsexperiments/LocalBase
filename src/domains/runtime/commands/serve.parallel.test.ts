import { expect, test } from "bun:test";
import { randomInt } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { defaultConfig, loadConfig, saveConfig } from "../../../manager";

const INITIAL_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const SWITCHED_MODEL = "qwen2.5-coder-7b-instruct-q4_k_m";
const PROJECT_ROOT = join(import.meta.dirname, "../../../..");

function reservePort(): number {
  for (let attempt = 0; attempt < 10; attempt++) {
    const port = randomInt(20_000, 60_000);
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
    if (existsSync(path)) return;
    await Bun.sleep(20);
  }
  throw new Error(`Timed out waiting for ${path}`);
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
      const launchArgs = readFileSync(argsPath, "utf8").trim().split("\n");
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
