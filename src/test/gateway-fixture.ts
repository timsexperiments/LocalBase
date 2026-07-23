import { chmodSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { defaultConfig, saveConfig, type LocalBaseConfig } from "../manager";
import { DatabaseSession } from "../db/client";

const LLM_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const STT_MODEL = "whisper-large-v3-turbo";
const IMAGE_MODEL = "stable-diffusion-v1-5";
const PROJECT_ROOT = join(import.meta.dirname, "../..");
const MAX_START_ATTEMPTS = 5;
type UpstreamRequest = {
  path: string;
  headers: Headers;
  body: string;
};

export type GatewayFixture = {
  baseUrl: string;
  root: string;
  upstreamRequests: UpstreamRequest[];
  stop: () => Promise<void>;
};

async function readProcessOutput(
  stream: ReadableStream<Uint8Array> | number | undefined,
): Promise<string> {
  if (!stream || typeof stream === "number") return "";
  return await new Response(stream).text();
}

function reservePort(): number {
  for (let attempt = 0; attempt < 20; attempt++) {
    const random = new Uint32Array(1);
    crypto.getRandomValues(random);
    const port = 20_000 + (random[0] % 40_000);
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
  throw new Error("Could not reserve a test port.");
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

async function readGatewayBaseUrl(
  stream: ReadableStream<Uint8Array>,
): Promise<string> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let output = "";
  const timeout = setTimeout(() => reader.cancel(), 15_000);
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      output += decoder.decode(value, { stream: true });
      const match = output.match(/Wrapper base URL: (http:\/\/[^\s]+)/);
      if (match) return match[1];
    }
  } finally {
    clearTimeout(timeout);
    reader.releaseLock();
  }
  throw new Error(`Gateway did not report its bound URL. Output:\n${output}`);
}

function startMockUpstream(requests: UpstreamRequest[]): Bun.Server<undefined> {
  const options = {
    hostname: "127.0.0.1",
    async fetch(request: Request) {
      const path = new URL(request.url).pathname;
      if (path === "/health") return Response.json({ status: "ok" });

      const body = await request.text();
      requests.push({ path, headers: new Headers(request.headers), body });
      const mode = request.headers.get("x-test-upstream");
      if (mode === "malformed") return new Response("not json");
      if (mode === "invalid-schema") return Response.json({ unexpected: true });
      if (mode === "stream") {
        return new Response('data: {"ok":true}\n\ndata: [DONE]\n\n', {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (path === "/v1/images/generations") {
        return Response.json({ created: 0, data: [{ b64_json: "test" }] });
      }
      if (path.startsWith("/v1/audio/")) return Response.json({ text: "ok" });
      if (path === "/v1/embeddings") {
        return Response.json({
          object: "list",
          data: [{ object: "embedding", index: 0, embedding: [0] }],
          model: LLM_MODEL,
          usage: { prompt_tokens: 1, total_tokens: 1 },
        });
      }
      if (path === "/v1/completions") {
        return Response.json({
          id: "cmpl-test",
          object: "text_completion",
          created: 0,
          model: LLM_MODEL,
          choices: [{ text: "ok", index: 0, finish_reason: "stop" }],
        });
      }
      return Response.json({
        id: "chatcmpl-test",
        object: "chat.completion",
        created: 0,
        model: LLM_MODEL,
        choices: [
          {
            index: 0,
            message: { role: "assistant", content: "ok" },
            finish_reason: "stop",
          },
        ],
      });
    },
  };
  for (let attempt = 0; attempt < MAX_START_ATTEMPTS; attempt++) {
    try {
      return Bun.serve({ ...options, port: reservePort() });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EADDRINUSE") throw error;
    }
  }
  throw new Error("Could not start mock upstream.");
}

function boundPort(server: Bun.Server<undefined>): number {
  if (server.port === undefined)
    throw new Error("Mock upstream did not bind a port.");
  return server.port;
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
  const runtimeDir = join(root, "test-runtimes");
  const cleanup = () => rmSync(root, { recursive: true, force: true });
  const upstreamRequests: UpstreamRequest[] = [];
  const llmUpstream = startMockUpstream(upstreamRequests);
  const sttUpstream = startMockUpstream(upstreamRequests);
  const imageUpstream = startMockUpstream(upstreamRequests);
  const llmPort = boundPort(llmUpstream);
  const sttPort = boundPort(sttUpstream);
  const imagePort = boundPort(imageUpstream);

  let config: LocalBaseConfig;
  try {
    config = defaultConfig(root);
    config.port = llmPort;
    config.sttPort = sttPort;
    config.activeLlmModel = LLM_MODEL;
    config.selectedLlmModels = [LLM_MODEL];
    config.activeSttModel = STT_MODEL;
    config.selectedSttModels = [STT_MODEL];
    config.activeImageModel = IMAGE_MODEL;
    config.selectedImageModels = [IMAGE_MODEL];
    const database = new DatabaseSession();
    saveConfig(database, config);
    database.close();

    mkdirSync(config.llmModelsDir, { recursive: true });
    mkdirSync(config.sttModelsDir, { recursive: true });
    mkdirSync(config.imageModelsDir, { recursive: true });
    mkdirSync(runtimeDir, { recursive: true });
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
      Bun.write(
        join(runtimeDir, "llama-server"),
        "#!/bin/sh\nexec sleep 600\n",
      ),
      Bun.write(
        join(runtimeDir, "whisper-server"),
        "#!/bin/sh\nexec sleep 600\n",
      ),
      Bun.write(join(runtimeDir, "sd-server"), "#!/bin/sh\nexec sleep 600\n"),
    ]);
    for (const binary of ["llama-server", "whisper-server", "sd-server"]) {
      chmodSync(join(runtimeDir, binary), 0o755);
    }
  } catch (error) {
    llmUpstream.stop(true);
    sttUpstream.stop(true);
    imageUpstream.stop(true);
    cleanup();
    throw error;
  }

  let serverProcess: Bun.Subprocess | undefined;
  let stdout: Promise<string> | undefined;
  let stderr: Promise<string> | undefined;
  let baseUrl = "";
  let lastError: unknown;
  for (let attempt = 0; attempt < MAX_START_ATTEMPTS; attempt++) {
    const port = reservePort();
    const gatewayProcess = Bun.spawn(
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
        "--llm-model-file",
        `${LLM_MODEL}.gguf`,
        "--llm-port",
        String(llmPort),
        "--stt-port",
        String(sttPort),
        "--image-port",
        String(imagePort),
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
          PATH: `${runtimeDir}:${process.env.PATH ?? ""}`,
          LOCALBASE_TEST_DISABLE_CONTINUE_SYNC: "1",
        } as Record<string, string>,
        stdout: "pipe",
        stderr: "pipe",
      },
    );
    if (!gatewayProcess.stdout || typeof gatewayProcess.stdout === "number") {
      await stopProcess(gatewayProcess);
      lastError = new Error("Gateway process did not provide stdout.");
      continue;
    }
    const [logStream, startupStream] = gatewayProcess.stdout.tee();
    const processStdout = readProcessOutput(logStream);
    const processStderr = readProcessOutput(gatewayProcess.stderr);
    try {
      baseUrl = await readGatewayBaseUrl(startupStream);
      await waitForReady(gatewayProcess, baseUrl);
      serverProcess = gatewayProcess;
      stdout = processStdout;
      stderr = processStderr;
      break;
    } catch (error) {
      lastError = error;
      await stopProcess(gatewayProcess);
      const [out, err] = await Promise.all([processStdout, processStderr]);
      const diagnostics = `${out}\n${err}`.toLowerCase();
      if (
        attempt === MAX_START_ATTEMPTS - 1 ||
        !diagnostics.includes("eaddrinuse")
      ) {
        lastError = new Error(`${error}\nstdout:\n${out}\nstderr:\n${err}`, {
          cause: error,
        });
        break;
      }
    }
  }

  if (!serverProcess || !stdout || !stderr) {
    llmUpstream.stop(true);
    sttUpstream.stop(true);
    imageUpstream.stop(true);
    cleanup();
    cleanup();
    throw lastError instanceof Error
      ? lastError
      : new Error("Gateway process was not created.");
  }

  return {
    baseUrl,
    root,
    upstreamRequests,
    stop: async () => {
      await stopProcess(serverProcess);
      await Promise.all([stdout, stderr]);
      llmUpstream.stop(true);
      sttUpstream.stop(true);
      imageUpstream.stop(true);
      cleanup();
    },
  };
}
