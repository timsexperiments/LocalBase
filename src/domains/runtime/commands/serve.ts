import { z } from "zod";
import { SpanStatusCode } from "@opentelemetry/api";
import { join, basename } from "node:path";
import { validateApiKey, installModel } from "../../../manager";
import {
  byId,
  evaluateModelFit,
  calculateMaxSafeContextSize,
  primaryArtifact,
  resolveCatalogInstallation,
} from "../../../catalog";
import type { AppContext } from "../../../context";
import { activateContextOtel } from "../../../context";
import { runtimeProcessSettings } from "../config-snapshot";
import { type ILogger } from "../../observability/logging";
import type { RuntimeModality } from "../modality";
import {
  RuntimeReconciler,
  RuntimeRequestAbortedError,
  type RuntimeAdmission,
} from "../runtime-reconciler";
import { type RuntimeOverrideOwnership } from "../reconciliation-plan";
import {
  createRuntimeSupervisorFactory,
  runtimeLaunchOverrides,
} from "../supervisor-factory";
import { MemorySafetyController } from "../memory-controller";
import { createHostMemoryProvider } from "../memory/host-memory-provider";
import { SupervisorRegistry } from "../supervisor-registry";
import { composeGatewayHealth } from "../gateway-health";
import { selectGatewayRoute } from "../route-dispatch";
import {
  acquireGatewayLease,
  acquireGatewayLeaseForServe,
} from "../../service/ownership";
import type { CommandExecution } from "../../app/commands/framework";
import type { ServeInput } from "../../app/commands/inputs";
import { CliInputError } from "../../app/commands/errors";
import {
  clientSpanOptions,
  serverSpanName,
  serverSpanOptions,
  type OtelRuntime,
} from "../../observability/otel";
import { gatewayIdentitySchema } from "../health";
import { openAIErrorResponseSchema, type OpenAIError } from "../openai-error";

type AuthMode = "bearer" | "x-api-key" | "either";

type ModalityState = Record<RuntimeModality, boolean>;

export function httpBaseUrl(host: string, port: number): string {
  const urlHost =
    host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  return `http://${urlHost}:${port}`;
}

const UPSTREAM_PROXY_TIMEOUT_MS = 120_000;
const MAX_REQUEST_BYTES = 25 * 1024 * 1024;

function parseAuthMode(raw: AuthMode | undefined): AuthMode {
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
  output: CommandExecution["output"],
): void {
  const baseUrl = httpBaseUrl(host, port);
  output.info("\nUnified API wrapper started.");
  output.info(`Wrapper base URL: ${baseUrl}`);
  if (enabled.llm) output.info(`OpenAI-compatible LLM endpoint: ${baseUrl}/v1`);
  if (enabled.stt)
    output.info(
      `OpenAI-compatible STT endpoint: ${baseUrl}/v1/audio/transcriptions`,
    );
  if (enabled.image)
    output.info(
      `OpenAI-compatible Image endpoint: ${baseUrl}/v1/images/generations`,
    );
  if (authRequired) {
    output.info(`Authentication: enabled (mode=${authMode}).`);
    output.info(
      "Supported credentials: Authorization: Bearer <key>, x-api-key: <key> (mode-dependent).",
    );
  } else {
    output.info("Authentication: disabled via --no-auth.");
  }
  output.info(
    `Enabled modalities: ${
      Object.entries(enabled)
        .filter(([, on]) => on)
        .map(([k]) => k)
        .join(", ") || "none"
    }`,
  );
  if (enabled.llm)
    output.info(`Upstream llama-server: http://127.0.0.1:${llmPort}`);
  if (enabled.stt)
    output.info(`Upstream whisper-server: http://127.0.0.1:${sttPort}`);
  if (enabled.image)
    output.info(`Upstream sd-server: http://127.0.0.1:${imagePort}`);

  if (enabled.llm) {
    output.info("\nExample chat request (Bearer):");
    output.info(
      `curl ${baseUrl}/v1/chat/completions -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"model":"<your-model>","messages":[{"role":"user","content":"hello"}]}'`,
    );
  }
  if (enabled.stt) {
    output.info("\nExample STT request (x-api-key):");
    output.info(
      `curl -X POST ${baseUrl}/v1/audio/transcriptions -H 'x-api-key: <API_KEY>' -F file=@audio.wav -F model=whisper`,
    );
  }
  if (enabled.image) {
    output.info("\nExample Image request (Bearer):");
    output.info(
      `curl ${baseUrl}/v1/images/generations -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"prompt":"A scenic sunset","n":1,"size":"512x512"}'`,
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

function openAIErrorResponse(
  error: OpenAIError,
  status: number,
  headers?: HeadersInit,
): Response {
  return Response.json(openAIErrorResponseSchema.parse({ error }), {
    status,
    headers,
  });
}

function routeNotFound(): Response {
  return openAIErrorResponse(
    {
      message: "This endpoint is not exposed by this gateway.",
      type: "invalid_request_error",
      param: null,
      code: "route_disabled",
    },
    404,
  );
}

function unauthorized(): Response {
  return openAIErrorResponse(
    {
      message: "Unauthorized: Invalid or missing API key.",
      type: "invalid_request_error",
      param: null,
      code: "invalid_api_key",
    },
    401,
    { "www-authenticate": "Bearer" },
  );
}

function notConfigured(feature: string): Response {
  return openAIErrorResponse(
    {
      message: `${feature} route is disabled.`,
      type: "invalid_request_error",
      param: null,
      code: "route_disabled",
    },
    501,
  );
}

function badRequest(message: string): Response {
  return openAIErrorResponse(
    {
      message,
      type: "invalid_request_error",
      param: null,
      code: "validation_failed",
    },
    400,
  );
}

function methodNotAllowed(allow: string): Response {
  return openAIErrorResponse(
    {
      message: "Method not allowed.",
      type: "invalid_request_error",
      param: null,
      code: "method_not_allowed",
    },
    405,
    { Allow: allow },
  );
}

function modelNotFound(model: string): Response {
  return openAIErrorResponse(
    {
      message: `The model '${model}' does not exist.`,
      type: "invalid_request_error",
      param: "model",
      code: "model_not_found",
    },
    404,
  );
}

function payloadTooLarge(): Response {
  return openAIErrorResponse(
    {
      message: `Request body exceeds the ${MAX_REQUEST_BYTES / (1024 * 1024)} MiB limit.`,
      type: "invalid_request_error",
      param: null,
      code: "payload_too_large",
    },
    413,
  );
}

function upstreamFailure(message: string): Response {
  return openAIErrorResponse(
    {
      message,
      type: "server_error",
      param: null,
      code: "upstream_error",
    },
    502,
  );
}

export function internalGatewayFailure(): Response {
  return openAIErrorResponse(
    {
      message: "The gateway encountered an unexpected error.",
      type: "server_error",
      param: null,
      code: "gateway_error",
    },
    500,
  );
}

function validationFailure(error: z.ZodError): Response {
  const issues = error.issues
    .map((issue) => `${issue.path.join(".")}: ${issue.message}`)
    .join(", ");
  return badRequest(`Validation failed: ${issues}`);
}

const chatToolCallSchema = z
  .object({
    id: z.string().min(1),
    type: z.literal("function"),
    function: z
      .object({
        name: z.string().min(1),
        arguments: z.string(),
      })
      .passthrough(),
  })
  .passthrough();

const jsonSchemaValueSchema: z.ZodType = z.lazy(() =>
  z.union([
    z.string(),
    z.number(),
    z.boolean(),
    z.null(),
    z.array(jsonSchemaValueSchema),
    z.record(z.string(), jsonSchemaValueSchema),
  ]),
);

const functionToolSchema = z
  .object({
    type: z.literal("function"),
    function: z
      .object({
        name: z.string().min(1),
        description: z.string().optional(),
        parameters: z.record(z.string(), jsonSchemaValueSchema).optional(),
        strict: z.boolean().optional(),
      })
      .strict(),
  })
  .strict();

const chatResponseFormatSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("text") }).strict(),
  z.object({ type: z.literal("json_object") }).strict(),
  z
    .object({
      type: z.literal("json_schema"),
      json_schema: z
        .object({
          name: z.string().min(1),
          description: z.string().optional(),
          schema: z.record(z.string(), jsonSchemaValueSchema),
          strict: z.boolean().optional(),
        })
        .strict(),
    })
    .strict(),
]);

const modelIdSchema = z
  .string()
  .refine((value) => value.trim().length > 0, "model must not be empty");

const textContentPartSchema = z
  .object({ type: z.literal("text"), text: z.string() })
  .passthrough();

const chatFileSchema = z.union([
  z.object({ file_id: z.string().min(1) }).strict(),
  z
    .object({
      filename: z.string().min(1),
      file_data: z.string().min(1),
    })
    .strict(),
]);

const userContentPartSchema = z.union([
  textContentPartSchema,
  z
    .object({
      type: z.literal("image_url"),
      image_url: z
        .object({
          url: z.string().min(1),
          detail: z.enum(["auto", "low", "high"]).optional(),
        })
        .passthrough(),
    })
    .passthrough(),
  z
    .object({
      type: z.literal("input_audio"),
      input_audio: z
        .object({
          data: z.string().min(1),
          format: z.enum(["wav", "mp3"]),
        })
        .passthrough(),
    })
    .passthrough(),
  z
    .object({
      type: z.literal("file"),
      file: chatFileSchema,
    })
    .passthrough(),
]);

const userContentSchema = z.union([z.string(), z.array(userContentPartSchema)]);

const systemDeveloperContentSchema = z.union([
  z.string(),
  z.array(textContentPartSchema).min(1),
]);

const assistantContentPartSchema = z.union([
  textContentPartSchema,
  z.object({ type: z.literal("refusal"), refusal: z.string() }).passthrough(),
]);

const assistantContentSchema = z.union([
  z.string(),
  z.null(),
  z.array(assistantContentPartSchema),
]);

const toolResultContentSchema = z.union([
  z.string(),
  z.array(
    z.object({ type: z.literal("text"), text: z.string() }).passthrough(),
  ),
]);

const assistantMessageSchema = z
  .object({
    role: z.literal("assistant"),
    content: assistantContentSchema.optional(),
    name: z.string().optional(),
    tool_calls: z.array(chatToolCallSchema).optional(),
    refusal: z.string().nullable().optional(),
    function_call: z.never().optional(),
  })
  .refine(
    (message) =>
      (message.content !== undefined && message.content !== null) ||
      (message.tool_calls?.length ?? 0) > 0 ||
      (message.refusal !== undefined && message.refusal !== null),
    "assistant messages require content, tool_calls, or refusal",
  )
  .passthrough();

const chatMessageSchema = z.discriminatedUnion("role", [
  z
    .object({
      role: z.enum(["system", "developer"]),
      content: systemDeveloperContentSchema,
      name: z.string().optional(),
    })
    .passthrough(),
  z
    .object({
      role: z.literal("user"),
      content: userContentSchema,
      name: z.string().optional(),
    })
    .passthrough(),
  assistantMessageSchema,
  z
    .object({
      role: z.literal("tool"),
      content: toolResultContentSchema,
      tool_call_id: z.string().min(1),
    })
    .passthrough(),
]);

const chatCompletionRequestSchema = z
  .object({
    model: modelIdSchema,
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
    response_format: chatResponseFormatSchema.optional(),
    tools: z.array(functionToolSchema).optional(),
    tool_choice: z
      .union([
        z.enum(["none", "auto", "required"]),
        z
          .object({
            type: z.literal("function"),
            function: z.object({ name: z.string().min(1) }).strict(),
          })
          .strict(),
      ])
      .optional(),
  })
  .passthrough();

const imageGenerationRequestSchema = z
  .object({
    prompt: z.string(),
    model: modelIdSchema.optional(),
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
  model: modelIdSchema.optional(),
  language: z.string().optional(),
  prompt: z.string().optional(),
  include: z.array(z.string()).optional(),
  response_format: z
    .enum(["json", "text", "srt", "verbose_json", "vtt"])
    .optional(),
  temperature: z.preprocess(
    (value) =>
      typeof value === "string" && value.trim() !== "" ? Number(value) : value,
    z.number().min(0).max(1).optional(),
  ),
  timestamp_granularities: z.array(z.enum(["word", "segment"])).optional(),
});

const embeddingsRequestSchema = z
  .object({
    model: modelIdSchema,
    input: z.union([
      z.string(),
      z.array(z.string()),
      z.array(z.number()),
      z.array(z.array(z.number())),
    ]),
    encoding_format: z.literal("float").optional(),
    dimensions: z.number().int().positive().optional(),
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
      z
        .object({
          index: z.number(),
          message: assistantMessageSchema,
          finish_reason: z.string().nullable().optional(),
        })
        .passthrough(),
    ),
    usage: z
      .object({
        prompt_tokens: z.number(),
        completion_tokens: z.number(),
        total_tokens: z.number(),
      })
      .passthrough()
      .optional(),
  })
  .passthrough();

const chatCompletionStreamDeltaSchema = z
  .object({
    role: z.literal("assistant").optional(),
    content: z.string().nullable().optional(),
    refusal: z.string().nullable().optional(),
    reasoning_content: z.string().nullable().optional(),
    tool_calls: z
      .array(
        z
          .object({
            index: z.number().int().nonnegative(),
            id: z.string().min(1).optional(),
            type: z.literal("function").optional(),
            function: z
              .object({
                name: z.string().min(1).optional(),
                arguments: z.string().optional(),
              })
              .strict()
              .optional(),
          })
          .strict(),
      )
      .optional(),
  })
  .strict();

const topTokenLogprobSchema = z
  .object({
    id: z.number().int(),
    token: z.string(),
    bytes: z.array(z.number().int().min(0).max(255)),
    logprob: z.number(),
  })
  .strict();

const tokenLogprobSchema = z
  .object({
    id: z.number().int(),
    token: z.string(),
    bytes: z.array(z.number().int().min(0).max(255)),
    logprob: z.number(),
    top_logprobs: z.array(topTokenLogprobSchema),
  })
  .strict();

const chatCompletionStreamLogprobsSchema = z
  .object({
    content: z.array(tokenLogprobSchema).nullable(),
    refusal: z.array(tokenLogprobSchema).nullable().optional(),
  })
  .strict();

const promptTokensDetailsSchema = z
  .object({
    audio_tokens: z.number().int().nonnegative().optional(),
    cached_tokens: z.number().int().nonnegative().optional(),
  })
  .strict();

const completionTokensDetailsSchema = z
  .object({
    accepted_prediction_tokens: z.number().int().nonnegative().optional(),
    audio_tokens: z.number().int().nonnegative().optional(),
    reasoning_tokens: z.number().int().nonnegative().optional(),
    rejected_prediction_tokens: z.number().int().nonnegative().optional(),
  })
  .strict();

const chatCompletionUsageSchema = z
  .object({
    prompt_tokens: z.number().int().nonnegative(),
    completion_tokens: z.number().int().nonnegative(),
    total_tokens: z.number().int().nonnegative(),
    prompt_tokens_details: promptTokensDetailsSchema.optional(),
    completion_tokens_details: completionTokensDetailsSchema.optional(),
  })
  .strict();

const llamaTimingsSchema = z
  .object({
    cache_n: z.number(),
    prompt_n: z.number(),
    prompt_ms: z.number(),
    prompt_per_token_ms: z.number(),
    prompt_per_second: z.number(),
    predicted_n: z.number(),
    predicted_ms: z.number(),
    predicted_per_token_ms: z.number(),
    predicted_per_second: z.number(),
    draft_n: z.number().optional(),
    draft_n_accepted: z.number().optional(),
  })
  .strict();

const llamaPromptProgressSchema = z
  .object({
    total: z.number(),
    cache: z.number(),
    processed: z.number(),
    time_ms: z.number(),
  })
  .strict();

const chatCompletionStreamChunkSchema = z
  .object({
    id: z.string(),
    object: z.literal("chat.completion.chunk"),
    created: z.number(),
    model: z.string(),
    choices: z.array(
      z
        .object({
          index: z.number(),
          delta: chatCompletionStreamDeltaSchema,
          finish_reason: z
            .enum(["stop", "length", "tool_calls", "content_filter"])
            .nullable(),
          logprobs: chatCompletionStreamLogprobsSchema.nullable().optional(),
        })
        .strict(),
    ),
    usage: chatCompletionUsageSchema.nullable().optional(),
    system_fingerprint: z.string().nullable().optional(),
    service_tier: z.string().nullable().optional(),
    timings: llamaTimingsSchema.optional(),
    prompt_progress: llamaPromptProgressSchema.optional(),
  })
  .strict();

const chatCompletionStreamEventSchema = z.union([
  chatCompletionStreamChunkSchema,
  openAIErrorResponseSchema,
]);

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
        b64_json: z.string(),
        revised_prompt: z.string().optional(),
      }),
    ),
  })
  .passthrough();

const transcriptionResponseSchema = z
  .object({
    task: z.string().optional(),
    language: z.string().optional(),
    duration: z.number().optional(),
    text: z.string(),
    segments: z
      .array(
        z
          .object({
            id: z.number(),
            seek: z.number(),
            start: z.number(),
            end: z.number(),
            text: z.string(),
            tokens: z.array(z.number()),
            temperature: z.number(),
            avg_logprob: z.number(),
            compression_ratio: z.number(),
            no_speech_prob: z.number(),
          })
          .passthrough(),
      )
      .optional(),
  })
  .passthrough();

class PayloadTooLargeError extends Error {}

class RequestAbortedError extends Error {
  constructor() {
    super("Request was aborted.");
    this.name = "RequestAbortedError";
  }
}

function waitForRequestAbort<T>(
  promise: Promise<T>,
  signal: AbortSignal,
): Promise<T> {
  if (signal.aborted) return Promise.reject(new RequestAbortedError());

  return new Promise<T>((resolve, reject) => {
    let settled = false;
    const finish = (callback: () => void) => {
      if (settled) return;
      settled = true;
      signal.removeEventListener("abort", onAbort);
      callback();
    };
    const onAbort = () => finish(() => reject(new RequestAbortedError()));

    signal.addEventListener("abort", onAbort, { once: true });
    void promise.then(
      (value) => finish(() => resolve(value)),
      (error) => finish(() => reject(error)),
    );
  });
}

function requestAborted(): Response {
  return new Response(null, { status: 499 });
}

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
    signal: request.signal,
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
    "baggage",
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
  const contentType = response.headers.get("content-type");
  if (!contentType) return false;
  return (
    contentType.split(";", 1)[0]?.trim().toLowerCase() === "text/event-stream"
  );
}

function eventData(event: string): string | undefined {
  const values: string[] = [];
  for (const line of event.split(/\r\n|\r|\n/)) {
    if (!line.startsWith("data:")) continue;
    const value = line.slice("data:".length);
    values.push(value.startsWith(" ") ? value.slice(1) : value);
  }
  return values.length > 0 ? values.join("\n") : undefined;
}

function validateEventStream(
  body: ReadableStream<Uint8Array>,
  schema: z.ZodType,
): ReadableStream<Uint8Array> {
  const decoder = new TextDecoder();
  const encoder = new TextEncoder();
  const boundary = /(?:\r\n|\r|\n)(?:\r\n|\r|\n)/;
  let buffered = "";
  let doneEvent: string | undefined;
  let failed = false;
  const choiceFinished = new Map<number, boolean>();

  const validationFailure = `data: ${JSON.stringify({
    error: {
      message: "The upstream service returned an invalid event stream.",
      type: "server_error",
      param: null,
      code: "upstream_error",
    },
  })}\n\ndata: [DONE]\n\n`;

  const fail = (
    controller: TransformStreamDefaultController<Uint8Array>,
    terminate: boolean,
  ): false => {
    if (!failed) controller.enqueue(encoder.encode(validationFailure));
    failed = true;
    if (terminate) controller.terminate();
    return false;
  };

  const flushEvent = (
    controller: TransformStreamDefaultController<Uint8Array>,
    event: string,
    terminateOnFailure: boolean,
    framed: boolean,
  ): boolean => {
    const hasFields = event.split(/\r\n|\r|\n/).some((line) => line.length > 0);
    if (!hasFields) {
      if (!doneEvent) controller.enqueue(encoder.encode(event));
      return true;
    }

    if (doneEvent) return fail(controller, terminateOnFailure);

    const data = eventData(event);
    if (data === "[DONE]") {
      if (
        !framed ||
        choiceFinished.size === 0 ||
        [...choiceFinished.values()].some((finished) => !finished)
      ) {
        return fail(controller, terminateOnFailure);
      }
      doneEvent = event;
      return true;
    }
    if (data !== undefined) {
      try {
        const parsed = schema.safeParse(JSON.parse(data));
        if (!parsed.success) {
          return fail(controller, terminateOnFailure);
        }
        const value = parsed.data as
          | z.infer<typeof chatCompletionStreamChunkSchema>
          | z.infer<typeof openAIErrorResponseSchema>;
        if ("error" in value) {
          controller.enqueue(encoder.encode(event));
          controller.terminate();
          return false;
        }
        for (const choice of value.choices) {
          if (choiceFinished.get(choice.index)) {
            return fail(controller, terminateOnFailure);
          }
          choiceFinished.set(choice.index, choice.finish_reason !== null);
        }
      } catch {
        return fail(controller, terminateOnFailure);
      }
    }

    controller.enqueue(encoder.encode(event));
    return true;
  };

  return body.pipeThrough(
    new TransformStream<Uint8Array, Uint8Array>({
      transform(chunk, controller) {
        buffered += decoder.decode(chunk, { stream: true });
        while (true) {
          const match = boundary.exec(buffered);
          if (!match || match.index === undefined) return;
          const end = match.index + match[0].length;
          if (!flushEvent(controller, buffered.slice(0, end), true, true)) {
            return;
          }
          buffered = buffered.slice(end);
        }
      },
      flush(controller) {
        buffered += decoder.decode();
        if (buffered && !flushEvent(controller, buffered, false, false)) return;
        if (failed) return;
        if (!doneEvent) {
          fail(controller, false);
          return;
        }
        controller.enqueue(encoder.encode(doneEvent));
      },
    }),
  );
}

async function proxyRequest(
  request: Request,
  targetBase: string,
  pathOverride?: string,
  responseSchema?: z.ZodType,
  eventStreamSchema?: z.ZodType,
  otel?: OtelRuntime,
): Promise<Response> {
  const incoming = new URL(request.url);
  const path = pathOverride ?? incoming.pathname;
  const target = `${targetBase}${path}${incoming.search}`;
  let upstream: Response;
  const fetchUpstream = async (): Promise<Response> => {
    const headers = filterProxyHeaders(request.headers);
    otel?.inject(headers);
    return await fetch(target, {
      method: request.method,
      headers,
      body: request.body,
      signal: AbortSignal.any([
        request.signal,
        AbortSignal.timeout(UPSTREAM_PROXY_TIMEOUT_MS),
      ]),
    });
  };
  try {
    upstream = otel
      ? await otel.withSpan(
          "localbase.backend.inference",
          clientSpanOptions({
            "server.address": new URL(targetBase).hostname,
            "http.request.method": request.method,
          }),
          async (span) => {
            const response = await fetchUpstream();
            span.setAttribute("http.response.status_code", response.status);
            if (response.status >= 500) {
              span.setStatus({
                code: SpanStatusCode.ERROR,
                message: "Upstream returned a server error.",
              });
            }
            return response;
          },
        )
      : await fetchUpstream();
  } catch {
    if (request.signal.aborted) return requestAborted();
    return upstreamFailure("The upstream service could not be reached.");
  }

  if (
    responseSchema &&
    eventStreamSchema &&
    isEventStream(upstream) &&
    upstream.ok
  ) {
    if (!upstream.body) {
      return upstreamFailure(
        "The upstream service returned an invalid response.",
      );
    }
    const headers = filterProxyHeaders(upstream.headers);
    headers.delete("content-length");
    return new Response(validateEventStream(upstream.body, eventStreamSchema), {
      status: upstream.status,
      statusText: upstream.statusText,
      headers,
    });
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

  if (upstream.status >= 400 && upstream.status <= 599) {
    try {
      const parsed = openAIErrorResponseSchema.safeParse(await upstream.json());
      if (parsed.success) {
        return openAIErrorResponse(parsed.data.error, upstream.status);
      }
    } catch {}
  }

  return upstreamFailure(
    "The upstream service returned an invalid error response.",
  );
}

/** Keeps an active-model lease until the client finishes or cancels the response. */
export function withResponseLease(
  response: Response,
  release: () => void,
  requestSignal: AbortSignal,
): Response {
  if (!response.body) {
    release();
    return response;
  }

  let released = false;
  let removeAbortListener = () => {};
  const releaseOnce = () => {
    if (released) return;
    released = true;
    removeAbortListener();
    release();
  };
  const reader = response.body.getReader();
  const cancelForRequestAbort = () => {
    releaseOnce();
    void reader.cancel(requestSignal.reason);
  };
  requestSignal.addEventListener("abort", cancelForRequestAbort, {
    once: true,
  });
  removeAbortListener = () =>
    requestSignal.removeEventListener("abort", cancelForRequestAbort);
  if (requestSignal.aborted) cancelForRequestAbort();
  const body = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const { done, value } = await reader.read();
        if (done) {
          releaseOnce();
          controller.close();
          return;
        }
        controller.enqueue(value);
      } catch (error) {
        releaseOnce();
        controller.error(error);
      }
    },
    async cancel(reason) {
      releaseOnce();
      await reader.cancel(reason);
    },
  });

  return new Response(body, {
    status: response.status,
    statusText: response.statusText,
    headers: response.headers,
  });
}

/**
 * Returns a standard HTTP 503 service unavailable response.
 */
function serviceUnavailable(serviceName: string): Response {
  return openAIErrorResponse(
    {
      message: `${serviceName} service is currently restarting or unavailable. Please try again shortly.`,
      type: "api_error",
      param: null,
      code: "service_unavailable",
    },
    503,
    { "Retry-After": "5" },
  );
}

export function resourceUnavailable(): Response {
  return openAIErrorResponse(
    {
      message:
        "Insufficient available memory to start the requested runtime. Please try again shortly.",
      type: "api_error",
      param: null,
      code: "insufficient_memory",
    },
    503,
    { "Retry-After": "5" },
  );
}

export async function finalizeGatewayShutdown(
  logger: ILogger,
  releaseLease: () => Promise<void>,
  exitStatus: number,
): Promise<number> {
  let finalStatus = exitStatus;
  try {
    await releaseLease();
  } catch (error) {
    finalStatus = 1;
    logger.event({
      severity: "error",
      eventName: "gateway.lease-release-failed",
      category: "gateway",
      component: "gateway",
      runtime: "gateway",
      message: "Gateway ownership release failed during shutdown.",
      error: {
        type: error instanceof Error ? error.name : "Error",
        message: error instanceof Error ? error.message : String(error),
      },
    });
  }
  logger.event({
    severity: "info",
    eventName: "gateway.stopped",
    category: "gateway",
    component: "gateway",
    runtime: "gateway",
    message: "LocalBase gateway stopped.",
    attributes: { exitCode: finalStatus },
  });
  try {
    await logger.close();
  } catch {
    finalStatus = 1;
  }
  return finalStatus;
}

export async function runServe(
  input: ServeInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { exitCode: number }; exitCode: number }> {
  const config = ctx.config;
  const wrapperHost = input.host ?? "0.0.0.0";
  const wrapperPort = input.port ?? 2273;

  const llmPort = input.llmPort ?? config.port;
  const sttPort = input.sttPort ?? config.sttPort;
  const imagePort = input.imagePort ?? 8090;

  const serviceId = process.env.LOCALBASE_SERVICE_ID;
  const serviceToken = process.env.LOCALBASE_SERVICE_TOKEN;
  const processSettings = runtimeProcessSettings(config.root, {
    host: wrapperHost,
    port: wrapperPort,
  });
  const endpoint = {
    host: processSettings.gateway.host,
    port: processSettings.gateway.port,
    ...(serviceId || serviceToken ? { serviceId, serviceToken } : {}),
  };
  const gatewayLease = ctx.initializationOperation
    ? await acquireGatewayLease(processSettings.root, endpoint)
    : await acquireGatewayLeaseForServe(processSettings.root, endpoint);
  await ctx.logger.enableFileLogging(processSettings.root);
  await ctx.initializationOperation?.release();
  ctx.initializationOperation = undefined;
  await activateContextOtel(ctx);
  ctx.logger.event({
    severity: "info",
    eventName: "gateway.starting",
    category: "gateway",
    component: "gateway",
    runtime: "gateway",
    message: "Starting LocalBase gateway.",
  });

  let ctxSize = input.ctxSize ?? 0;
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

  const sttPath = input.sttPath ?? "/inference";
  const authRequired = input.auth ?? true;
  const authMode = parseAuthMode(input.authMode);

  const llmModelFileOverride = input.llmModelFile;
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

  const resolveSttModelFile = async (
    runtimeConfig: typeof config,
    modelId: string,
  ): Promise<string> => {
    if (input.sttModelFile && modelId === config.activeSttModel) {
      return input.sttModelFile;
    }
    const spec = byId(modelId);
    const primaryFilename = spec && primaryArtifact(spec).filename;
    if (
      primaryFilename &&
      (await Bun.file(
        join(runtimeConfig.sttModelsDir, primaryFilename),
      ).exists())
    ) {
      return primaryFilename;
    } else if (
      await Bun.file(
        join(runtimeConfig.sttModelsDir, `${modelId}.bin`),
      ).exists()
    ) {
      return `${modelId}.bin`;
    }
    return `${modelId}.gguf`;
  };

  let sttModelFile = await resolveSttModelFile(config, config.activeSttModel);

  let imageModelFile = input.imageModelFile;
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
    llm: input.llm ?? true,
    stt: input.stt ?? config.selectedSttModels.length > 0,
    image: input.image ?? config.selectedImageModels.length > 0,
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

  const bypassCheck = input.bypassMemoryCheck;

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
          throw new Error("LLM model does not fit available memory.");
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
          throw new Error("STT model does not fit available memory.");
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
          throw new Error("Image model does not fit available memory.");
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
    throw new CliInputError(
      "No modalities enabled. Remove at least one --no-<modality> option.",
    );
  }

  if (!enabled.llm && input.llm === undefined) {
    console.log("LLM route auto-disabled (no local LLM model file found).");
  }
  if (!enabled.stt && input.stt === undefined) {
    console.log("STT route auto-disabled (no local STT model file found).");
  }
  if (!enabled.image && input.image === undefined) {
    console.log("Image route auto-disabled (no local Image model file found).");
  }

  const configuredOverrides: RuntimeOverrideOwnership = {
    configFields: [
      ...(input.llmHost || process.env.LOCALBASE_HOST ? ["host" as const] : []),
      ...(input.llmPort || process.env.LOCALBASE_PORT ? ["port" as const] : []),
      ...(input.ctxSize || process.env.LOCALBASE_CTX_SIZE
        ? ["ctxSize" as const]
        : []),
      ...(input.sttHost || process.env.LOCALBASE_STT_HOST
        ? ["sttHost" as const]
        : []),
      ...(input.sttPort || process.env.LOCALBASE_STT_PORT
        ? ["sttPort" as const]
        : []),
      ...(input.llmModelFile ? ["activeLlmModel" as const] : []),
      ...(input.sttModelFile ? ["activeSttModel" as const] : []),
      ...(input.imageModelFile ? ["activeImageModel" as const] : []),
    ],
    configuredModalities: {
      ...(input.llm === undefined ? {} : { llm: input.llm }),
      ...(input.stt === undefined ? {} : { stt: input.stt }),
      ...(input.image === undefined ? {} : { image: input.image }),
    },
  };
  const launchOverrides = Object.freeze({
    ...runtimeLaunchOverrides(input),
    ...(input.llmHost || !process.env.LOCALBASE_HOST
      ? {}
      : { llmHost: config.host }),
    ...(input.llmPort || !process.env.LOCALBASE_PORT
      ? {}
      : { llmPort: config.port }),
    ...(input.ctxSize || !process.env.LOCALBASE_CTX_SIZE
      ? {}
      : { ctxSize: config.ctxSize }),
    ...(input.sttHost || !process.env.LOCALBASE_STT_HOST
      ? {}
      : { sttHost: config.sttHost }),
    ...(input.sttPort || !process.env.LOCALBASE_STT_PORT
      ? {}
      : { sttPort: config.sttPort }),
  });
  const memoryProvider = createHostMemoryProvider();
  const memorySafety = new MemorySafetyController(
    memoryProvider,
    ctx.config.memory,
    bypassCheck,
  );
  const factory = createRuntimeSupervisorFactory(ctx, launchOverrides, {
    memorySafety,
  });
  const initialSnapshot = ctx.runtimeConfig.read();
  const supervisors = new SupervisorRegistry({
    ...(enabled.llm ? { llm: factory.create("llm", initialSnapshot) } : {}),
    ...(enabled.stt ? { stt: factory.create("stt", initialSnapshot) } : {}),
    ...(enabled.image
      ? { image: factory.create("image", initialSnapshot) }
      : {}),
  });
  const reconciler = new RuntimeReconciler(
    ctx.runtimeConfig,
    configuredOverrides,
    supervisors,
    factory,
    ctx.logger,
  );

  const gatewayStartedAt = Date.now();
  let gatewayStopping = false;
  const healthSnapshot = () =>
    composeGatewayHealth({
      startedAtMs: gatewayStartedAt,
      nowMs: Date.now(),
      stopping: gatewayStopping,
      configured: reconciler.configuredModalities(),
      supervisors,
    });

  const proxyWithAdmission = async (
    admission: RuntimeAdmission,
    serviceName: string,
    requestSignal: AbortSignal,
    dispatch: () => Promise<Response>,
  ): Promise<Response> => {
    try {
      admission.onDetach(() => {
        if (admission.supervisor.state() === "starting") {
          void admission.supervisor.kill();
        }
      });
      await waitForRequestAbort(
        admission.supervisor.ensureRunning(),
        requestSignal,
      );
      const response = await waitForRequestAbort(dispatch(), requestSignal);
      admission.markResponseStarted();
      return withResponseLease(response, admission.release, requestSignal);
    } catch (error) {
      admission.release();
      if (error instanceof RequestAbortedError) return requestAborted();
      return serviceUnavailable(serviceName);
    }
  };

  const handleRequest = async (
    request: Request,
    pathname: string,
  ): Promise<Response> => {
    const route = selectGatewayRoute(pathname);
    if (route === "health") {
      if (request.method !== "GET" && request.method !== "HEAD") {
        return methodNotAllowed("GET, HEAD");
      }
      const health = healthSnapshot();
      const body = JSON.stringify(health);
      return new Response(request.method === "HEAD" ? null : body, {
        status: health.status === "ok" ? 200 : 503,
        headers: {
          "content-type": "application/json; charset=utf-8",
          "content-length": String(new TextEncoder().encode(body).byteLength),
        },
      });
    }

    await reconciler.refresh();
    const currentConfig = ctx.runtimeConfig.copy();

    if (route === "instance") {
      if (
        request.headers.get("x-localbase-instance-token") !==
        gatewayLease.instance.instanceToken
      ) {
        return new Response(null, { status: 404 });
      }
      return Response.json(
        gatewayIdentitySchema.parse({
          instanceId: gatewayLease.instance.instanceId,
          rootHash: gatewayLease.instance.rootHash,
        }),
      );
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
        return unauthorized();
      }
    }

    if (requestExceedsSizeLimit(request)) return payloadTooLarge();

    if (route === "transcription") {
      let admission: RuntimeAdmission | undefined;
      try {
        const body = await readBoundedRequestBody(request);
        const multipartBody = new ArrayBuffer(body.byteLength);
        new Uint8Array(multipartBody).set(body);
        const formData = await new Request(request.url, {
          method: request.method,
          headers: request.headers,
          body: multipartBody,
          signal: request.signal,
        }).formData();
        const bodyObj: Record<
          string,
          FormDataEntryValue | FormDataEntryValue[]
        > = {};
        for (const [key, value] of formData.entries()) {
          const repeatedField =
            key === "include[]" || key === "timestamp_granularities[]";
          const normalizedKey =
            key === "include[]"
              ? "include"
              : key === "timestamp_granularities[]"
                ? "timestamp_granularities"
                : key;
          const existing = bodyObj[normalizedKey];
          bodyObj[normalizedKey] =
            existing === undefined
              ? repeatedField
                ? [value]
                : value
              : Array.isArray(existing)
                ? [...existing, value]
                : [existing, value];
        }

        const parsed = transcriptionRequestSchema.safeParse(bodyObj);
        if (!parsed.success) {
          return validationFailure(parsed.error);
        }
        const selected = await reconciler.admitModel(
          "stt",
          parsed.data.model,
          request.signal,
        );
        if (selected.kind === "not-configured") return notConfigured("STT");
        if (selected.kind === "model-not-found") {
          return modelNotFound(parsed.data.model ?? "");
        }
        if (selected.kind === "resource-unavailable") {
          return resourceUnavailable();
        }
        if (selected.kind === "unavailable") return serviceUnavailable("STT");
        admission = selected.value.admission;
        const normalizedForm = new FormData();
        for (const [key, value] of Object.entries(parsed.data)) {
          if (value === undefined) continue;
          const formKey =
            key === "include" || key === "timestamp_granularities"
              ? `${key}[]`
              : key;
          const values = Array.isArray(value) ? value : [value];
          for (const item of values) {
            normalizedForm.append(
              formKey,
              item instanceof Blob ? item : String(item),
            );
          }
        }
        request = new Request(request.url, {
          method: request.method,
          body: normalizedForm,
          signal: request.signal,
        });
      } catch (e) {
        admission?.release();
        if (e instanceof RuntimeRequestAbortedError) return requestAborted();
        return e instanceof PayloadTooLargeError
          ? payloadTooLarge()
          : badRequest("Invalid form data payload.");
      }
      return await proxyWithAdmission(
        admission!,
        "STT",
        request.signal,
        async () =>
          await proxyRequest(
            request,
            factory.baseUrl("stt", admission!.snapshot),
            sttPath,
            transcriptionResponseSchema,
            undefined,
            ctx.otel,
          ),
      );
    }

    if (route === "imageGeneration") {
      const parsed = await parseJsonRequest(
        request,
        imageGenerationRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      let selected;
      try {
        selected = await reconciler.admitModel(
          "image",
          parsed.data.model,
          request.signal,
        );
      } catch (error) {
        if (error instanceof RuntimeRequestAbortedError)
          return requestAborted();
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("Image");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "resource-unavailable") {
        return resourceUnavailable();
      }
      if (selected.kind === "unavailable") return serviceUnavailable("Image");
      return await proxyWithAdmission(
        selected.value.admission,
        "Image",
        request.signal,
        async () =>
          await proxyRequest(
            requestWithJsonBody(request, parsed.data),
            factory.baseUrl("image", selected.value.admission.snapshot),
            undefined,
            imageGenerationResponseSchema,
            undefined,
            ctx.otel,
          ),
      );
    }

    if (route === "chatCompletion") {
      const parsed = await parseJsonRequest(
        request,
        chatCompletionRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      let selected;
      try {
        selected = await reconciler.admitModel(
          "llm",
          parsed.data.model,
          request.signal,
        );
      } catch (error) {
        if (error instanceof RuntimeRequestAbortedError)
          return requestAborted();
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("LLM");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "resource-unavailable") {
        return resourceUnavailable();
      }
      if (selected.kind === "unavailable") return serviceUnavailable("LLM");
      return await proxyWithAdmission(
        selected.value.admission,
        "LLM",
        request.signal,
        async () =>
          await proxyRequest(
            requestWithJsonBody(request, parsed.data),
            factory.baseUrl("llm", selected.value.admission.snapshot),
            undefined,
            chatCompletionResponseSchema,
            chatCompletionStreamEventSchema,
            ctx.otel,
          ),
      );
    }

    if (route === "embeddings") {
      const parsed = await parseJsonRequest(request, embeddingsRequestSchema);
      if (!parsed.success) return parsed.response;
      let selected;
      try {
        selected = await reconciler.admitModel(
          "llm",
          parsed.data.model,
          request.signal,
        );
      } catch (error) {
        if (error instanceof RuntimeRequestAbortedError)
          return requestAborted();
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("LLM");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "resource-unavailable") {
        return resourceUnavailable();
      }
      if (selected.kind === "unavailable") return serviceUnavailable("LLM");
      return await proxyWithAdmission(
        selected.value.admission,
        "LLM",
        request.signal,
        async () =>
          await proxyRequest(
            requestWithJsonBody(request, parsed.data),
            factory.baseUrl("llm", selected.value.admission.snapshot),
            undefined,
            embeddingsResponseSchema,
            undefined,
            ctx.otel,
          ),
      );
    }

    if (route === "models") {
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

    return routeNotFound();
  };

  const server = Bun.serve({
    hostname: wrapperHost,
    port: wrapperPort,
    fetch: async (request) => {
      const start = performance.now();
      const { pathname } = new URL(request.url);
      const method = request.method;
      const requestId = request.headers.get("x-request-id") ?? undefined;
      const parent = ctx.otel.extract(request.headers);
      return await ctx.otel.withSpan(
        serverSpanName(method, pathname),
        serverSpanOptions(method, pathname),
        async (span) => {
          if (method === "OPTIONS") {
            span.setAttribute("http.response.status_code", 204);
            return new Response(null, {
              status: 204,
              headers: {
                "Access-Control-Allow-Origin": "*",
                "Access-Control-Allow-Methods":
                  "GET, POST, PUT, DELETE, OPTIONS",
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
            response = internalGatewayFailure();
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
          span.setAttribute("http.response.status_code", corsResponse.status);
          if (corsResponse.status >= 500) {
            span.setStatus({ code: SpanStatusCode.ERROR });
          }
          ctx.logger.request(
            method,
            pathname,
            corsResponse.status,
            durationMs,
            requestId,
          );
          return corsResponse;
        },
        parent,
      );
    },
  });

  ctx.logger.event({
    severity: "info",
    eventName: "gateway.started",
    category: "gateway",
    component: "gateway",
    runtime: "gateway",
    message: "LocalBase gateway is listening.",
    attributes: {
      host: wrapperHost,
      port: server.port ?? wrapperPort,
      llmEnabled: enabled.llm,
      sttEnabled: enabled.stt,
      imageEnabled: enabled.image,
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
    execution.output,
  );
  execution.output.lifecycle({
    event: "started",
    baseUrl: httpBaseUrl(wrapperHost, server.port ?? wrapperPort),
    enabled,
  });

  let shutdownPromise: Promise<void> | null = null;
  let exitPromise: Promise<number> | null = null;
  let exitAfterShutdown: ((status: number) => Promise<number>) | undefined;
  let requestedExitStatus = 0;
  let resolveServeExit: (status: number) => void;
  const serveExit = new Promise<number>((resolve) => {
    resolveServeExit = resolve;
  });
  const shutdown = (): Promise<void> => {
    if (!shutdownPromise) {
      shutdownPromise = (async () => {
        ctx.logger.event({
          severity: "info",
          eventName: "gateway.stopping",
          category: "gateway",
          component: "gateway",
          runtime: "gateway",
          message: "Stopping LocalBase gateway.",
        });
        ctx.logger.info("Manager", "Shutting down servers and subprocesses...");
        gatewayStopping = true;
        server.stop(true);
        try {
          await supervisors.shutdown();
        } finally {
          await memoryProvider.close();
        }
      })();
    }
    return shutdownPromise;
  };

  exitAfterShutdown = (status: number): Promise<number> => {
    requestedExitStatus = Math.max(requestedExitStatus, status);
    if (!exitPromise) {
      exitPromise = (async () => {
        try {
          await shutdown();
        } catch (err) {
          ctx.logger.error("Manager", "Shutdown failed", err as Error);
          requestedExitStatus = 1;
          execution.output.lifecycle({
            event: "error",
            error: {
              code: "operational_error",
              message: err instanceof Error ? err.message : String(err),
            },
          });
        } finally {
          requestedExitStatus = await finalizeGatewayShutdown(
            ctx.logger,
            async () => await gatewayLease.release(),
            requestedExitStatus,
          );
        }
        execution.output.lifecycle({
          event: "stopped",
          exitCode: requestedExitStatus,
        });
        resolveServeExit(requestedExitStatus);
        return requestedExitStatus;
      })();
    }
    return exitPromise;
  };

  // SIGKILL cannot run cleanup: POSIX does not let a process handle its own SIGKILL.
  process.once("SIGINT", () => void exitAfterShutdown?.(0));
  process.once("SIGTERM", () => void exitAfterShutdown?.(0));
  process.once("SIGHUP", () => void exitAfterShutdown?.(0));

  const exitCode = await serveExit;
  return { data: { exitCode }, exitCode };
}
