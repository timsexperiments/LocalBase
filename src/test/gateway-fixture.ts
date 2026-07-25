import { mkdirSync, mkdtempSync, rmSync, truncateSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { byId, primaryArtifact } from "../catalog";
import {
  createApiKey,
  defaultConfig,
  loadConfig,
  saveConfig,
  type LocalBaseConfig,
} from "../manager";
import { DatabaseSession } from "../db/client";
import { compileRuntimeFixture } from "./runtime-fixture";

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

type ControlledStream = {
  close: () => void;
  aborted: Promise<void>;
};

type ControlledHeaderWait = {
  aborted: Promise<void>;
};

export async function writeCompleteCatalogArtifact(
  directory: string,
  modelId: string,
): Promise<string> {
  const model = byId(modelId);
  if (!model) throw new Error(`Unknown catalog model: ${modelId}`);
  const artifact = primaryArtifact(model);
  if (artifact.expectedSizeBytes === undefined) {
    throw new Error(`Catalog model ${modelId} has no expected artifact size.`);
  }
  const path = join(directory, artifact.filename);
  await Bun.write(path, "");
  truncateSync(path, artifact.expectedSizeBytes);
  return path;
}

export type GatewayFixture = {
  baseUrl: string;
  root: string;
  apiKey?: string;
  upstreamRequests: UpstreamRequest[];
  readConfig: () => LocalBaseConfig;
  saveConfig: (config: LocalBaseConfig) => void;
  readLlmRuntimeLaunches: () => Promise<string[][]>;
  waitForLlmRuntimeLaunches: (
    offset: number,
    count: number,
  ) => Promise<string[][]>;
  waitForUpstreamRequest: (id: string) => Promise<void>;
  closeControlledStream: (id: string) => void;
  waitForControlledStreamAbort: (id: string) => Promise<void>;
  waitForControlledHeaderAbort: (id: string) => Promise<void>;
  stop: () => Promise<void>;
};

export type GatewayFixtureOptions = {
  auth?: { mode?: "bearer" | "x-api-key" | "either" };
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

async function compileGatewayCli(outputPath: string): Promise<void> {
  const build = Bun.spawn(
    [
      process.execPath,
      "build",
      "src/cli.ts",
      "--compile",
      "--target=bun",
      "--asset-naming=[dir]/[name].[ext]",
      `--outfile=${outputPath}`,
    ],
    { cwd: PROJECT_ROOT, stdout: "pipe", stderr: "pipe" },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    build.exited,
    readProcessOutput(build.stdout),
    readProcessOutput(build.stderr),
  ]);
  if (exitCode !== 0) {
    throw new Error(`Could not compile gateway CLI:\n${stdout}${stderr}`);
  }
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

function startMockUpstream(
  requests: UpstreamRequest[],
  controlledStreams: Map<string, ControlledStream>,
  controlledHeaderWaits: Map<string, ControlledHeaderWait>,
): Bun.Server<undefined> {
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
      if (mode === "custom-tool-response") {
        return Response.json({
          id: "chatcmpl-custom-tool",
          object: "chat.completion",
          created: 0,
          model: LLM_MODEL,
          choices: [
            {
              index: 0,
              message: {
                role: "assistant",
                content: null,
                tool_calls: [
                  {
                    id: "custom_1",
                    type: "custom",
                    custom: { name: "code_execution", input: "print(1)" },
                  },
                ],
              },
              finish_reason: "tool_calls",
            },
          ],
        });
      }
      if (mode === "deprecated-function-call-response") {
        return Response.json({
          id: "chatcmpl-function-call",
          object: "chat.completion",
          created: 0,
          model: LLM_MODEL,
          choices: [
            {
              index: 0,
              message: {
                role: "assistant",
                content: null,
                function_call: { name: "weather", arguments: "{}" },
              },
              finish_reason: "function_call",
            },
          ],
        });
      }
      if (mode === "null-refusal-response") {
        return Response.json({
          id: "chatcmpl-null-refusal",
          object: "chat.completion",
          created: 0,
          model: LLM_MODEL,
          choices: [
            {
              index: 0,
              message: { role: "assistant", content: null, refusal: null },
              finish_reason: "stop",
            },
          ],
        });
      }
      if (mode === "tool-round-trip") {
        const messages = JSON.parse(body) as {
          messages?: Array<{ role?: string; tool_call_id?: string }>;
        };
        const hasToolResult = messages.messages?.some(
          (message) =>
            message.role === "tool" && message.tool_call_id === "call_weather",
        );
        return Response.json({
          id: hasToolResult ? "chatcmpl-tool-result" : "chatcmpl-tool-call",
          object: "chat.completion",
          created: 0,
          model: LLM_MODEL,
          choices: [
            hasToolResult
              ? {
                  index: 0,
                  message: { role: "assistant", content: "73°F" },
                  finish_reason: "stop",
                }
              : {
                  index: 0,
                  message: {
                    role: "assistant",
                    content: null,
                    tool_calls: [
                      {
                        id: "call_weather",
                        type: "function",
                        function: {
                          name: "weather",
                          arguments: '{"city":"Austin"}',
                        },
                        extra_content: {
                          google: { thought_signature: "signature" },
                        },
                      },
                    ],
                    reasoning_content: "Looking up the weather.",
                  },
                  finish_reason: "tool_calls",
                },
          ],
        });
      }
      if (mode === "controlled-stream" || mode === "ai-sdk-controlled-stream") {
        const id = request.headers.get("x-test-stream-id");
        if (!id) return new Response("Missing stream id", { status: 400 });
        if (controlledStreams.has(id)) {
          return new Response("Duplicate stream id", { status: 409 });
        }

        let closed = false;
        let resolveAborted: () => void;
        const aborted = new Promise<void>((resolve) => {
          resolveAborted = resolve;
        });
        const abort = () => resolveAborted!();
        request.signal.addEventListener("abort", abort, { once: true });
        if (request.signal.aborted) abort();
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            controlledStreams.set(id, {
              close: () => {
                if (closed) return;
                closed = true;
                request.signal.removeEventListener("abort", abort);
                controller.enqueue(
                  new TextEncoder().encode("data: [DONE]\n\n"),
                );
                controller.close();
              },
              aborted,
            });
            controller.enqueue(
              new TextEncoder().encode(
                `data: ${JSON.stringify({
                  id: "chatcmpl-controlled",
                  object: "chat.completion.chunk",
                  created: 0,
                  model: LLM_MODEL,
                  choices: [
                    {
                      index: 0,
                      delta: { role: "assistant", content: "waiting" },
                      finish_reason: null,
                    },
                  ],
                })}\n\n`,
              ),
            );
          },
          cancel() {
            abort();
            closed = true;
            request.signal.removeEventListener("abort", abort);
          },
        });
        return new Response(body, {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (mode === "controlled-headers") {
        const id = request.headers.get("x-test-stream-id");
        if (!id) return new Response("Missing stream id", { status: 400 });
        if (controlledHeaderWaits.has(id)) {
          return new Response("Duplicate stream id", { status: 409 });
        }

        let releaseHeaders: () => void;
        let resolveAborted: () => void;
        const headersReleased = new Promise<void>((resolve) => {
          releaseHeaders = resolve;
        });
        const aborted = new Promise<void>((resolve) => {
          resolveAborted = resolve;
        });
        const abort = () => {
          resolveAborted!();
          releaseHeaders!();
        };
        request.signal.addEventListener("abort", abort, { once: true });
        if (request.signal.aborted) abort();
        controlledHeaderWaits.set(id, { aborted });
        await headersReleased;
        request.signal.removeEventListener("abort", abort);
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
      }
      if (mode === "stream") {
        return new Response(
          `data: ${JSON.stringify({
            id: "chatcmpl-stream",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [
              {
                index: 0,
                delta: { role: "assistant", content: "ok" },
                finish_reason: "stop",
              },
            ],
          })}\n\ndata: [DONE]\n\n`,
          {
            headers: {
              "content-type": "Text/Event-Stream; charset=utf-8",
              "x-stream-fixture": "preserved",
            },
          },
        );
      }
      if (mode === "invalid-stream") {
        const invalidEvent = `data: ${JSON.stringify({
          id: "chatcmpl-invalid-stream",
          object: "chat.completion.chunk",
          created: 0,
          model: LLM_MODEL,
          choices: [
            {
              index: 0,
              delta: {
                tool_calls: [{ index: 0, unexpected: true }],
              },
              finish_reason: null,
            },
          ],
        })}\n\n`;
        const splitAt = Math.floor(invalidEvent.length / 2);
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(
              new TextEncoder().encode(invalidEvent.slice(0, splitAt)),
            );
            controller.enqueue(
              new TextEncoder().encode(invalidEvent.slice(splitAt)),
            );
            controller.close();
          },
        });
        return new Response(body, {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (mode === "post-done-stream") {
        return new Response(
          `data: [DONE]\n\ndata: ${JSON.stringify({
            id: "chatcmpl-after-done",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [
              {
                index: 0,
                delta: { content: "too late" },
                finish_reason: null,
              },
            ],
          })}\n\n`,
          { headers: { "content-type": "text/event-stream" } },
        );
      }
      if (mode === "duplicate-done-stream") {
        return new Response("data: [DONE]\n\ndata: [DONE]\n\n", {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (mode === "invalid-media-type-stream") {
        return new Response(
          `data: ${JSON.stringify({
            id: "chatcmpl-wrong-media-type",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [
              {
                index: 0,
                delta: { content: "not an SSE response" },
                finish_reason: null,
              },
            ],
          })}\n\ndata: [DONE]\n\n`,
          { headers: { "content-type": "application/x-text/event-stream" } },
        );
      }
      if (mode === "truncated-ai-sdk-stream") {
        return new Response(
          `data: ${JSON.stringify({
            id: "chatcmpl-truncated",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [
              {
                index: 0,
                delta: { role: "assistant", content: "partial" },
                finish_reason: null,
              },
            ],
            usage: null,
          })}\n\n`,
          { headers: { "content-type": "text/event-stream" } },
        );
      }
      if (mode === "llama-wire-stream") {
        const chunks = [
          {
            id: "chatcmpl-llama-wire",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            system_fingerprint: "b9741",
            choices: [
              {
                index: 0,
                delta: { role: "assistant", content: null },
                finish_reason: null,
              },
            ],
            usage: null,
          },
          {
            id: "chatcmpl-llama-wire",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            system_fingerprint: "b9741",
            choices: [
              {
                index: 0,
                delta: { reasoning_content: "Considering tools." },
                finish_reason: null,
              },
            ],
            usage: null,
          },
          {
            id: "chatcmpl-llama-wire",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            system_fingerprint: "b9741",
            choices: [
              {
                index: 0,
                delta: {
                  tool_calls: [
                    {
                      index: 0,
                      id: "call_weather",
                      type: "function",
                      function: { name: "weather" },
                    },
                  ],
                },
                finish_reason: null,
              },
            ],
            usage: null,
          },
          {
            id: "chatcmpl-llama-wire",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            system_fingerprint: "b9741",
            choices: [
              {
                index: 0,
                delta: {
                  tool_calls: [
                    {
                      index: 0,
                      function: { arguments: '{"city":"Austin"}' },
                    },
                  ],
                },
                finish_reason: null,
              },
            ],
            usage: null,
          },
          {
            id: "chatcmpl-llama-wire",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            system_fingerprint: "b9741",
            choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
            usage: {
              prompt_tokens: 3,
              completion_tokens: 2,
              total_tokens: 5,
              prompt_tokens_details: { cached_tokens: 1 },
            },
            timings: {
              cache_n: 1,
              prompt_n: 2,
              prompt_ms: 3,
              prompt_per_token_ms: 1.5,
              prompt_per_second: 666.67,
              predicted_n: 2,
              predicted_ms: 4,
              predicted_per_token_ms: 2,
              predicted_per_second: 500,
            },
          },
        ];
        return new Response(
          `${chunks.map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`).join("")}data: [DONE]\n\n`,
          { headers: { "content-type": "text/event-stream" } },
        );
      }
      if (mode === "ai-sdk-stream") {
        const chunks = [
          {
            id: "chatcmpl-stream",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [
              {
                index: 0,
                delta: { role: "assistant", content: "hel" },
                finish_reason: null,
              },
            ],
            usage: null,
          },
          {
            id: "chatcmpl-stream",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [
              {
                index: 0,
                delta: { content: "lo" },
                finish_reason: null,
              },
            ],
            usage: null,
          },
          {
            id: "chatcmpl-stream",
            object: "chat.completion.chunk",
            created: 0,
            model: LLM_MODEL,
            choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
            usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
          },
        ];
        return new Response(
          `${chunks.map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`).join("")}data: [DONE]\n\n`,
          { headers: { "content-type": "text/event-stream" } },
        );
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
        usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
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

export async function startGatewayFixture(
  options: GatewayFixtureOptions = {},
): Promise<GatewayFixture> {
  const root = mkdtempSync(join(tmpdir(), "localbase-gateway-"));
  const runtimeDir = join(root, "test-runtimes");
  const cliPath = join(root, "local-base");
  const llmLaunchesPath = join(root, "llama-launches.jsonl");
  const cleanup = () => rmSync(root, { recursive: true, force: true });
  const upstreamRequests: UpstreamRequest[] = [];
  const controlledStreams = new Map<string, ControlledStream>();
  const controlledHeaderWaits = new Map<string, ControlledHeaderWait>();
  const llmUpstream = startMockUpstream(
    upstreamRequests,
    controlledStreams,
    controlledHeaderWaits,
  );
  const sttUpstream = startMockUpstream(
    upstreamRequests,
    controlledStreams,
    controlledHeaderWaits,
  );
  const imageUpstream = startMockUpstream(
    upstreamRequests,
    controlledStreams,
    controlledHeaderWaits,
  );
  const llmPort = boundPort(llmUpstream);
  const sttPort = boundPort(sttUpstream);
  const imagePort = boundPort(imageUpstream);

  let config: LocalBaseConfig;
  let apiKey: string | undefined;
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
    if (options.auth) {
      apiKey = createApiKey(database, config, "conformance").rawKey;
    }
    database.close();

    mkdirSync(config.llmModelsDir, { recursive: true });
    mkdirSync(config.sttModelsDir, { recursive: true });
    mkdirSync(config.imageModelsDir, { recursive: true });
    mkdirSync(runtimeDir, { recursive: true });
    await Promise.all([
      writeCompleteCatalogArtifact(config.llmModelsDir, LLM_MODEL),
      Bun.write(
        join(config.sttModelsDir, "ggml-large-v3-turbo.bin"),
        "test model placeholder",
      ),
      Bun.write(
        join(config.imageModelsDir, "v1-5-pruned-emaonly.safetensors"),
        "test model placeholder",
      ),
      compileRuntimeFixture(
        join(runtimeDir, "llama-server"),
        undefined,
        llmLaunchesPath,
      ),
      compileRuntimeFixture(join(runtimeDir, "whisper-server")),
      compileRuntimeFixture(join(runtimeDir, "sd-server")),
      compileGatewayCli(cliPath),
    ]);
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
        cliPath,
        "serve",
        "--root",
        root,
        "--host",
        "127.0.0.1",
        "--port",
        String(port),
        "--llm-port",
        String(llmPort),
        "--stt-port",
        String(sttPort),
        "--image-port",
        String(imagePort),
        ...(options.auth
          ? ["--auth-mode", options.auth.mode ?? "bearer"]
          : ["--no-auth"]),
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
    throw lastError instanceof Error
      ? lastError
      : new Error("Gateway process was not created.");
  }

  const readLlmRuntimeLaunches = async (): Promise<string[][]> => {
    const file = Bun.file(llmLaunchesPath);
    if (!(await file.exists())) return [];
    return (await file.text())
      .trim()
      .split("\n")
      .filter(Boolean)
      .map((line) => JSON.parse(line) as string[]);
  };
  const waitForLlmRuntimeLaunches = async (
    offset: number,
    count: number,
  ): Promise<string[][]> => {
    const deadline = Date.now() + 2_000;
    while (Date.now() < deadline) {
      const launches = await readLlmRuntimeLaunches();
      if (launches.length >= offset + count) return launches.slice(offset);
      await Bun.sleep(10);
    }
    const launches = await readLlmRuntimeLaunches();
    throw new Error(
      `Expected ${count} LLM runtime launches, received ${launches.length - offset}: ${JSON.stringify(launches.slice(offset))}`,
    );
  };

  return {
    baseUrl,
    root,
    apiKey,
    upstreamRequests,
    readConfig() {
      const database = new DatabaseSession();
      try {
        return loadConfig(database, root);
      } finally {
        database.close();
      }
    },
    saveConfig(nextConfig) {
      const database = new DatabaseSession();
      try {
        saveConfig(database, nextConfig);
      } finally {
        database.close();
      }
    },
    readLlmRuntimeLaunches,
    waitForLlmRuntimeLaunches,
    async waitForUpstreamRequest(id) {
      const deadline = Date.now() + 2_000;
      while (Date.now() < deadline) {
        if (
          upstreamRequests.some(
            (upstream) => upstream.headers.get("x-test-stream-id") === id,
          )
        ) {
          return;
        }
        await Bun.sleep(10);
      }
      throw new Error(
        `Upstream request ${id} did not start within two seconds.`,
      );
    },
    closeControlledStream(id) {
      const stream = controlledStreams.get(id);
      if (!stream) throw new Error(`Unknown controlled stream: ${id}`);
      stream.close();
    },
    waitForControlledStreamAbort(id) {
      const stream = controlledStreams.get(id);
      if (!stream) throw new Error(`Unknown controlled stream: ${id}`);
      return stream.aborted;
    },
    waitForControlledHeaderAbort(id) {
      const headerWait = controlledHeaderWaits.get(id);
      if (!headerWait) throw new Error(`Unknown controlled header wait: ${id}`);
      return headerWait.aborted;
    },
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
