import { z } from "zod";
import { join, basename } from "node:path";
import {
  startLlamaServerProcess,
  startWhisperServerProcess,
  startSdServerProcess,
  validateApiKey,
  installModel,
  saveConfig,
  loadConfig,
} from "../../../manager";
import {
  byId,
  evaluateModelFit,
  calculateMaxSafeContextSize,
  primaryArtifact,
  resolveCatalogInstallation,
} from "../../../catalog";
import type { AppContext } from "../../../context";
import { parseBool, parseFlag, toInt } from "../../../utils/args";
import { syncContinueConfig } from "../../config/commands/configure";
import { type ILogger } from "../../../utils/logger";
import { DEFAULT_SYSTEM_PROMPT } from "./prompt";
import { guardianProcessCommand } from "../backend-guardian";

type AuthMode = "bearer" | "x-api-key" | "either";

type ModalityState = {
  llm: boolean;
  stt: boolean;
  image: boolean;
};

const CHILD_STOP_GRACE_MS = 500;
const HEALTH_PROBE_TIMEOUT_MS = 2_000;
const UPSTREAM_PROXY_TIMEOUT_MS = 120_000;
const MAX_REQUEST_BYTES = 25 * 1024 * 1024;

function parseAuthMode(raw: string | undefined): AuthMode {
  if (!raw) return "either";
  if (raw === "bearer" || raw === "x-api-key" || raw === "either") return raw;
  throw new Error(
    `Invalid --auth-mode value: ${raw}. Expected bearer|x-api-key|either`,
  );
}

function printUnifiedNextSteps(
  host: string,
  port: number,
  llmPort: number,
  sttPort: number,
  imagePort: number,
  authRequired: boolean,
  authMode: AuthMode,
  enabled: ModalityState,
): void {
  console.log("\nUnified API wrapper started.");
  console.log(`Wrapper base URL: http://${host}:${port}`);
  if (enabled.llm)
    console.log(`OpenAI-compatible LLM endpoint: http://${host}:${port}/v1`);
  if (enabled.stt)
    console.log(
      `OpenAI-compatible STT endpoint: http://${host}:${port}/v1/audio/transcriptions`,
    );
  if (enabled.image)
    console.log(
      `OpenAI-compatible Image endpoint: http://${host}:${port}/v1/images/generations`,
    );
  if (authRequired) {
    console.log(`Authentication: enabled (mode=${authMode}).`);
    console.log(
      "Supported credentials: Authorization: Bearer <key>, x-api-key: <key> (mode-dependent).",
    );
  } else {
    console.log("Authentication: disabled via --auth false.");
  }
  console.log(
    `Enabled modalities: ${
      Object.entries(enabled)
        .filter(([, on]) => on)
        .map(([k]) => k)
        .join(", ") || "none"
    }`,
  );
  if (enabled.llm)
    console.log(`Upstream llama-server: http://127.0.0.1:${llmPort}`);
  if (enabled.stt)
    console.log(`Upstream whisper-server: http://127.0.0.1:${sttPort}`);
  if (enabled.image)
    console.log(`Upstream sd-server: http://127.0.0.1:${imagePort}`);

  if (enabled.llm) {
    console.log("\nExample chat request (Bearer):");
    console.log(
      `curl http://${host}:${port}/v1/chat/completions -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"model":"<your-model>","messages":[{"role":"user","content":"hello"}]}'`,
    );
  }
  if (enabled.stt) {
    console.log("\nExample STT request (x-api-key):");
    console.log(
      `curl -X POST http://${host}:${port}/v1/audio/transcriptions -H 'x-api-key: <API_KEY>' -F file=@audio.wav -F model=whisper`,
    );
  }
  if (enabled.image) {
    console.log("\nExample Image request (Bearer):");
    console.log(
      `curl http://${host}:${port}/v1/images/generations -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"prompt":"A scenic sunset","n":1,"size":"512x512"}'`,
    );
  }
}

function extractBearerToken(request: Request): string | null {
  const auth =
    request.headers.get("authorization") ??
    request.headers.get("Authorization");
  if (!auth) return null;
  const [scheme, token] = auth.split(/\s+/, 2);
  if (!scheme || !token) return null;
  if (scheme.toLowerCase() !== "bearer") return null;
  return token.trim();
}

function extractApiKeyHeader(request: Request): string | null {
  return request.headers.get("x-api-key") ?? request.headers.get("X-API-Key");
}

function extractAuthToken(request: Request, mode: AuthMode): string | null {
  if (mode === "bearer") return extractBearerToken(request);
  if (mode === "x-api-key") return extractApiKeyHeader(request)?.trim() ?? null;
  return (
    extractBearerToken(request) ?? extractApiKeyHeader(request)?.trim() ?? null
  );
}

type OpenAIErrorType =
  | "invalid_request_error"
  | "api_error"
  | "invalid_authentication_error"
  | "server_error";

type OpenAIErrorCode =
  | "invalid_api_key"
  | "route_disabled"
  | "validation_failed"
  | "service_unavailable"
  | "payload_too_large"
  | "upstream_error";

interface OpenAIError {
  message: string;
  type: OpenAIErrorType;
  param: string | null;
  code: OpenAIErrorCode | null;
  expected?: string;
  hint?: string;
}

interface OpenAIErrorResponse {
  error: OpenAIError;
}

function unauthorized(mode: AuthMode): Response {
  const hint = mode === "x-api-key" ? "x-api-key" : "Bearer";
  const body: OpenAIErrorResponse = {
    error: {
      message: "Unauthorized: Invalid or missing API key.",
      type: "invalid_request_error",
      param: null,
      code: "invalid_api_key",
      expected: mode === "either" ? "Bearer or x-api-key" : hint,
    },
  };
  return Response.json(body, {
    status: 401,
    headers: { "www-authenticate": "Bearer" },
  });
}

function notConfigured(feature: string): Response {
  const body: OpenAIErrorResponse = {
    error: {
      message: `${feature} route is disabled.`,
      type: "invalid_request_error",
      param: null,
      code: "route_disabled",
      hint: `Set --${feature.toLowerCase()} true and configure upstream/model to enable this route`,
    },
  };
  return Response.json(body, { status: 501 });
}

function badRequest(message: string): Response {
  const body: OpenAIErrorResponse = {
    error: {
      message,
      type: "invalid_request_error",
      param: null,
      code: "validation_failed",
    },
  };
  return Response.json(body, { status: 400 });
}

function payloadTooLarge(): Response {
  return Response.json(
    {
      error: {
        message: `Request body exceeds the ${MAX_REQUEST_BYTES / (1024 * 1024)} MiB limit.`,
        type: "invalid_request_error",
        param: null,
        code: "payload_too_large",
      },
    } satisfies OpenAIErrorResponse,
    { status: 413 },
  );
}

function upstreamFailure(message: string): Response {
  return Response.json(
    {
      error: {
        message,
        type: "server_error",
        param: null,
        code: "upstream_error",
      },
    } satisfies OpenAIErrorResponse,
    { status: 502 },
  );
}

function validationFailure(error: z.ZodError): Response {
  const issues = error.issues
    .map((issue) => `${issue.path.join(".")}: ${issue.message}`)
    .join(", ");
  return badRequest(`Validation failed: ${issues}`);
}

const chatMessageSchema = z
  .object({
    role: z.enum([
      "system",
      "user",
      "assistant",
      "function",
      "tool",
      "developer",
    ]),
    content: z
      .union([
        z.string(),
        z.array(
          z
            .object({
              type: z.string(),
              text: z.string().optional(),
              image_url: z.object({ url: z.string() }).optional(),
            })
            .passthrough(),
        ),
      ])
      .optional(),
    name: z.string().optional(),
    tool_calls: z.array(z.unknown()).optional(),
    tool_call_id: z.string().optional(),
  })
  .passthrough();

const chatCompletionRequestSchema = z
  .object({
    model: z.string(),
    messages: z.array(chatMessageSchema),
    temperature: z.number().min(0).max(2).optional(),
    top_p: z.number().min(0).max(1).optional(),
    n: z.number().min(1).optional(),
    stream: z.boolean().optional(),
    stop: z
      .union([z.string(), z.array(z.string())])
      .nullable()
      .optional(),
    max_tokens: z.number().positive().optional(),
    presence_penalty: z.number().min(-2).max(2).optional(),
    frequency_penalty: z.number().min(-2).max(2).optional(),
    logit_bias: z.record(z.string(), z.number()).nullable().optional(),
    user: z.string().optional(),
    response_format: z
      .object({ type: z.enum(["text", "json_object"]) })
      .optional(),
    tools: z.array(z.unknown()).optional(),
    tool_choice: z
      .union([
        z.string(),
        z.object({
          type: z.string(),
          function: z.object({ name: z.string() }),
        }),
      ])
      .optional(),
  })
  .passthrough();

const textCompletionRequestSchema = z
  .object({
    model: z.string(),
    prompt: z.union([z.string(), z.array(z.string())]),
    temperature: z.number().min(0).max(2).optional(),
    top_p: z.number().min(0).max(1).optional(),
    n: z.number().min(1).optional(),
    stream: z.boolean().optional(),
    stop: z
      .union([z.string(), z.array(z.string())])
      .nullable()
      .optional(),
    max_tokens: z.number().positive().optional(),
    presence_penalty: z.number().min(-2).max(2).optional(),
    frequency_penalty: z.number().min(-2).max(2).optional(),
    user: z.string().optional(),
  })
  .passthrough();

const imageGenerationRequestSchema = z
  .object({
    prompt: z.string(),
    model: z.string().optional(),
    n: z.number().min(1).max(10).optional(),
    quality: z.enum(["standard", "hd"]).optional(),
    response_format: z.enum(["url", "b64_json"]).optional(),
    size: z.enum(["256x256", "512x512", "1024x1024"]).optional(),
    style: z.enum(["vivid", "natural"]).optional(),
    user: z.string().optional(),
  })
  .passthrough();

const transcriptionRequestSchema = z.object({
  file: z.instanceof(Blob, {
    message: "file must be a valid File or Blob object",
  }),
  model: z.string().optional(),
  language: z.string().optional(),
  prompt: z.string().optional(),
  response_format: z
    .enum(["json", "text", "srt", "verbose_json", "vtt"])
    .optional(),
  temperature: z.number().min(0).max(1).optional(),
});

const embeddingsRequestSchema = z
  .object({
    model: z.string(),
    input: z.union([
      z.string(),
      z.array(z.string()),
      z.array(z.number()),
      z.array(z.array(z.number())),
    ]),
    user: z.string().optional(),
  })
  .passthrough();

const chatCompletionResponseSchema = z
  .object({
    id: z.string(),
    object: z.string(),
    created: z.number(),
    model: z.string(),
    choices: z.array(
      z.object({
        index: z.number(),
        message: z.object({
          role: z.string(),
          content: z.string().nullable().optional(),
        }),
        finish_reason: z.string().nullable().optional(),
      }),
    ),
    usage: z
      .object({
        prompt_tokens: z.number(),
        completion_tokens: z.number(),
        total_tokens: z.number(),
      })
      .optional(),
  })
  .passthrough();

const textCompletionResponseSchema = z
  .object({
    id: z.string(),
    object: z.string(),
    created: z.number(),
    model: z.string(),
    choices: z.array(
      z.object({
        text: z.string(),
        index: z.number(),
        finish_reason: z.string().nullable().optional(),
      }),
    ),
    usage: z
      .object({
        prompt_tokens: z.number(),
        completion_tokens: z.number(),
        total_tokens: z.number(),
      })
      .optional(),
  })
  .passthrough();

const embeddingsResponseSchema = z
  .object({
    object: z.string(),
    data: z.array(
      z.object({
        object: z.string(),
        index: z.number(),
        embedding: z.array(z.number()),
      }),
    ),
    model: z.string(),
    usage: z.object({
      prompt_tokens: z.number(),
      total_tokens: z.number(),
    }),
  })
  .passthrough();

const imageGenerationResponseSchema = z
  .object({
    created: z.number(),
    data: z.array(
      z.object({
        url: z.string().optional(),
        b64_json: z.string().optional(),
        revised_prompt: z.string().optional(),
      }),
    ),
  })
  .passthrough();

const transcriptionResponseSchema = z.union([
  z.object({
    text: z.string(),
  }),
  z
    .object({
      task: z.string().optional(),
      language: z.string().optional(),
      duration: z.number().optional(),
      text: z.string(),
      segments: z.array(z.unknown()).optional(),
    })
    .passthrough(),
]);

class PayloadTooLargeError extends Error {}

function requestExceedsSizeLimit(request: Request): boolean {
  const contentLength = request.headers.get("content-length");
  if (!contentLength) return false;
  const size = Number(contentLength);
  return !Number.isSafeInteger(size) || size < 0 || size > MAX_REQUEST_BYTES;
}

async function readBoundedRequestBody(request: Request): Promise<Uint8Array> {
  if (requestExceedsSizeLimit(request)) throw new PayloadTooLargeError();
  const body = request.clone().body;
  if (!body) return new Uint8Array();

  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_REQUEST_BYTES) throw new PayloadTooLargeError();
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const result = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return result;
}

async function parseJsonRequest<T extends z.ZodType>(
  request: Request,
  schema: T,
): Promise<
  { success: true; data: z.output<T> } | { success: false; response: Response }
> {
  try {
    const body = await readBoundedRequestBody(request);
    const parsed = schema.safeParse(JSON.parse(new TextDecoder().decode(body)));
    return parsed.success
      ? { success: true, data: parsed.data }
      : { success: false, response: validationFailure(parsed.error) };
  } catch (error) {
    return {
      success: false,
      response:
        error instanceof PayloadTooLargeError
          ? payloadTooLarge()
          : badRequest("Invalid JSON payload."),
    };
  }
}

function requestWithJsonBody(request: Request, body: unknown): Request {
  const headers = new Headers(request.headers);
  headers.set("content-type", "application/json");
  headers.delete("content-length");
  return new Request(request.url, {
    method: request.method,
    headers,
    body: JSON.stringify(body),
  });
}

function filterProxyHeaders(headers: Headers): Headers {
  const filtered = new Headers(headers);
  const connection = filtered.get("connection");
  const connectionHeaders = connection
    ? connection
        .split(",")
        .map((header) => header.trim())
        .filter(Boolean)
    : [];
  for (const header of [
    "authorization",
    "x-api-key",
    "proxy-authorization",
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
    "host",
    ...connectionHeaders,
  ]) {
    filtered.delete(header);
  }
  return filtered;
}

function isEventStream(response: Response): boolean {
  return (
    response.headers.get("content-type")?.includes("text/event-stream") ?? false
  );
}

async function proxyRequest(
  request: Request,
  targetBase: string,
  pathOverride?: string,
  responseSchema?: z.ZodType,
): Promise<Response> {
  const incoming = new URL(request.url);
  const path = pathOverride ?? incoming.pathname;
  const target = `${targetBase}${path}${incoming.search}`;
  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method: request.method,
      headers: filterProxyHeaders(request.headers),
      body: request.body,
      signal: AbortSignal.any([
        request.signal,
        AbortSignal.timeout(UPSTREAM_PROXY_TIMEOUT_MS),
      ]),
    });
  } catch {
    return upstreamFailure("The upstream service could not be reached.");
  }

  if (responseSchema && !isEventStream(upstream) && upstream.ok) {
    try {
      const parsed = responseSchema.safeParse(await upstream.json());
      if (!parsed.success) {
        return upstreamFailure(
          "The upstream service returned an invalid response.",
        );
      }

      const headers = filterProxyHeaders(upstream.headers);
      headers.delete("content-length");
      return Response.json(parsed.data, {
        status: upstream.status,
        headers,
      });
    } catch {
      return upstreamFailure("The upstream service returned malformed JSON.");
    }
  }

  return new Response(upstream.body, {
    status: upstream.status,
    headers: filterProxyHeaders(upstream.headers),
  });
}

/**
 * Supervisor that manages a model backend subprocess (e.g. llama-server).
 * Handles lazy loading, auto-restart crash recovery with exponential backoff,
 * sliding-window crash limits, stdout/stderr log piping, and startup readiness checks.
 */
class ManagedService {
  private proc: Bun.Subprocess | null = null;
  private name: string;
  private startFn: () => Promise<Bun.Subprocess>;
  private healthUrl: string;
  private logger: ILogger;
  private crashTimes: number[] = [];
  private isRestarting = false;
  private restartPromise: Promise<void> | null = null;
  private isShuttingDown = false;
  private shutdownPromise: Promise<void> | null = null;
  private guardians = new Map<number, Bun.Subprocess>();
  private onFatal: () => Promise<void>;
  private timeoutMs: number;

  constructor(
    name: string,
    healthUrl: string,
    logger: ILogger,
    startFn: () => Promise<Bun.Subprocess>,
    timeoutMs = 30000,
    onFatal: () => Promise<void> = async () => {},
  ) {
    this.name = name;
    this.healthUrl = healthUrl;
    this.logger = logger;
    this.startFn = startFn;
    this.timeoutMs = timeoutMs;
    this.onFatal = onFatal;
  }

  /**
   * Lazily starts the service on first use, or awaits recovery if currently restarting.
   */
  async ensureRunning(): Promise<void> {
    if (this.isShuttingDown) {
      throw new Error(`${this.name} is shutting down`);
    }
    if (this.proc && this.proc.exitCode === null) {
      return;
    }
    if (this.isRestarting) {
      await this.restartPromise;
      return;
    }
    await this.start();
  }

  /**
   * Spawns the subprocess, registers crash handlers, pipes logs, and awaits healthy status.
   * Employs exponential backoff on retry and exits the manager if crash limits are hit.
   */
  private async start(): Promise<void> {
    this.isRestarting = true;
    this.restartPromise = (async () => {
      const now = Date.now();
      this.crashTimes = this.crashTimes.filter((t) => now - t < 300000); // 5 min window
      const crashCount = this.crashTimes.length;

      if (crashCount >= 5) {
        this.logger.error(
          this.name,
          `Service has crashed ${crashCount} times in 5 minutes. Stopping manager.`,
        );
        void this.onFatal().catch((err) => {
          this.logger.error(this.name, "Failed to stop manager", err as Error);
        });
        return;
      }

      if (crashCount > 0) {
        const backoffMs = Math.min(1000 * Math.pow(2, crashCount - 1), 16000);
        this.logger.warn(
          this.name,
          `Crashed previously. Backing off for ${backoffMs}ms before restarting...`,
        );
        await new Promise((resolve) => setTimeout(resolve, backoffMs));
      }

      this.logger.info(this.name, "Starting subprocess...");
      try {
        const proc = await this.startFn();
        if (this.isShuttingDown) {
          await this.stopProcess(proc);
          return;
        }

        this.proc = proc;
        this.startGuardian(proc);
        if (this.proc.stdout && typeof this.proc.stdout !== "number")
          this.logger.pipeStream(this.proc.stdout, this.name);
        if (this.proc.stderr && typeof this.proc.stderr !== "number")
          this.logger.pipeStream(this.proc.stderr, this.name);

        proc.exited.then(() => {
          void this.stopGuardian(proc);
          this.handleCrash(proc);
        });

        const ok = await this.waitHealthy();
        if (!ok) {
          throw new Error("Health check failed to pass within timeout");
        }
        this.logger.info(this.name, "Subprocess is healthy and ready.");
      } catch (err) {
        this.logger.error(this.name, "Failed to start service", err as Error);
        this.crashTimes.push(Date.now());
        await this.kill();
        this.isRestarting = false;
        throw err;
      }
      this.isRestarting = false;
    })();
    await this.restartPromise;
  }

  /**
   * Polls the backend's /health endpoint until it is online or startup times out.
   */
  private async waitHealthy(): Promise<boolean> {
    const timeout = this.timeoutMs;
    const interval = 200; // 200ms
    const start = Date.now();

    while (Date.now() - start < timeout) {
      if (this.isShuttingDown) return false;
      if (this.proc?.exitCode !== null) {
        this.logger.error(
          this.name,
          `Subprocess exited during startup with code ${this.proc?.exitCode}`,
        );
        return false;
      }
      try {
        const res = await fetch(this.healthUrl, {
          signal: AbortSignal.timeout(HEALTH_PROBE_TIMEOUT_MS),
        });
        if (res.ok) return true;
      } catch (e) {
        // Expected network failures while booting
      }
      await new Promise((resolve) => setTimeout(resolve, interval));
    }
    return false;
  }

  /**
   * Stops a backend for a model switch without triggering crash recovery.
   */
  async kill(): Promise<void> {
    await this.stopCurrentProcess();
  }

  /** Prevents future restarts and waits for an active startup to finish stopping. */
  async shutdown(): Promise<void> {
    if (!this.shutdownPromise) {
      this.isShuttingDown = true;
      const startup = this.restartPromise;
      this.shutdownPromise = (async () => {
        await this.stopCurrentProcess();
        if (startup) await startup.catch(() => {});
        await this.stopCurrentProcess();
        await this.stopAllGuardians();
      })();
    }
    await this.shutdownPromise;
  }

  private async stopCurrentProcess(): Promise<void> {
    const p = this.proc;
    if (p) {
      this.proc = null;
      await this.stopProcess(p);
      await this.stopGuardian(p);
    }
  }

  private startGuardian(proc: Bun.Subprocess): void {
    const guardian = Bun.spawn(guardianProcessCommand(process.pid, proc.pid), {
      stdin: "ignore",
      stdout: "ignore",
      stderr: "ignore",
      detached: true,
    });
    this.guardians.set(proc.pid, guardian);
    guardian.exited.then(() => {
      if (this.guardians.get(proc.pid) === guardian) {
        this.guardians.delete(proc.pid);
      }
    });
  }

  private async stopGuardian(proc: Bun.Subprocess): Promise<void> {
    const guardian = this.guardians.get(proc.pid);
    if (!guardian) return;
    this.guardians.delete(proc.pid);
    await this.stopProcess(guardian);
  }

  private async stopAllGuardians(): Promise<void> {
    const guardians = [...this.guardians.values()];
    this.guardians.clear();
    await Promise.all(guardians.map((guardian) => this.stopProcess(guardian)));
  }

  /** Gracefully terminates a subprocess, then forcefully reaps it if needed. */
  private async stopProcess(p: Bun.Subprocess): Promise<void> {
    if (p.exitCode !== null) return;

    try {
      p.kill(15);
    } catch {
      return;
    }

    const exitedDuringGrace = await Promise.race([
      p.exited.then(
        () => true,
        () => true,
      ),
      Bun.sleep(CHILD_STOP_GRACE_MS).then(() => false),
    ]);
    if (exitedDuringGrace || p.exitCode !== null) return;

    try {
      p.kill(9);
    } catch {
      return;
    }
    await p.exited.catch(() => {});
  }

  /**
   * Initiates asynchronous process recovery when a running subprocess exits.
   */
  handleCrash(proc: Bun.Subprocess): void {
    if (
      !this.isShuttingDown &&
      this.proc === proc &&
      proc.exitCode !== null &&
      !this.isRestarting
    ) {
      this.logger.warn(
        this.name,
        `Subprocess exited unexpectedly with code ${proc.exitCode}. Triggering self-healing...`,
      );
      this.crashTimes.push(Date.now());
      this.proc = null;
      this.start().catch(() => {});
    }
  }
}

/**
 * Returns a standard HTTP 503 service unavailable response.
 */
function serviceUnavailable(serviceName: string): Response {
  const body: OpenAIErrorResponse = {
    error: {
      message: `${serviceName} service is currently restarting or unavailable. Please try again shortly.`,
      type: "api_error",
      param: null,
      code: "service_unavailable",
    },
  };
  return new Response(JSON.stringify(body), {
    status: 503,
    headers: {
      "Content-Type": "application/json",
      "Retry-After": "5",
    },
  });
}

/**
 * Main command handler for 'serve'. Starts the unified proxy server.
 * Handles dynamic context sizing: min(recommendedForHardwareAndModel, maxContextCeiling).
 * Automatically maps OpenAI 'developer' role to 'system' role for tokenizer compatibility.
 * Maps client-side '/v1/slots|metrics|props|system_info' queries to standard llama-server root endpoints.
 * Automatically synchronizes active model specifications and context limits to OpenCode in real-time.
 */
export async function runServe(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const config = ctx.config;
  const wrapperHost = parseFlag(args, "--host") ?? "0.0.0.0";
  const wrapperPort = toInt(parseFlag(args, "--port"), 2273);

  const llmHost = parseFlag(args, "--llm-host") ?? "127.0.0.1";
  const llmPort = toInt(parseFlag(args, "--llm-port"), config.port);
  const sttHost = parseFlag(args, "--stt-host") ?? "127.0.0.1";
  const sttPort = toInt(parseFlag(args, "--stt-port"), config.sttPort);
  const imageHost = parseFlag(args, "--image-host") ?? "127.0.0.1";
  const imagePort = toInt(parseFlag(args, "--image-port"), 8090);

  let ctxSize = toInt(parseFlag(args, "--ctx-size"), 0);
  if (!ctxSize) {
    const spec = byId(config.activeLlmModel);
    const recommendedCtx = spec
      ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
      : ctx.specs.gpuVramGb >= 32
        ? 32768
        : 8192;
    ctxSize = Math.min(recommendedCtx, config.ctxSize);
    console.log(`\n💡 Dynamic Context Size: Initialized to ${ctxSize} tokens.`);
    console.log(
      `   (Calculated best for "${config.activeLlmModel}" on ${ctx.specs.gpuVramGb} GB hardware: ${recommendedCtx} tokens, limited by max context setting: ${config.ctxSize} tokens)`,
    );
  } else {
    console.log(
      `\n💡 Context Size: Using explicit override --ctx-size ${ctxSize} tokens.`,
    );
  }

  // Automatically synchronize active model and calculated context size with Continue configuration
  await syncContinueConfig(config, ctxSize);
  const sttPath = parseFlag(args, "--stt-path") ?? "/inference";
  const authRequired = parseFlag(args, "--auth") !== "false";
  const authMode = parseAuthMode(parseFlag(args, "--auth-mode"));

  const llmModelFileOverride = parseFlag(args, "--llm-model-file");
  let llmModelFile = llmModelFileOverride;
  let llmModelExists: boolean;
  if (!llmModelFile) {
    const spec = byId(config.activeLlmModel);
    if (spec) {
      llmModelFile = primaryArtifact(spec).filename;
      llmModelExists = (
        await resolveCatalogInstallation(spec, config.llmModelsDir)
      ).complete;
    } else if (
      await Bun.file(
        join(config.llmModelsDir, `${config.activeLlmModel}.bin`),
      ).exists()
    ) {
      llmModelFile = `${config.activeLlmModel}.bin`;
      llmModelExists = true;
    } else {
      llmModelFile = `${config.activeLlmModel}.gguf`;
      llmModelExists = await Bun.file(
        join(config.llmModelsDir, llmModelFile),
      ).exists();
    }
  } else {
    llmModelExists = await Bun.file(
      join(config.llmModelsDir, llmModelFile),
    ).exists();
  }

  let sttModelFile = parseFlag(args, "--stt-model-file");
  if (!sttModelFile) {
    const spec = byId(config.activeSttModel);
    const primaryFilename = spec && primaryArtifact(spec).filename;
    if (
      primaryFilename &&
      (await Bun.file(join(config.sttModelsDir, primaryFilename)).exists())
    ) {
      sttModelFile = primaryFilename;
    } else if (
      await Bun.file(
        join(config.sttModelsDir, `${config.activeSttModel}.bin`),
      ).exists()
    ) {
      sttModelFile = `${config.activeSttModel}.bin`;
    } else {
      sttModelFile = `${config.activeSttModel}.gguf`;
    }
  }

  let imageModelFile = parseFlag(args, "--image-model-file");
  if (!imageModelFile) {
    const spec = byId(config.activeImageModel);
    const primaryFilename = spec && primaryArtifact(spec).filename;
    if (
      primaryFilename &&
      (await Bun.file(join(config.imageModelsDir, primaryFilename)).exists())
    ) {
      imageModelFile = primaryFilename;
    } else {
      imageModelFile = `${config.activeImageModel}.safetensors`;
    }
  }

  let sttModelExists = await Bun.file(
    join(config.sttModelsDir, sttModelFile),
  ).exists();
  let imageModelExists = await Bun.file(
    join(config.imageModelsDir, imageModelFile),
  ).exists();

  const enabled: ModalityState = {
    llm: parseBool(parseFlag(args, "--llm"), true),
    stt: parseBool(
      parseFlag(args, "--stt"),
      config.selectedSttModels.length > 0,
    ),
    image: parseBool(
      parseFlag(args, "--image"),
      config.selectedImageModels.length > 0,
    ),
  };

  if (enabled.stt && !config.activeSttModel) {
    throw new Error(
      "STT modality is enabled but no active STT model is configured. Run `local-base configure` first.",
    );
  }
  if (enabled.image && !config.activeImageModel) {
    throw new Error(
      "Image modality is enabled but no active Image model is configured. Run `local-base configure` first.",
    );
  }

  // Perform memory fit evaluation BEFORE downloading
  const specs = ctx.specs;
  const getModelIdFromFile = (filename: string): string => {
    return filename.replace(/\.(gguf|bin|onnx|safetensors|pth)$/i, "");
  };

  const bypassCheck =
    args.includes("--bypass-memory-check") || args.includes("--force");

  if (enabled.llm) {
    const llmModelId = getModelIdFromFile(llmModelFile);
    const llmSpec = byId(llmModelId);
    if (llmSpec) {
      const fit = evaluateModelFit(llmSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run LLM "${llmSpec.modelId}".`,
        );
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.error(
          `   Running this model will likely crash the system or cause severe slowdowns.`,
        );
        if (!bypassCheck) {
          console.error(
            `   To force launch this model anyway, use --bypass-memory-check`,
          );
          return 1;
        } else {
          console.warn(
            `   Bypassing memory validation check and proceeding...`,
          );
        }
      } else if (fit.status === "tight") {
        console.warn(
          `\n⚠️ WARNING: Tight memory fit for LLM "${llmSpec.modelId}".`,
        );
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.warn(
          `   Leaves only ${fit.headroomGb.toFixed(1)} GB headroom. Large context windows may cause slowdowns.`,
        );
      } else {
        console.log(
          `\n✅ Memory check passed: LLM "${llmSpec.modelId}" fits comfortably in ${specs.gpuVramGb} GB.`,
        );
      }

      const maxSafeCtx = calculateMaxSafeContextSize(llmSpec, specs.gpuVramGb);
      if (ctxSize < maxSafeCtx) {
        console.log(
          `\n💡 Tip: Your system supports a larger context size of up to ${maxSafeCtx} tokens for "${llmSpec.modelId}".`,
        );
        console.log(
          `   You can configure this by running 'local-base configure' or starting with '--ctx-size ${maxSafeCtx}'.`,
        );
      }
    }
  }

  if (enabled.stt) {
    const sttModelId = getModelIdFromFile(sttModelFile);
    const sttSpec = byId(sttModelId);
    if (sttSpec) {
      const fit = evaluateModelFit(sttSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run STT "${sttSpec.modelId}".`,
        );
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        if (!bypassCheck) {
          console.error(
            `   To force launch this model anyway, use --bypass-memory-check`,
          );
          return 1;
        } else {
          console.warn(
            `   Bypassing memory validation check and proceeding...`,
          );
        }
      } else if (fit.status === "tight") {
        console.warn(
          `\n⚠️ WARNING: Tight memory fit for STT "${sttSpec.modelId}".`,
        );
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  if (enabled.image) {
    const imageModelId = getModelIdFromFile(imageModelFile);
    const imageSpec = byId(imageModelId);
    if (imageSpec) {
      const fit = evaluateModelFit(imageSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run Image model "${imageSpec.modelId}".`,
        );
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        if (!bypassCheck) {
          console.error(
            `   To force launch this model anyway, use --bypass-memory-check`,
          );
          return 1;
        } else {
          console.warn(
            `   Bypassing memory validation check and proceeding...`,
          );
        }
      } else if (fit.status === "tight") {
        console.warn(
          `\n⚠️ WARNING: Tight memory fit for Image model "${imageSpec.modelId}".`,
        );
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  // Automatically download models if they pass memory checks and are missing.
  if (enabled.llm && !llmModelExists) {
    console.log(
      `LLM model is incomplete. Automatically installing "${config.activeLlmModel}"...`,
    );
    const installedPath = await installModel(config, config.activeLlmModel);
    llmModelFile = basename(installedPath);
    llmModelExists = true;
  }

  if (enabled.stt && !sttModelExists) {
    console.log(
      `STT model file is missing. Automatically installing "${config.activeSttModel}"...`,
    );
    const installedPath = await installModel(config, config.activeSttModel);
    sttModelFile = basename(installedPath);
    sttModelExists = true;
  }

  if (enabled.image && !imageModelExists) {
    console.log(
      `Image model file is missing. Automatically installing "${config.activeImageModel}"...`,
    );
    const installedPath = await installModel(config, config.activeImageModel);
    imageModelFile = basename(installedPath);
    imageModelExists = true;
  }

  if (!enabled.llm && !enabled.stt && !enabled.image) {
    throw new Error(
      "No modalities enabled. Enable with --llm/--stt/--image true.",
    );
  }

  if (!enabled.llm && !parseFlag(args, "--llm")) {
    console.log("LLM route auto-disabled (no local LLM model file found).");
  }
  if (!enabled.stt && !parseFlag(args, "--stt")) {
    console.log("STT route auto-disabled (no local STT model file found).");
  }
  if (!enabled.image && !parseFlag(args, "--image")) {
    console.log("Image route auto-disabled (no local Image model file found).");
  }

  const llmBase = `http://${llmHost}:${llmPort}`;
  const sttBase = `http://${sttHost}:${sttPort}`;
  const imageBase = `http://${imageHost}:${imagePort}`;

  const activeLlmSpec = byId(config.activeLlmModel);
  const llmTimeoutMs =
    activeLlmSpec && activeLlmSpec.minVramGb >= 16 ? 180000 : 60000;

  let exitAfterShutdown: ((status: number) => Promise<never>) | undefined;
  const fatalServiceExit = async (): Promise<void> => {
    if (!exitAfterShutdown) {
      throw new Error("Serve shutdown is not initialized");
    }
    await exitAfterShutdown(1);
  };

  const llmService = enabled.llm
    ? new ManagedService(
        "llama-server",
        llmBase + "/health",
        ctx.logger,
        async () => {
          const launchConfig = loadConfig(ctx.database, config.root);
          const activeModel = launchConfig.activeLlmModel;
          let modelFile = llmModelFileOverride;
          if (!modelFile) {
            const spec = byId(activeModel);
            if (spec) {
              const installation = await resolveCatalogInstallation(
                spec,
                launchConfig.llmModelsDir,
              );
              if (!installation.complete) {
                ctx.logger.info(
                  "llama-server",
                  `Model is incomplete for "${activeModel}". Automatically installing...`,
                );
                const installedPath = await installModel(
                  launchConfig,
                  activeModel,
                );
                modelFile = basename(installedPath);
              } else {
                modelFile = primaryArtifact(spec).filename;
              }
            } else {
              const expectedFile = (await Bun.file(
                join(launchConfig.llmModelsDir, `${activeModel}.bin`),
              ).exists())
                ? `${activeModel}.bin`
                : `${activeModel}.gguf`;
              const modelPath = join(launchConfig.llmModelsDir, expectedFile);
              if (!(await Bun.file(modelPath).exists())) {
                ctx.logger.info(
                  "llama-server",
                  `Model file is missing for "${activeModel}". Automatically installing...`,
                );
                const installedPath = await installModel(
                  launchConfig,
                  activeModel,
                );
                modelFile = basename(installedPath);
              } else {
                modelFile = expectedFile;
              }
            }
          }

          let finalCtxSize = toInt(parseFlag(args, "--ctx-size"), 0);
          if (!finalCtxSize) {
            const spec = byId(activeModel);
            const recommendedCtx = spec
              ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
              : ctx.specs.gpuVramGb >= 32
                ? 32768
                : 8192;
            finalCtxSize = Math.min(recommendedCtx, launchConfig.ctxSize);
          }

          ctx.logger.info(
            "llama-server",
            `Spawning model "${activeModel}" (file: ${modelFile}, context: ${finalCtxSize} tokens)`,
          );
          return startLlamaServerProcess(
            launchConfig,
            modelFile,
            llmHost,
            llmPort,
            finalCtxSize,
            { memoryGb: ctx.specs.gpuVramGb },
          );
        },
        llmTimeoutMs,
        fatalServiceExit,
      )
    : null;

  const sttService = enabled.stt
    ? new ManagedService(
        "whisper-server",
        sttBase + "/health",
        ctx.logger,
        () => startWhisperServerProcess(config, sttModelFile, sttHost, sttPort),
        30000,
        fatalServiceExit,
      )
    : null;

  const imageService = enabled.image
    ? new ManagedService(
        "sd-server",
        imageBase + "/",
        ctx.logger,
        async () => {
          const launchConfig = loadConfig(ctx.database, config.root);
          const activeModel = launchConfig.activeImageModel;
          let modelFile = parseFlag(args, "--image-model-file");
          if (!modelFile) {
            const spec = byId(activeModel);
            let expectedFile = spec
              ? primaryArtifact(spec).filename
              : undefined;
            if (!expectedFile) {
              expectedFile = `${activeModel}.safetensors`;
            }
            const modelPath = join(launchConfig.imageModelsDir, expectedFile);
            if (!(await Bun.file(modelPath).exists())) {
              throw new Error(`Image model file missing at ${modelPath}`);
            }
            modelFile = expectedFile;
          }

          ctx.logger.info(
            "sd-server",
            `Spawning image model "${activeModel}" (file: ${modelFile}) on port ${imagePort}`,
          );
          return startSdServerProcess(
            launchConfig,
            modelFile,
            imageHost,
            imagePort,
          );
        },
        30000,
        fatalServiceExit,
      )
    : null;

  let modelSwitches = Promise.resolve();
  const serializeModelSwitch = async (
    work: () => Promise<void>,
  ): Promise<void> => {
    const next = modelSwitches.then(work, work);
    modelSwitches = next.catch(() => {});
    await next;
  };

  const switchLlmModel = async (requestedModel: string): Promise<void> => {
    if (!llmService) return;
    await serializeModelSwitch(async () => {
      const latestConfig = loadConfig(ctx.database, config.root);
      const normalized = requestedModel.replace(
        /^(localbase|openai|ollama)\//,
        "",
      );
      const matchedModel = latestConfig.selectedLlmModels.find(
        (model) => model.toLowerCase() === normalized.toLowerCase(),
      );
      if (!matchedModel || matchedModel === latestConfig.activeLlmModel) return;

      ctx.logger.info(
        "llama-server",
        `Switching active LLM from "${latestConfig.activeLlmModel}" to "${matchedModel}"`,
      );
      await llmService.kill();
      latestConfig.activeLlmModel = matchedModel;
      saveConfig(ctx.database, latestConfig);

      const spec = byId(matchedModel);
      const recommendedCtx = spec
        ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
        : ctx.specs.gpuVramGb >= 32
          ? 32768
          : 8192;
      const newCtxSize = Math.min(recommendedCtx, latestConfig.ctxSize);
      void syncContinueConfig(latestConfig, newCtxSize).catch((error) => {
        ctx.logger.warn(
          "sync",
          `Failed to sync Continue config: ${error.message}`,
        );
      });
    });
  };

  const switchImageModel = async (
    requestedModel: string | undefined,
  ): Promise<void> => {
    if (!imageService || !requestedModel) return;
    await serializeModelSwitch(async () => {
      const latestConfig = loadConfig(ctx.database, config.root);
      const matchedModel = latestConfig.selectedImageModels.find(
        (model) => model.toLowerCase() === requestedModel.toLowerCase(),
      );
      if (!matchedModel || matchedModel === latestConfig.activeImageModel)
        return;

      ctx.logger.info(
        "sd-server",
        `Switching active Image model from "${latestConfig.activeImageModel}" to "${matchedModel}"`,
      );
      await imageService.kill();
      latestConfig.activeImageModel = matchedModel;
      saveConfig(ctx.database, latestConfig);
    });
  };

  const ensureLlm = async (): Promise<Response | null> => {
    if (!enabled.llm || !llmService) return notConfigured("LLM");
    try {
      await llmService.ensureRunning();
      return null;
    } catch {
      return serviceUnavailable("LLM");
    }
  };

  const handleRequest = async (
    request: Request,
    pathname: string,
  ): Promise<Response> => {
    const currentConfig = loadConfig(ctx.database, config.root);

    if (pathname === "/health") {
      return Response.json({
        status: "ok",
        enabled,
        llmUpstream: enabled.llm ? llmBase : null,
        sttUpstream: enabled.stt ? sttBase : null,
        imageUpstream: enabled.image ? imageBase : null,
        authRequired,
        authMode,
      });
    }

    if (authRequired) {
      const token = extractAuthToken(request, authMode);
      const isMasterKey =
        process.env.LOCALBASE_API_KEY &&
        token === process.env.LOCALBASE_API_KEY;
      if (
        !token ||
        (!isMasterKey && !validateApiKey(ctx.database, currentConfig, token))
      ) {
        return unauthorized(authMode);
      }
    }

    if (requestExceedsSizeLimit(request)) return payloadTooLarge();

    if (
      pathname === "/v1/audio/transcriptions" ||
      pathname === "/v1/audio/translations"
    ) {
      if (!enabled.stt || !sttService) return notConfigured("STT");
      try {
        const body = await readBoundedRequestBody(request);
        const multipartBody = new ArrayBuffer(body.byteLength);
        new Uint8Array(multipartBody).set(body);
        const formData = await new Request(request.url, {
          method: request.method,
          headers: request.headers,
          body: multipartBody,
        }).formData();
        const bodyObj: Record<
          string,
          FormDataEntryValue | FormDataEntryValue[]
        > = {};
        for (const [key, value] of formData.entries()) {
          const existing = bodyObj[key];
          bodyObj[key] =
            existing === undefined
              ? value
              : Array.isArray(existing)
                ? [...existing, value]
                : [existing, value];
        }

        const parsed = transcriptionRequestSchema.safeParse(bodyObj);
        if (!parsed.success) {
          return validationFailure(parsed.error);
        }
        const normalizedForm = new FormData();
        for (const [key, value] of Object.entries(parsed.data)) {
          if (value !== undefined)
            normalizedForm.append(
              key,
              value instanceof Blob ? value : String(value),
            );
        }
        request = new Request(request.url, {
          method: request.method,
          body: normalizedForm,
        });
      } catch (e) {
        return e instanceof PayloadTooLargeError
          ? payloadTooLarge()
          : badRequest("Invalid form data payload.");
      }
      try {
        await sttService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("STT");
      }
      return proxyRequest(
        request,
        sttBase,
        sttPath,
        transcriptionResponseSchema,
      );
    }

    if (
      ["/llm", "/stt", "/image"].some(
        (namespace) =>
          pathname === namespace || pathname.startsWith(`${namespace}/`),
      )
    ) {
      return Response.json(
        {
          error: {
            message:
              "Raw backend passthrough routes are not exposed by this gateway.",
            type: "invalid_request_error",
            param: null,
            code: "route_disabled",
          },
        } satisfies OpenAIErrorResponse,
        { status: 404 },
      );
    }

    if (pathname.startsWith("/tts")) {
      return Response.json(
        { error: "TTS service is not yet implemented. Stay tuned!" },
        { status: 501 },
      );
    }

    if (pathname.startsWith("/video")) {
      return Response.json(
        {
          error: "Video generation service is not yet implemented. Stay tuned!",
        },
        { status: 501 },
      );
    }

    if (pathname === "/v1/images/generations") {
      if (!enabled.image || !imageService) return notConfigured("Image");
      const parsed = await parseJsonRequest(
        request,
        imageGenerationRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      await switchImageModel(parsed.data.model);
      try {
        await imageService.ensureRunning();
      } catch {
        return serviceUnavailable("Image");
      }
      return proxyRequest(
        requestWithJsonBody(request, parsed.data),
        imageBase,
        undefined,
        imageGenerationResponseSchema,
      );
    }

    if (pathname === "/v1/chat/completions") {
      const parsed = await parseJsonRequest(
        request,
        chatCompletionRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      await switchLlmModel(parsed.data.model);
      const unavailable = await ensureLlm();
      if (unavailable) return unavailable;

      const messages = parsed.data.messages.map((message) =>
        message.role === "developer"
          ? { ...message, role: "system" as const }
          : message,
      );
      const currentPrompt =
        loadConfig(ctx.database, config.root).systemPrompt ||
        DEFAULT_SYSTEM_PROMPT;
      if (
        !messages.some((message) => message.role === "system") &&
        currentPrompt
      ) {
        messages.unshift({ role: "system", content: currentPrompt });
      }
      return proxyRequest(
        requestWithJsonBody(request, { ...parsed.data, messages }),
        llmBase,
        undefined,
        chatCompletionResponseSchema,
      );
    }

    if (pathname === "/v1/completions") {
      const parsed = await parseJsonRequest(
        request,
        textCompletionRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      await switchLlmModel(parsed.data.model);
      const unavailable = await ensureLlm();
      if (unavailable) return unavailable;
      return proxyRequest(
        requestWithJsonBody(request, parsed.data),
        llmBase,
        undefined,
        textCompletionResponseSchema,
      );
    }

    if (pathname === "/v1/embeddings") {
      const parsed = await parseJsonRequest(request, embeddingsRequestSchema);
      if (!parsed.success) return parsed.response;
      await switchLlmModel(parsed.data.model);
      const unavailable = await ensureLlm();
      if (unavailable) return unavailable;
      return proxyRequest(
        requestWithJsonBody(request, parsed.data),
        llmBase,
        undefined,
        embeddingsResponseSchema,
      );
    }

    if (pathname === "/v1/models") {
      const modelsList = [
        ...new Set([
          currentConfig.activeLlmModel,
          ...currentConfig.selectedLlmModels,
        ]),
      ];
      const data = modelsList.map((modelId) => ({
        id: modelId,
        object: "model",
        created: 1670000000,
        owned_by: "local-base",
      }));
      return Response.json({
        object: "list",
        data,
      });
    }

    const llmIntrospectionRoutes: Record<string, string> = {
      "/v1/slots": "/slots",
      "/v1/metrics": "/metrics",
      "/v1/props": "/props",
      "/v1/system_info": "/system_info",
    };
    const upstreamPath = llmIntrospectionRoutes[pathname];
    if (upstreamPath) {
      const unavailable = await ensureLlm();
      if (unavailable) return unavailable;
      return proxyRequest(request, llmBase, upstreamPath);
    }

    const unavailable = await ensureLlm();
    if (unavailable) return unavailable;
    return proxyRequest(request, llmBase);
  };

  const server = Bun.serve({
    hostname: wrapperHost,
    port: wrapperPort,
    fetch: async (request) => {
      const start = performance.now();
      const ip = server.requestIP(request)?.address ?? "127.0.0.1";
      const { pathname } = new URL(request.url);
      const method = request.method;

      if (method === "OPTIONS") {
        return new Response(null, {
          status: 204,
          headers: {
            "Access-Control-Allow-Origin": "*",
            "Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, OPTIONS",
            "Access-Control-Allow-Headers":
              "Content-Type, Authorization, x-api-key",
            "Access-Control-Max-Age": "86400",
          },
        });
      }

      let response: Response;
      try {
        response = await handleRequest(request, pathname);
      } catch (err) {
        ctx.logger.error(
          "HTTP",
          `Error handling request ${method} ${pathname}`,
          err as Error,
        );
        response = Response.json(
          { error: "Internal Server Error" },
          { status: 500 },
        );
      }

      const headers = new Headers(response.headers);
      headers.set("Access-Control-Allow-Origin", "*");
      headers.set(
        "Access-Control-Allow-Methods",
        "GET, POST, PUT, DELETE, OPTIONS",
      );
      headers.set(
        "Access-Control-Allow-Headers",
        "Content-Type, Authorization, x-api-key",
      );

      const corsResponse = new Response(response.body, {
        status: response.status,
        statusText: response.statusText,
        headers,
      });

      const durationMs = performance.now() - start;
      ctx.logger.request(ip, method, pathname, corsResponse.status, durationMs);
      return corsResponse;
    },
  });

  printUnifiedNextSteps(
    wrapperHost,
    server.port ?? wrapperPort,
    llmPort,
    sttPort,
    imagePort,
    authRequired,
    authMode,
    enabled,
  );

  let shutdownPromise: Promise<void> | null = null;
  let exitPromise: Promise<never> | null = null;
  let requestedExitStatus = 0;
  const shutdown = (): Promise<void> => {
    if (!shutdownPromise) {
      shutdownPromise = (async () => {
        ctx.logger.info("Manager", "Shutting down servers and subprocesses...");
        server.stop(true);
        await Promise.all([
          llmService?.shutdown(),
          sttService?.shutdown(),
          imageService?.shutdown(),
        ]);
      })();
    }
    return shutdownPromise;
  };

  exitAfterShutdown = (status: number): Promise<never> => {
    requestedExitStatus = Math.max(requestedExitStatus, status);
    if (!exitPromise) {
      exitPromise = (async () => {
        try {
          await shutdown();
        } catch (err) {
          ctx.logger.error("Manager", "Shutdown failed", err as Error);
          requestedExitStatus = 1;
        }
        process.exit(requestedExitStatus);
      })();
    }
    return exitPromise;
  };

  // SIGKILL cannot run cleanup: POSIX does not let a process handle its own SIGKILL.
  process.once("SIGINT", () => void exitAfterShutdown?.(0));
  process.once("SIGTERM", () => void exitAfterShutdown?.(0));
  process.once("SIGHUP", () => void exitAfterShutdown?.(0));

  // Keep alive forever
  await new Promise(() => {});
  return 0;
}
