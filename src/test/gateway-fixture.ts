import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { randomInt } from "node:crypto";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { defaultConfig, saveConfig, type LocalBaseConfig } from "../manager";

const LLM_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const STT_MODEL = "whisper-large-v3-turbo";
const IMAGE_MODEL = "stable-diffusion-v1-5";
const PROJECT_ROOT = join(import.meta.dirname, "../..");
const MAX_START_ATTEMPTS = 5;

export type GatewayFixture = {
  baseUrl: string;
  root: string;
  stop: () => Promise<void>;
};

async function readProcessOutput(
  stream: ReadableStream<Uint8Array> | number | undefined,
): Promise<string> {
  if (!stream || typeof stream === "number") return "";
  return await new Response(stream).text();
}

function getGatewayPort(): number {
  return randomInt(20_000, 60_000);
}

async function stopProcess(serverProcess: Bun.Subprocess): Promise<void> {
  if (serverProcess.exitCode === null) serverProcess.kill(15);
  await Promise.race([
    serverProcess.exited,
    Bun.sleep(5_000).then(() => {
      if (serverProcess.exitCode === null) serverProcess.kill(9);
    }),
  ]);
  await serverProcess.exited;
}

async function waitForReady(
  serverProcess: Bun.Subprocess,
  baseUrl: string,
): Promise<void> {
  const deadline = Date.now() + 15_000;
  let lastStatus = "no response";

  while (Date.now() < deadline) {
    if (serverProcess.exitCode !== null) {
      throw new Error(
        `Gateway exited before readiness (code ${serverProcess.exitCode}).`,
      );
    }

    try {
      const response = await fetch(`${baseUrl}/health`, {
        signal: AbortSignal.timeout(500),
      });
      lastStatus = `HTTP ${response.status}`;
      if (response.ok) {
        const body = (await response.json()) as { status?: string };
        if (body.status === "ok") return;
        lastStatus = `unexpected health payload: ${JSON.stringify(body)}`;
      }
    } catch (error) {
      lastStatus = error instanceof Error ? error.message : String(error);
    }

    await Bun.sleep(100);
  }

  throw new Error(
    `Gateway did not become ready within 15 seconds (${lastStatus}).`,
  );
}

export async function startGatewayFixture(): Promise<GatewayFixture> {
  const root = mkdtempSync(join(tmpdir(), "localbase-gateway-"));
  const cleanup = () => rmSync(root, { recursive: true, force: true });

  let config: LocalBaseConfig;
  try {
    config = defaultConfig(root);
    config.port = 0;
    config.sttPort = 0;
    config.activeLlmModel = LLM_MODEL;
    config.selectedLlmModels = [LLM_MODEL];
    config.activeSttModel = STT_MODEL;
    config.selectedSttModels = [STT_MODEL];
    config.activeImageModel = IMAGE_MODEL;
    config.selectedImageModels = [IMAGE_MODEL];
    saveConfig(config);

    mkdirSync(config.llmModelsDir, { recursive: true });
    mkdirSync(config.sttModelsDir, { recursive: true });
    mkdirSync(config.imageModelsDir, { recursive: true });
    await Promise.all([
      Bun.write(
        join(config.llmModelsDir, `${LLM_MODEL}.gguf`),
        "test model placeholder",
      ),
      Bun.write(
        join(config.sttModelsDir, "ggml-large-v3-turbo.bin"),
        "test model placeholder",
      ),
      Bun.write(
        join(config.imageModelsDir, "v1-5-pruned-emaonly.safetensors"),
        "test model placeholder",
      ),
    ]);
  } catch (error) {
    cleanup();
    throw error;
  }

  let port = 0;
  let serverProcess: Bun.Subprocess | undefined;
  let stdout: Promise<string> | undefined;
  let stderr: Promise<string> | undefined;

  for (let attempt = 0; attempt < MAX_START_ATTEMPTS; attempt++) {
    port = getGatewayPort();
    const baseUrl = `http://127.0.0.1:${port}`;
    try {
      serverProcess = Bun.spawn(
        [
          "bun",
          "run",
          "src/cli.ts",
          "serve",
          "--root",
          root,
          "--host",
          "127.0.0.1",
          "--port",
          String(port),
          "--llm",
          "true",
          "--stt",
          "true",
          "--image",
          "true",
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
    } catch (error) {
      cleanup();
      throw error;
    }
    stdout = readProcessOutput(serverProcess.stdout);
    stderr = readProcessOutput(serverProcess.stderr);

    try {
      await waitForReady(serverProcess, baseUrl);
      break;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      await stopProcess(serverProcess);
      const [out, err] = await Promise.all([stdout, stderr]);
      const diagnostics = `${message}\n${out}\n${err}`.toLowerCase();
      if (
        attempt < MAX_START_ATTEMPTS - 1 &&
        (diagnostics.includes("eaddrinuse") ||
          diagnostics.includes("address already in use") ||
          (diagnostics.includes("port ") && diagnostics.includes(" in use")))
      ) {
        continue;
      }
      cleanup();
      throw new Error(`${message}\nstdout:\n${out}\nstderr:\n${err}`, {
        cause: error,
      });
    }
  }

  if (!serverProcess || !stdout || !stderr) {
    cleanup();
    throw new Error("Gateway process was not created.");
  }

  const baseUrl = `http://127.0.0.1:${port}`;

  return {
    baseUrl,
    root,
    stop: async () => {
      await stopProcess(serverProcess);
      await Promise.all([stdout, stderr]);
      cleanup();
    },
  };
}
