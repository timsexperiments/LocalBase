import { acknowledgeStaticConfiguration } from "../../config/activation";
import { z } from "zod";
import { SpanStatusCode, context, trace } from "@opentelemetry/api";
import { join, basename, resolve } from "node:path";
import { validateJpegImage, validatePngImage } from "../image-validation";
import { resolveApiKey, type LocalBaseConfig } from "../../../manager";
import {
  authorize,
  permissionSchema,
  principalOwnerId,
  principalSchema,
  type AuthorizationRequirement,
  type Principal,
} from "../../auth/authorization";
import { resolveAccessControl } from "../../auth/access-control";
import { createDatabaseBrowserSessionStore } from "../../auth/browser-sessions";
import {
  clearMagicLinks,
  createMagicLinkService,
} from "../../auth/magic-links";
import { loadEmailDeliveryConfig } from "../../auth/email-delivery";
import {
  byId,
  IMAGE_LORA_MAX_STRENGTH,
  CATALOG,
  calculateMaxSafeContextSize,
  isDecisionModel,
  primaryArtifact,
  resolveCatalogInstallation,
  speechVoiceSchema,
  ttsVoiceAllowed,
  modelKindSchema,
  type ModelSpec,
} from "../../../catalog";
import {
  metadataCatalogRow,
  parseCatalogQuery,
  queryCatalog,
} from "../../../catalog/catalog-query";
import type { Capability } from "../../../catalog/capabilities";
import {
  collectModelMemoryInputs,
  evaluateModelFit,
  type ModelMemoryInputs,
} from "../../models/memory-fit";
import {
  createObservedMemoryStore,
  observationTarget,
  observePeakMemory,
  platformMemorySampler,
} from "../observed-memory";
import {
  directoryForModel,
  inspectCatalogInstallations,
  modelMetadataById,
  projectModelMetadataList,
} from "../../models/model-metadata";
import { listServedModels } from "../../models/served-models";
import {
  resolveCapabilitiesForModel,
  resolveCatalogCapabilities,
  runtimeCapabilityKey,
} from "../../models/model-capabilities";
import {
  createRuntimeCapabilityStore,
  probeLlamaServerCapabilities,
} from "../runtime-capabilities";
import {
  createLlmKvGeometryReader,
  createLlmTrainingContextLengthReader,
} from "../gguf-geometry-cache";
import type { AppContext } from "../../../context";
import { activateContextOtel } from "../../../context";
import { runtimeProcessSettings } from "../config-snapshot";
import {
  configuredLlmContextSize,
  recommendedLlmContextSize,
} from "../launch-plan";
import {
  logHttpMetadataSchema,
  redactLogAttributes,
  type HttpRequestLogInput,
  type ILogger,
} from "../../observability/logging";
import {
  InferenceTelemetry,
  recordStructuredOutputPreparation,
  type InferenceMetadata,
  type InferenceOutcome,
  type InferenceTerminal,
  type InferenceTerminalSource,
  type StructuredOutputValidationTelemetry,
} from "../../observability/inference";
import {
  isPlainTextTranscriptionFormat,
  normalizePlainTranscription,
  normalizeTranscriptionJson,
  type TranscriptionResponseFormat,
} from "../transcript";
import { modalityComponents, type RuntimeModality } from "../modality";
import {
  RuntimeReconciler,
  RuntimeRequestAbortedError,
  type ModelAdmissionResult,
  type RuntimeAdmission,
} from "../runtime-reconciler";
import { type RuntimeOverrideOwnership } from "../reconciliation-plan";
import {
  createRuntimeSupervisorFactory,
  installSelectedModel,
  runtimeLaunchOverrides,
} from "../supervisor-factory";
import {
  MemorySafetyController,
  RuntimeMemoryAdmissionError,
} from "../memory-controller";
import { createHostMemoryProvider } from "../memory/host-memory-provider";
import { MemoryPressureMonitor } from "../memory-pressure-monitor";
import type { MemorySafetyTransition } from "../memory-safety";
import {
  InferenceQueueCapacityError,
  InferenceQueueTimeoutError,
  InferenceQueueUnavailableError,
} from "../inference-queue";
import { SupervisorRegistry } from "../supervisor-registry";
import {
  SPEECH_MAX_INPUT_CHARACTERS,
  SpeechGenerationAbortedError,
  SpeechGenerationTimeoutError,
  SpeechOutputError,
  bufferedSpeechStream,
  speechPcmFromWav,
  speechWavFromPcm,
  type SpeechAudioStream,
} from "../speech-supervisor";
import { composeGatewayHealth } from "../gateway-health";
import { composeGatewayReadiness } from "../readiness";
import { playgroundResponse } from "../../../ui/static";
import { createModelManagement } from "../../models/model-management";
import {
  discoveryRequestSchema,
  hubTokenFrom,
  runDiscoveryRequest,
} from "../../models/discovery";
import {
  hubTokenRequestSchema,
  hubTokenStatus,
  runHubTokenRequest,
} from "../../models/hub-token";
import { checkHubToken } from "../../models/huggingface";
import { videoTargetFromTopology } from "../../models/model-eligibility";
import {
  ModelManagementError,
  modelManagementRequestSchema,
} from "../../models/model-management-contract";
import {
  createUiAccess,
  isUiAccessPath,
  loadUiAccessConfig,
} from "../../../ui/access";
import {
  canonicalGatewayHttpRoute,
  gatewayRequestDisablesIdleTimeout,
  gatewayRouteServesBufferedJson,
  modelMetadataIdFromPath,
  servedModelIdFromPath,
  selectGatewayRoute,
  unmatchedRouteHint,
} from "../route-dispatch";
import { gatewayAuthorizationRequirement } from "../route-authorization";
import { VideoJobManager } from "../video/video-job-manager";
import { createVideoArtifactPreparer } from "../video/video-converter";
import { ensureVideoConverter } from "../../../manager/video-converter";
import { logVideoJobTerminal } from "../video/video-job-logging";
import { createStableDiffusionVideoClient } from "../video/stable-diffusion-video-client";
import { handleVideoGatewayRequest } from "../video/gateway-handler";
import { videoCreateRequestSchema } from "../video/gateway-contract";
import {
  acquireGatewayLease,
  acquireGatewayLeaseForServe,
  getGatewayInstanceStateAtRoot,
  RootOwnershipError,
} from "../../service/ownership";
import type { CommandExecution } from "../../app/commands/framework";
import type { ServeInput } from "../../app/commands/inputs";
import { CliInputError } from "../../app/commands/errors";
import {
  clientSpanOptions,
  internalSpanOptions,
  serverSpanName,
  serverSpanOptions,
  spanCorrelation,
  type OtelRuntime,
} from "../../observability/otel";
import { gatewayIdentitySchema } from "../health";
import {
  assertServePortsAvailable,
  assertModelInstallConsent,
  installMissingModel,
  ModelInstallConsentError,
} from "../startup-preflight";
import { createAuthManagement } from "../../auth/management-http";
import {
  openAIErrorResponseSchema,
  upstreamOpenAIErrorResponseSchema,
  type OpenAIError,
} from "../openai-error";
import {
  chatResponseFormatSchema,
  jsonSchemaValueSchema,
  prepareStructuredOutput,
  validateCompletedStructuredOutput,
  type CompletedStructuredOutputValidation,
  type StructuredOutputErrorCode,
  type StructuredOutputValidator,
} from "../structured-output";
import { anchorJsonSchemaPatterns } from "../tool-schema";
import { startCatalogFeedScheduler } from "../../../catalog/feed-scheduler";
import { withLifecycleSignal } from "../lifecycle-request";
import { compressJsonResponse } from "../../../utils/http-compression";

type AuthMode = "bearer" | "x-api-key" | "either";

type ModalityState = Record<RuntimeModality, boolean>;
function serveModalityState(
  config: LocalBaseConfig,
  input: ServeInput,
): ModalityState {
  return {
    llm: input.llm ?? true,
    stt: input.stt ?? config.selectedSttModels.length > 0,
    tts: input.tts ?? config.selectedTtsModels.length > 0,
    image: input.image ?? config.selectedImageModels.length > 0,
    video: input.video ?? config.selectedVideoModels.length > 0,
  };
}
type AdmittedModel = Extract<
  ModelAdmissionResult,
  { kind: "admitted" }
>["value"];
type InferenceTelemetryOptions = Pick<
  ConstructorParameters<typeof InferenceTelemetry>[0],
  "streaming" | "structuredOutput"
>;

export function httpBaseUrl(host: string, port: number): string {
  const urlHost =
    host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  return `http://${urlHost}:${port}`;
}

const MAX_REQUEST_BYTES = 25 * 1024 * 1024;
// Oversized bodies up to this many bytes past the limit are read and discarded before
// the 413 is sent, so a keep-alive client that is still writing reads the 413 at a request
// boundary instead of hitting a reset. Bun 1.3 ignored a handler's `connection: close` and
// could reuse the desynced socket (a bare 400 on the next request); Bun 1.4.2 closes it, but
// the drain still protects clients that keep streaming.
const MAX_DISCARDED_REQUEST_BYTES = 1024 * 1024;
// Job owner when gateway auth is disabled and requests carry no principal.
const LOCAL_VIDEO_OWNER_ID = "anonymous:local";

type GatewayRequestAuth = Readonly<{
  resolve(config: LocalBaseConfig): Principal;
  resolveUi(request: Request): Principal;
  telemetry(required: boolean): NonNullable<HttpRequestLogInput["auth"]>;
}>;

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
  if (enabled.tts)
    output.info(`OpenAI-compatible TTS endpoint: ${baseUrl}/v1/audio/speech`);
  if (enabled.image)
    output.info(
      `OpenAI-compatible Image endpoint: ${baseUrl}/v1/images/generations`,
    );
  if (enabled.image)
    output.info(`Image edits endpoint: ${baseUrl}/v1/images/edits`);
  if (enabled.tts) {
    output.info("\nExample TTS request (Bearer):");
    output.info(
      `curl ${baseUrl}/v1/audio/speech -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"Hello from LocalBase.","voice":"default","response_format":"wav","speed":1}' --output speech.wav`,
    );
  }
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

function isBrowserRequest(request: Request): boolean {
  return request.headers.has("origin") || request.headers.has("sec-fetch-site");
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

function forbidden(): Response {
  return openAIErrorResponse(
    {
      message: "Forbidden: The credential lacks the required permission.",
      type: "invalid_request_error",
      param: null,
      code: "insufficient_permissions",
    },
    403,
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

function badRequest(message: string, param: string | null = null): Response {
  return openAIErrorResponse(
    {
      message,
      type: "invalid_request_error",
      param,
      code: "validation_failed",
    },
    400,
  );
}

function structuredOutputRequestFailure(
  message: string,
  code: StructuredOutputErrorCode,
): Response {
  return openAIErrorResponse(
    {
      message,
      type: "invalid_request_error",
      param: "response_format.json_schema.schema",
      code,
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

function payloadTooLarge(limit = MAX_REQUEST_BYTES): Response {
  return openAIErrorResponse(
    {
      message: `Request body exceeds the ${limit / (1024 * 1024)} MiB limit.`,
      type: "invalid_request_error",
      param: null,
      code: "payload_too_large",
    },
    413,
    // The rest of the body may still be in flight, so do not reuse this connection.
    { connection: "close" },
  );
}

function upstreamError(message: string): OpenAIError {
  return openAIErrorResponseSchema.shape.error.parse({
    message,
    type: "server_error",
    param: null,
    code: "upstream_error",
  });
}

function upstreamFailure(message: string): Response {
  return openAIErrorResponse(upstreamError(message), 502);
}

function upstreamErrorEvent(message: string): Uint8Array {
  const error = openAIErrorResponseSchema.parse({
    error: upstreamError(message),
  });
  return new TextEncoder().encode(`data: ${JSON.stringify(error)}\n\n`);
}

/** Whisper reports failures as JSON bodies; successful text formats are never JSON-typed. */
function looksLikeBackendError(upstream: Response, body: string): boolean {
  if (!upstream.headers.get("content-type")?.includes("json")) return false;
  const trimmed = body.trimStart();
  if (!trimmed.startsWith("{")) return false;
  try {
    const parsed: unknown = JSON.parse(trimmed);
    return (
      typeof parsed === "object" &&
      parsed !== null &&
      !Array.isArray(parsed) &&
      "error" in parsed
    );
  } catch {
    return false;
  }
}

function speechTimeout(): Response {
  return openAIErrorResponse(
    {
      message: "Speech generation exceeded the 120 second limit.",
      type: "server_error",
      param: null,
      code: "speech_timeout",
    },
    504,
  );
}

function modelInstallConsentResponse(
  error: ModelInstallConsentError,
): Response {
  return openAIErrorResponse(
    {
      message: error.message,
      type: "invalid_request_error",
      param: "model",
      code: "model_install_consent_required",
    },
    409,
  );
}

export function speechGenerationFailure(error: unknown): Readonly<{
  response: Response;
  source?: InferenceTerminalSource;
}> {
  if (error instanceof ModelInstallConsentError) {
    return {
      response: modelInstallConsentResponse(error),
    };
  }
  if (error instanceof RuntimeMemoryAdmissionError) {
    return {
      response: resourceUnavailable(error),
      source: "memory_admission",
    };
  }
  if (error instanceof SpeechGenerationTimeoutError) {
    return { response: speechTimeout(), source: "speech_timeout" };
  }
  if (error instanceof SpeechOutputError) {
    return { response: upstreamFailure(error.message) };
  }
  return { response: serviceUnavailable("TTS") };
}

function structuredOutputValidationFailure(): Response {
  return openAIErrorResponse(
    {
      message:
        "The backend returned a completed response that did not match the requested JSON Schema.",
      type: "server_error",
      param: null,
      code: "structured_output_validation_failed",
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
  const param = error.issues.some(
    (issue) => issue.path[0] === "response_format",
  )
    ? "response_format"
    : null;
  return badRequest(`Validation failed: ${issues}`, param);
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

const namedToolChoiceSchema = z
  .object({
    type: z.literal("function"),
    function: z.object({ name: z.string().min(1) }).strict(),
  })
  .strict();

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
    reasoning: z.string().nullable().optional(),
    reasoning_content: z.string().nullable().optional(),
    function_call: z.never().optional(),
  })
  .refine(
    (message) =>
      (message.content !== undefined && message.content !== null) ||
      (message.tool_calls?.length ?? 0) > 0 ||
      (message.refusal !== undefined && message.refusal !== null) ||
      (message.reasoning !== undefined && message.reasoning !== null) ||
      (message.reasoning_content !== undefined &&
        message.reasoning_content !== null),
    "assistant messages require content, reasoning, tool_calls, or refusal",
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
    max_completion_tokens: z.number().positive().optional(),
    presence_penalty: z.number().min(-2).max(2).optional(),
    frequency_penalty: z.number().min(-2).max(2).optional(),
    logit_bias: z.record(z.string(), z.number()).nullable().optional(),
    user: z.string().optional(),
    response_format: chatResponseFormatSchema.optional(),
    tools: z.array(functionToolSchema).optional(),
    tool_choice: z
      .union([z.enum(["none", "auto", "required"]), namedToolChoiceSchema])
      .optional(),
  })
  .passthrough()
  .superRefine((request, context) => {
    const toolChoice = request.tool_choice;
    if (!toolChoice || typeof toolChoice !== "object") return;
    const matchingTools =
      request.tools?.filter(
        (tool) => tool.function.name === toolChoice.function.name,
      ) ?? [];
    if (matchingTools.length === 1) return;
    context.addIssue({
      code: "custom",
      path: ["tool_choice", "function", "name"],
      message:
        "tool_choice.function.name must name exactly one declared function tool.",
    });
  });

type ChatCompletionRequest = z.output<typeof chatCompletionRequestSchema>;

function stripNoopToolFields(
  request: ChatCompletionRequest,
): ChatCompletionRequest {
  const { tools, functions, tool_choice, function_call, messages, ...rest } =
    request;
  return {
    ...rest,
    ...(tools?.length ? { tools } : {}),
    ...(functions !== undefined &&
    !(Array.isArray(functions) && functions.length === 0)
      ? { functions }
      : {}),
    ...(tool_choice && tool_choice !== "none" ? { tool_choice } : {}),
    ...(function_call !== undefined && function_call !== "none"
      ? { function_call }
      : {}),
    messages: messages.map((message) => {
      if (message.role !== "assistant" || message.tool_calls?.length) {
        return message;
      }
      const { tool_calls: _emptyToolCalls, ...assistantMessage } = message;
      return assistantMessage;
    }),
  } as ChatCompletionRequest;
}

function anchorToolSchemas(
  request: ChatCompletionRequest,
): ChatCompletionRequest {
  if (!request.tools?.length) return request;
  return {
    ...request,
    tools: request.tools.map((tool) =>
      tool.function.parameters
        ? {
            ...tool,
            function: {
              ...tool.function,
              parameters: anchorJsonSchemaPatterns(
                tool.function.parameters,
              ) as NonNullable<typeof tool.function.parameters>,
            },
          }
        : tool,
    ),
  };
}

function prepareChatCompletionRequest(
  request: ChatCompletionRequest,
): ChatCompletionRequest {
  const anchored = anchorToolSchemas(request);
  const toolChoice = anchored.tool_choice;
  if (!toolChoice || typeof toolChoice !== "object") return anchored;

  const selectedTools =
    anchored.tools?.filter(
      (tool) => tool.function.name === toolChoice.function.name,
    ) ?? [];

  return {
    ...anchored,
    tools: selectedTools,
    tool_choice: "required",
  };
}

export const IMAGE_SIZES = [
  "256x256",
  "512x512",
  "1024x1024",
  "512x768",
  "768x512",
  "768x1024",
  "1024x768",
] as const;

// Tuning fields stable-diffusion.cpp reads from an inline sd_cpp_extra_args block.
const imageTuningSchema = z.object({
  negative_prompt: z
    .string()
    .max(16_384)
    .refine(
      (value) => !value.toLowerCase().includes("sd_cpp_extra_args"),
      "negative_prompt must not contain sd_cpp_extra_args.",
    )
    .optional(),
  seed: z.number().int().min(0).max(2_147_483_647).optional(),
  steps: z.number().int().min(1).max(150).optional(),
  cfg_scale: z.number().min(0).max(30).optional(),
  strength: z.number().min(0).max(1).optional(),
  lora_strength: z.number().min(0).max(IMAGE_LORA_MAX_STRENGTH).optional(),
});
type ImageTuning = z.infer<typeof imageTuningSchema>;

/** A LoRA file sd-server applies to one request, at an absolute path. */
export type ImageLoraApplication = Readonly<{
  path: string;
  multiplier: number;
}>;

/**
 * The LoRA a request applies to an image model: the requested strength or the
 * catalog default, none at strength 0. sd-server's OpenAI routes accept LoRAs
 * only as structured extra args and resolve paths against its own cwd, so the
 * path is absolute.
 */
export function imageLoraFor(
  spec: ModelSpec | undefined,
  modelsDirectory: string,
  strength: number | undefined,
): ImageLoraApplication | undefined {
  const lora = spec?.imageLaunchOptions?.lora;
  if (!lora) return undefined;
  const multiplier = strength ?? lora.defaultStrength;
  return multiplier > 0
    ? { path: resolve(modelsDirectory, lora.artifactFilename), multiplier }
    : undefined;
}

/** The 400 for a lora_strength sent to a model without a LoRA, if any. */
function unusableLoraStrength(
  spec: ModelSpec | undefined,
  strength: number | undefined,
): Response | undefined {
  if (
    strength === undefined ||
    spec?.kind !== "image" ||
    spec.imageLaunchOptions?.lora
  )
    return undefined;
  return badRequest(
    `Model '${spec.modelId}' has no LoRA; lora_strength applies only to image models added with one.`,
    "lora_strength",
  );
}

export function promptWithImageTuning(
  prompt: string,
  tuning: Omit<ImageTuning, "lora_strength">,
  lora?: ImageLoraApplication,
) {
  const extra: Record<string, unknown> = {};
  if (lora)
    extra.lora = [
      { path: lora.path, multiplier: lora.multiplier, is_high_noise: false },
    ];
  if (tuning.negative_prompt) extra.negative_prompt = tuning.negative_prompt;
  if (tuning.seed !== undefined) extra.seed = tuning.seed;
  if (tuning.strength !== undefined) extra.strength = tuning.strength;
  const sample: Record<string, unknown> = {};
  if (tuning.steps !== undefined) sample.sample_steps = tuning.steps;
  if (tuning.cfg_scale !== undefined)
    sample.guidance = { txt_cfg: tuning.cfg_scale };
  if (Object.keys(sample).length) extra.sample_params = sample;
  return Object.keys(extra).length
    ? `${prompt} <sd_cpp_extra_args>${JSON.stringify(extra)}</sd_cpp_extra_args>`
    : prompt;
}

const imageGenerationRequestSchema = imageTuningSchema
  .omit({ strength: true })
  .extend({
    prompt: z
      .string()
      .refine(
        (value) => !value.toLowerCase().includes("sd_cpp_extra_args"),
        "prompt must not contain sd_cpp_extra_args.",
      ),
    model: modelIdSchema.optional(),
    n: z.number().min(1).max(10).optional(),
    quality: z.enum(["standard", "hd"]).optional(),
    response_format: z.enum(["url", "b64_json"]).optional(),
    size: z.enum(IMAGE_SIZES).optional(),
    style: z.enum(["vivid", "natural"]).optional(),
    user: z.string().optional(),
  })
  .passthrough();

const speechGenerationRequestSchema = z
  .object({
    model: modelIdSchema,
    input: z
      .string()
      .refine(
        (value) => Array.from(value).length > 0,
        "input must not be empty",
      )
      .refine(
        (value) => Array.from(value).length <= SPEECH_MAX_INPUT_CHARACTERS,
        `input must not exceed ${SPEECH_MAX_INPUT_CHARACTERS} characters`,
      ),
    voice: speechVoiceSchema,
    response_format: z
      .enum(["wav", "pcm"], {
        error: "response_format supports only 'wav' or 'pcm'",
      })
      .default("wav"),
    stream_format: z
      .enum(["audio", "sse"], {
        error: "stream_format supports only 'audio' or 'sse'",
      })
      .optional(),
    speed: z.literal(1, { error: "speed must be 1" }).optional(),
    instructions: z
      .never({ error: "instructions are not supported" })
      .optional(),
  })
  .strict();

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

const decisionInstructionsSchema = z.union([
  z.string(),
  z.record(z.string(), z.unknown()),
  z.array(z.unknown()),
]);

/** Up to 255 options: the most any llama.cpp decision architecture accepts. */
const MAX_DECISION_CHOICE_OPTIONS = 255;

const decisionQuestionSchema = z.discriminatedUnion("type", [
  z
    .object({
      type: z.literal("choice"),
      instructions: decisionInstructionsSchema.optional(),
      criteria: z
        .record(z.string().min(1), z.string().nullable())
        .refine((criteria) => Object.keys(criteria).length >= 2, {
          message: "A choice question needs at least 2 options.",
        })
        .refine(
          (criteria) =>
            Object.keys(criteria).length <= MAX_DECISION_CHOICE_OPTIONS,
          {
            message: `A choice question accepts at most ${MAX_DECISION_CHOICE_OPTIONS} options.`,
          },
        ),
    })
    .strict(),
  z
    .object({
      type: z.literal("score"),
      instructions: decisionInstructionsSchema.optional(),
      criteria: z.array(z.string()).min(2).max(10),
    })
    .strict(),
  z
    .object({
      type: z.literal("noul"),
      instructions: decisionInstructionsSchema.optional(),
      criteria: z
        .object({ true: z.string().optional(), false: z.string().optional() })
        .strict()
        .optional(),
    })
    .strict(),
]);

const decisionFileSchema = z
  .string()
  .regex(
    /^data:(image|audio)\/[a-z0-9.+-]+;base64,/i,
    "Each file must be a base64 data URL (data:image/... or data:audio/...).",
  );

/**
 * POST /v1/systemone: the TypeSafe System One request llama-server answers
 * with a decision model. `model` is required so the gateway can admit the
 * right runtime.
 */
const decisionRequestSchema = z
  .object({
    model: modelIdSchema,
    state: z
      .union([
        z.string(),
        z.record(z.string(), z.unknown()),
        z.array(z.unknown()),
        z.null(),
      ])
      .optional()
      .refine((state) => state !== undefined, {
        message: "state is required.",
      }),
    files: z.array(decisionFileSchema).optional(),
    images: z.array(decisionFileSchema).optional(),
    questions: z
      .record(z.string().min(1), decisionQuestionSchema)
      .refine((questions) => Object.keys(questions).length > 0, {
        message: "questions must contain at least one question.",
      }),
  })
  .strict();

/** Fills a missing or empty `instructions` with the question id. */
function prepareDecisionRequest(
  request: z.output<typeof decisionRequestSchema>,
): z.output<typeof decisionRequestSchema> {
  const questions = Object.fromEntries(
    Object.entries(request.questions).map(([id, question]) => [
      id,
      question.instructions === undefined ||
      (typeof question.instructions === "string" &&
        question.instructions.trim().length === 0)
        ? { ...question, instructions: id }
        : question,
    ]),
  );
  return { ...request, questions };
}

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

function normalizeReasoningContent<
  T extends {
    reasoning?: string | null;
    reasoning_content?: string | null;
  },
>(source: T): Omit<T, "reasoning_content"> & { reasoning?: string | null } {
  const { reasoning_content, ...normalized } = source;
  return reasoning_content == null || normalized.reasoning !== undefined
    ? normalized
    : { ...normalized, reasoning: reasoning_content };
}

const chatCompletionStreamDeltaSchema = z
  .object({
    role: z.literal("assistant").optional(),
    content: z.string().nullable().optional(),
    refusal: z.string().nullable().optional(),
    reasoning: z.string().nullable().optional(),
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

const decisionProbabilitiesSchema = z.record(z.string(), z.number());

const decisionResponseSchema = z
  .object({
    model: z.string(),
    answers: z.record(
      z.string(),
      z.discriminatedUnion("type", [
        z
          .object({
            type: z.literal("choice"),
            choice: z.string(),
            probabilities: decisionProbabilitiesSchema,
            confidence: z.number(),
          })
          .passthrough(),
        z
          .object({
            type: z.literal("score"),
            score: z.number(),
            legend: z.record(z.string(), z.string()),
            probabilities: decisionProbabilitiesSchema,
            confidence: z.number(),
          })
          .passthrough(),
        z.object({ type: z.literal("noul"), noul: z.number() }).passthrough(),
      ]),
    ),
    usage: z
      .object({ input_tokens: z.number(), output_tokens: z.number() })
      .passthrough(),
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
            seek: z.number().optional(),
            start: z.number().optional(),
            end: z.number().optional(),
            text: z.string(),
            speaker: z.string().optional(),
            tokens: z.array(z.number()).optional(),
            words: z
              .array(z.object({ word: z.string() }).passthrough())
              .optional(),
            // whisper.cpp serializes NaN (e.g. avg_logprob with no tokens) as null.
            temperature: z.number().nullable().optional(),
            avg_logprob: z.number().nullable().optional(),
            compression_ratio: z.number().nullable().optional(),
            no_speech_prob: z.number().nullable().optional(),
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

function requestExceedsSizeLimit(
  request: Request,
  limit = MAX_REQUEST_BYTES,
): boolean {
  const contentLength = request.headers.get("content-length");
  if (!contentLength) return false;
  const size = Number(contentLength);
  return !Number.isSafeInteger(size) || size < 0 || size > limit;
}

function imageMimeFromMagic(bytes: Uint8Array): string | undefined {
  if (
    bytes.length >= 8 &&
    [137, 80, 78, 71, 13, 10, 26, 10].every(
      (value, index) => bytes[index] === value,
    )
  )
    return "image/png";
  if (
    bytes.length >= 3 &&
    bytes[0] === 0xff &&
    bytes[1] === 0xd8 &&
    bytes[2] === 0xff
  )
    return "image/jpeg";
  return undefined;
}

function requestBodyBuffer(bytes: Uint8Array): ArrayBuffer {
  const buffer = new ArrayBuffer(bytes.byteLength);
  new Uint8Array(buffer).set(bytes);
  return buffer;
}

// Drains a body whose content-length is over `limit` when the excess is small enough.
async function discardDeclaredOversizedBody(
  request: Request,
  limit: number,
): Promise<void> {
  const declared = Number(request.headers.get("content-length"));
  if (!Number.isSafeInteger(declared)) return;
  if (declared > limit + MAX_DISCARDED_REQUEST_BYTES) return;
  // Read the original stream: the request is rejected, so nothing else consumes it.
  const reader = request.body?.getReader();
  if (reader) await discardRemainingBody(reader, declared);
}

// Reads and drops up to `budget` more body bytes so an oversized request finishes
// arriving before its 413 is sent; gives up (cancelling the stream) past the budget.
async function discardRemainingBody(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  budget: number,
): Promise<void> {
  let discarded = 0;
  try {
    while (discarded <= budget) {
      const { done, value } = await reader.read();
      if (done) return;
      discarded += value.byteLength;
    }
    await reader.cancel();
  } catch {
    // The client went away or the stream failed; the 413 is still the right answer.
  }
}

async function readBoundedRequestBody(
  request: Request,
  limit = MAX_REQUEST_BYTES,
): Promise<Uint8Array> {
  if (requestExceedsSizeLimit(request, limit)) {
    await discardDeclaredOversizedBody(request, limit);
    throw new PayloadTooLargeError();
  }
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
      if (size > limit) {
        await discardRemainingBody(reader, MAX_DISCARDED_REQUEST_BYTES);
        throw new PayloadTooLargeError();
      }
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
    if (request.signal.aborted) {
      return { success: false, response: requestAborted() };
    }
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
    "cookie",
    "cf-access-jwt-assertion",
    "cf-access-authenticated-user-email",
    "cf-access-client-id",
    "cf-access-client-secret",
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

function withCanonicalModel(
  event: string,
  value: z.output<typeof chatCompletionStreamChunkSchema>,
  modelId: string,
): string {
  const newline = event.includes("\r\n")
    ? "\r\n"
    : event.includes("\r")
      ? "\r"
      : "\n";
  let replaced = false;
  return event
    .split(/\r\n|\r|\n/)
    .flatMap((line) => {
      if (!line.startsWith("data:")) return [line];
      if (replaced) return [];
      replaced = true;
      return [`data: ${JSON.stringify({ ...value, model: modelId })}`];
    })
    .join(newline);
}

function validateEventStream(
  body: ReadableStream<Uint8Array>,
  schema: z.ZodType<z.output<typeof chatCompletionStreamEventSchema>>,
  onValidatedEvent?: (
    value: z.output<typeof chatCompletionStreamEventSchema>,
  ) => void,
  onInvalidEvent?: () => void,
  canonicalModelId?: string,
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
  })}\n\n`;

  const fail = (
    controller: TransformStreamDefaultController<Uint8Array>,
    terminate: boolean,
  ): false => {
    if (!failed) {
      onInvalidEvent?.();
      controller.enqueue(encoder.encode(validationFailure));
    }
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
        const value =
          "error" in parsed.data
            ? parsed.data
            : {
                ...parsed.data,
                choices: parsed.data.choices.map((choice) => ({
                  ...choice,
                  delta: normalizeReasoningContent(choice.delta),
                })),
              };
        onValidatedEvent?.(value);
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
        if (canonicalModelId) {
          event = withCanonicalModel(event, value, canonicalModelId);
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
        // An SSE event is only complete after its blank-line delimiter.
        const hasUnframedEvent = buffered.length > 0;
        buffered = "";
        if (failed) return;
        if (hasUnframedEvent || !doneEvent) {
          fail(controller, false);
          return;
        }
        controller.enqueue(encoder.encode(doneEvent));
      },
    }),
  );
}

export function recoverEventStreamReadErrors(
  body: ReadableStream<Uint8Array>,
  onError?: () => void,
): ReadableStream<Uint8Array> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  const encoder = new TextEncoder();
  let buffered = "";
  let failed = false;
  let released = false;
  const release = () => {
    if (released) return;
    released = true;
    reader.releaseLock();
  };
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (failed) return;
      try {
        while (true) {
          const result = await reader.read();
          if (result.done) {
            buffered += decoder.decode();
            if (buffered) {
              buffered = "";
              onError?.();
              controller.enqueue(
                upstreamErrorEvent("The upstream stream ended mid-event."),
              );
            }
            controller.close();
            release();
            return;
          }
          buffered += decoder.decode(result.value, { stream: true });
          const boundaries = [
            ...buffered.matchAll(/(?:\r\n|\r|\n)(?:\r\n|\r|\n)/g),
          ];
          const last = boundaries.at(-1);
          if (last?.index !== undefined) {
            const end = last.index + last[0].length;
            controller.enqueue(encoder.encode(buffered.slice(0, end)));
            buffered = buffered.slice(end);
            return;
          }
        }
      } catch {
        failed = true;
        onError?.();
        buffered = "";
        controller.enqueue(upstreamErrorEvent("The upstream stream failed."));
        controller.close();
        release();
      }
    },
    async cancel(reason) {
      try {
        await reader.cancel(reason);
      } finally {
        release();
      }
    },
  });
}

function inferenceMetadata(
  modality: RuntimeModality,
  modelId: string,
  admission: InferenceMetadata["admission"],
): InferenceMetadata {
  const model = byId(modelId);
  if (!model) {
    return {
      modelId,
      modality,
      runtimeName: modalityComponents[modality],
      admission,
    };
  }
  const artifact = primaryArtifact(model);
  return {
    modelId,
    modality,
    runtimeName: modalityComponents[modality],
    catalog: {
      artifactRevision: artifact.source?.revision ?? model.repositoryRevision,
      quantization: model.quant,
    },
    admission,
  };
}

function withServerTiming(response: Response, timing: string | undefined) {
  if (!timing) return response;
  const headers = new Headers(response.headers);
  headers.set("server-timing", timing);
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

async function proxyRequest(
  request: Request,
  targetBase: string,
  pathOverride?: string,
  responseSchema?: z.ZodType,
  eventStreamSchema?: z.ZodType<
    z.output<typeof chatCompletionStreamEventSchema>
  >,
  otel?: OtelRuntime,
  onValidatedEvent?: (value: unknown, status: number) => void,
  onInvalidEvent?: (status: number) => void,
  structuredOutput?: Readonly<{
    validator: StructuredOutputValidator;
    onValidation: (result: CompletedStructuredOutputValidation) => void;
  }>,
  canonicalModelId?: string,
  onUpstreamStatus?: (status: number) => void,
  parentContext?: Parameters<OtelRuntime["startSpan"]>[2],
  transcriptionFormat?: TranscriptionResponseFormat,
): Promise<Response> {
  const incoming = new URL(request.url);
  const path = pathOverride ?? incoming.pathname;
  const target = `${targetBase}${path}${incoming.search}`;
  let upstream: Response;
  const fetchUpstream = async (): Promise<Response> => {
    const headers = filterProxyHeaders(request.headers);
    otel?.inject(headers);
    const options = {
      method: request.method,
      headers,
      body: request.body,
      signal: request.signal,
      timeout: false,
    } satisfies BunFetchRequestInit & { timeout: false };
    return await fetch(target, options);
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
          parentContext,
        )
      : await fetchUpstream();
  } catch {
    if (request.signal.aborted) return requestAborted();
    return upstreamFailure("The upstream service could not be reached.");
  }
  onUpstreamStatus?.(upstream.status);
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
    return new Response(
      validateEventStream(
        recoverEventStreamReadErrors(upstream.body, () =>
          onInvalidEvent?.(502),
        ),
        eventStreamSchema,
        (value) => onValidatedEvent?.(value, upstream.status),
        () => onInvalidEvent?.(upstream.status),
        canonicalModelId,
      ),
      {
        status: upstream.status,
        statusText: upstream.statusText,
        headers,
      },
    );
  }

  if (
    transcriptionFormat &&
    isPlainTextTranscriptionFormat(transcriptionFormat) &&
    upstream.ok
  ) {
    try {
      const raw = await upstream.text();
      if (looksLikeBackendError(upstream, raw)) {
        onInvalidEvent?.(502);
        return upstreamFailure(
          "The upstream service returned an invalid response.",
        );
      }
      const text = normalizePlainTranscription(raw, transcriptionFormat);
      const headers = filterProxyHeaders(upstream.headers);
      headers.delete("content-length");
      headers.set("content-type", "text/plain; charset=utf-8");
      return new Response(text, { status: upstream.status, headers });
    } catch {
      if (request.signal.aborted) return requestAborted();
      onInvalidEvent?.(502);
      return upstreamFailure("The upstream service returned malformed text.");
    }
  }

  if (responseSchema && !isEventStream(upstream) && upstream.ok) {
    try {
      const parsed = responseSchema.safeParse(await upstream.json());
      if (!parsed.success) {
        onInvalidEvent?.(502);
        return upstreamFailure(
          "The upstream service returned an invalid response.",
        );
      }

      const chatResponse = chatCompletionResponseSchema.safeParse(parsed.data);
      if (structuredOutput) {
        const validation: StructuredOutputValidationTelemetry =
          chatResponse.success
            ? validateCompletedStructuredOutput(
                chatResponse.data,
                structuredOutput.validator,
              )
            : { outcome: "failed", skipReasons: [] };
        structuredOutput.onValidation(validation);
        if (validation.outcome === "failed") {
          onInvalidEvent?.(502);
          return structuredOutputValidationFailure();
        }
      }
      onValidatedEvent?.(parsed.data, upstream.status);

      const publicData = transcriptionFormat
        ? normalizeTranscriptionJson(
            parsed.data as Parameters<typeof normalizeTranscriptionJson>[0],
            transcriptionFormat,
          )
        : chatResponse.success
          ? {
              ...chatResponse.data,
              choices: chatResponse.data.choices.map((choice) => ({
                ...choice,
                message: normalizeReasoningContent(choice.message),
              })),
            }
          : parsed.data;

      const headers = filterProxyHeaders(upstream.headers);
      headers.delete("content-length");
      if (transcriptionFormat) headers.set("content-type", "application/json");
      return Response.json(
        canonicalModelId &&
          typeof publicData === "object" &&
          publicData !== null &&
          "model" in publicData
          ? { ...publicData, model: canonicalModelId }
          : publicData,
        {
          status: upstream.status,
          headers,
        },
      );
    } catch {
      if (request.signal.aborted) return requestAborted();
      onInvalidEvent?.(502);
      return upstreamFailure("The upstream service returned malformed JSON.");
    }
  }

  if (upstream.status >= 400 && upstream.status <= 599) {
    try {
      const parsed = upstreamOpenAIErrorResponseSchema.safeParse(
        await upstream.json(),
      );
      if (parsed.success) {
        return openAIErrorResponse(parsed.data.error, upstream.status);
      }
    } catch {}
  }

  return upstreamFailure(
    "The upstream service returned an invalid error response.",
  );
}

/** RIFF size fields for a stream whose final length is not yet known. */
const STREAMING_WAV_UNKNOWN_SIZE = 0xffffffff;
const SPEECH_PCM_CONTENT_TYPE =
  "audio/pcm;rate=24000;channels=1;encoding=s16le";

function streamingWavHeader(): Uint8Array {
  const header = speechWavFromPcm(new Uint8Array(0)).slice(0, 44);
  const view = new DataView(header.buffer);
  view.setUint32(4, STREAMING_WAV_UNKNOWN_SIZE, true);
  view.setUint32(40, STREAMING_WAV_UNKNOWN_SIZE, true);
  return header;
}

function base64Chunk(bytes: Uint8Array): string {
  return Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength).toString(
    "base64",
  );
}

/**
 * Encodes streamed PCM16 speech as raw audio (WAV with an open-ended header,
 * or bare PCM) or as OpenAI `speech.audio.delta` server-sent events.
 */
export function speechStreamResponse(
  audio: SpeechAudioStream,
  responseFormat: "wav" | "pcm",
  streamFormat: "audio" | "sse",
): Response {
  const encoder = new TextEncoder();
  const reader = audio.pcm.getReader();
  let headerPending = responseFormat === "wav";
  const encode = (bytes: Uint8Array): Uint8Array =>
    streamFormat === "sse"
      ? encoder.encode(
          `data: ${JSON.stringify({ type: "speech.audio.delta", audio: base64Chunk(bytes) })}\n\n`,
        )
      : bytes;
  const body = new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (headerPending) {
        headerPending = false;
        controller.enqueue(encode(streamingWavHeader()));
        return;
      }
      const { done, value } = await reader.read();
      if (done) {
        if (streamFormat === "sse") {
          controller.enqueue(
            encoder.encode(
              `data: ${JSON.stringify({ type: "speech.audio.done" })}\n\n`,
            ),
          );
        }
        controller.close();
        return;
      }
      controller.enqueue(encode(value));
    },
    async cancel(reason) {
      await reader.cancel(reason);
    },
  });
  const headers: Record<string, string> =
    streamFormat === "sse"
      ? {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        }
      : {
          "content-type":
            responseFormat === "wav" ? "audio/wav" : SPEECH_PCM_CONTENT_TYPE,
        };
  return new Response(body, { headers });
}

/** Keeps an active-model lease until the client finishes or cancels the response. */
export function withResponseLease(
  response: Response,
  release: () => void,
  cancel: () => void,
  requestSignal: AbortSignal,
  onSettled?: (terminal: InferenceTerminal) => void,
): Response {
  if (!response.body) {
    if (requestSignal.aborted) {
      cancel();
      onSettled?.({
        outcome: "cancelled",
        httpStatus: response.status,
        source: "request_aborted",
      });
    } else {
      release();
      onSettled?.({ outcome: "completed", httpStatus: response.status });
    }
    return response;
  }

  let completed = false;
  let readerReleased = false;
  let removeAbortListener = () => {};
  const settleOnce = (terminal: InferenceTerminal) => {
    if (completed) return;
    completed = true;
    removeAbortListener();
    if (terminal.outcome === "completed") release();
    else cancel();
    onSettled?.(terminal);
  };
  const reader = response.body.getReader();
  const releaseReader = () => {
    if (readerReleased) return;
    readerReleased = true;
    reader.releaseLock();
  };
  const cancelReader = async (reason?: unknown) => {
    try {
      await reader.cancel(reason);
    } finally {
      releaseReader();
    }
  };
  const cancelForRequestAbort = () => {
    settleOnce({
      outcome: "cancelled",
      httpStatus: response.status,
      source: "request_aborted",
    });
    void cancelReader(requestSignal.reason);
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
          settleOnce({ outcome: "completed", httpStatus: response.status });
          releaseReader();
          controller.close();
          return;
        }
        controller.enqueue(value);
      } catch (error) {
        settleOnce({
          outcome: "error",
          httpStatus: response.status,
          source: "response_stream_error",
        });
        releaseReader();
        controller.error(error);
      }
    },
    async cancel(reason) {
      settleOnce({
        outcome: "cancelled",
        httpStatus: response.status,
        source: "response_cancelled",
      });
      await cancelReader(reason);
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

type InferenceQueueRejection =
  | Readonly<{
      kind: "capacity";
      code: "inference_queue_full";
      status: 429;
    }>
  | Readonly<{
      kind: "timeout";
      code: "inference_queue_timeout";
      status: 429;
      queueWaitMs?: number;
    }>
  | Readonly<{
      kind: "unavailable";
      code: "service_unavailable";
      status: 503;
    }>;

function classifyInferenceQueueError(
  error: unknown,
): InferenceQueueRejection | undefined {
  if (error instanceof InferenceQueueCapacityError) {
    return { kind: "capacity", code: "inference_queue_full", status: 429 };
  }
  if (error instanceof InferenceQueueTimeoutError) {
    return {
      kind: "timeout",
      code: "inference_queue_timeout",
      status: 429,
      ...(error.queueWaitMs === undefined
        ? {}
        : { queueWaitMs: error.queueWaitMs }),
    };
  }
  if (error instanceof InferenceQueueUnavailableError) {
    return { kind: "unavailable", code: "service_unavailable", status: 503 };
  }
  return undefined;
}

function inferenceQueueErrorResponse(
  rejection: InferenceQueueRejection,
): Response {
  if (rejection.kind === "capacity" || rejection.kind === "timeout") {
    return openAIErrorResponse(
      {
        message:
          rejection.kind === "timeout"
            ? "The inference request exceeded its queue wait deadline."
            : "The inference queue is full. Please try again shortly.",
        type: "rate_limit_error",
        param: null,
        code: rejection.code,
      },
      429,
      { "Retry-After": "1" },
    );
  }
  return serviceUnavailable("Inference");
}

export function inferenceQueueError(
  error: unknown,
  telemetry?: Readonly<{
    modality: RuntimeModality;
    requestId: string;
    logger: Pick<ILogger, "event">;
    span?: Readonly<{
      setAttribute: (name: string, value: string | number | boolean) => unknown;
    }>;
  }>,
): Response | undefined {
  const rejection = classifyInferenceQueueError(error);
  if (!rejection) return undefined;
  if (telemetry) {
    telemetry.span?.setAttribute(
      "localbase.inference.admission.outcome",
      "rejected",
    );
    telemetry.span?.setAttribute(
      "localbase.inference.admission.error_code",
      rejection.code,
    );
    telemetry.span?.setAttribute(
      "localbase.inference.admission.source",
      rejection.kind,
    );
    if (rejection.kind === "timeout" && rejection.queueWaitMs !== undefined) {
      telemetry.span?.setAttribute(
        "localbase.inference.queue.duration_ms",
        rejection.queueWaitMs,
      );
    }
    telemetry.logger.event({
      severity: "warn",
      eventName: "inference.admission-rejected",
      category: "runtime",
      component: "inference",
      runtime: telemetry.modality,
      message: "Inference admission was rejected.",
      requestId: telemetry.requestId,
      attributes: {
        modality: telemetry.modality,
        error_code: rejection.code,
        source: rejection.kind,
        http_status: rejection.status,
        ...(rejection.kind === "timeout" && rejection.queueWaitMs !== undefined
          ? { queue_wait_ms: Number(rejection.queueWaitMs.toFixed(2)) }
          : {}),
      },
    });
  }
  return inferenceQueueErrorResponse(rejection);
}

const gibibytes = (bytes: number): string =>
  `${(bytes / 1024 ** 3).toFixed(1)} GiB`;

/**
 * Maps a memory admission failure to a response. Transient shortages keep the
 * retryable 503; a model that exceeds the host's usable capacity can never be
 * admitted, so it gets a non-retryable 422 without Retry-After.
 */
export function resourceUnavailable(error?: unknown): Response {
  const capacity =
    error instanceof RuntimeMemoryAdmissionError ? error.capacity : undefined;
  if (capacity) {
    return openAIErrorResponse(
      {
        message:
          `This model requires about ${gibibytes(capacity.requiredBytes)} of memory, ` +
          `but only ${gibibytes(capacity.usableBytes)} is available for models on this host ` +
          `(${gibibytes(capacity.capacityBytes)} total, after the safety reserve). ` +
          "It cannot be loaded here even when nothing else is running. " +
          "Use a smaller quantization or a smaller model.",
        type: "invalid_request_error",
        param: "model",
        code: "model_too_large",
      },
      422,
    );
  }
  const gpuWorkingSet =
    error instanceof RuntimeMemoryAdmissionError
      ? error.gpuWorkingSet
      : undefined;
  return openAIErrorResponse(
    {
      message: gpuWorkingSet
        ? `This model needs about ${gibibytes(gpuWorkingSet.requiredBytes)} of GPU memory, ` +
          `but models already loaded use ${gibibytes(gpuWorkingSet.residentBytes)} ` +
          `of the ${gibibytes(gpuWorkingSet.capBytes)} GPU working-set limit on this host. ` +
          "Retry once another model is idle or unloaded."
        : "Insufficient available memory to start the requested runtime. Please try again shortly.",
      type: "api_error",
      param: null,
      code: "insufficient_memory",
    },
    503,
    { "Retry-After": "5" },
  );
}

export async function proxyWithAdmission(
  admission: RuntimeAdmission,
  serviceName: string,
  requestSignal: AbortSignal,
  dispatch: () => Promise<Response>,
  onSettled?: (terminal: InferenceTerminal) => void,
): Promise<Response> {
  try {
    await waitForRequestAbort(admission.ready, requestSignal);
    await Promise.resolve();
    if (requestSignal.aborted) throw new RequestAbortedError();
    const response = await waitForRequestAbort(dispatch(), requestSignal);
    admission.markResponseStarted();
    return withResponseLease(
      response,
      admission.release,
      admission.cancel,
      requestSignal,
      (terminal) =>
        onSettled?.(
          terminal.outcome === "completed" && response.status >= 400
            ? { outcome: "error", httpStatus: response.status }
            : terminal,
        ),
    );
  } catch (error) {
    if (error instanceof RequestAbortedError) {
      admission.cancel();
      const response = requestAborted();
      onSettled?.({
        outcome: "cancelled",
        httpStatus: response.status,
        source: "request_aborted",
      });
      return response;
    }
    admission.release();
    if (error instanceof RuntimeMemoryAdmissionError) {
      const response = resourceUnavailable(error);
      onSettled?.({
        outcome: "error",
        httpStatus: response.status,
        source: "memory_admission",
      });
      return response;
    }
    if (error instanceof ModelInstallConsentError) {
      const response = openAIErrorResponse(
        {
          message: error.message,
          type: "invalid_request_error",
          param: "model",
          code: "model_install_consent_required",
        },
        409,
      );
      onSettled?.({ outcome: "error", httpStatus: response.status });
      return response;
    }
    const response = serviceUnavailable(serviceName);
    onSettled?.({ outcome: "error", httpStatus: response.status });
    return response;
  }
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

export async function applyElevatedMemoryPressure(
  reconciler: Pick<
    RuntimeReconciler,
    "evictIdleRuntimes" | "evictAllRuntimes"
  > &
    Partial<Pick<RuntimeReconciler, "isWithinReclaimGrace">>,
  videoJobs: Pick<VideoJobManager, "failActiveForMemoryPressure">,
  transition: MemorySafetyTransition,
): Promise<void> {
  const { current } = transition;
  if (current.state === "constrained") {
    const evicted = await reconciler.evictIdleRuntimes();
    if (
      evicted === 0 &&
      current.consecutiveNormalSnapshots === 0 &&
      !reconciler.isWithinReclaimGrace?.()
    ) {
      await videoJobs.failActiveForMemoryPressure();
    }
  } else if (current.state === "critical") {
    await videoJobs.failActiveForMemoryPressure();
    await reconciler.evictAllRuntimes();
  }
}

export async function admitModelWithIdleRecovery(
  reconciler: Pick<
    RuntimeReconciler,
    "admitModel" | "recoverWithIdleEviction" | "waitForAdmissionAfterEviction"
  >,
  modality: RuntimeModality,
  modelId: string,
  signal: AbortSignal,
): Promise<Awaited<ReturnType<RuntimeReconciler["admitModel"]>>> {
  const first = await reconciler.admitModel(modality, modelId, signal);
  if (
    first.kind === "insufficient-memory" &&
    first.error.capacity === undefined
  ) {
    signal.throwIfAborted();
    if (
      !(await reconciler.recoverWithIdleEviction(modality, modelId, signal))
    ) {
      return first;
    }
    signal.throwIfAborted();
    if (
      !(await reconciler.waitForAdmissionAfterEviction(
        modality,
        modelId,
        signal,
      ))
    ) {
      return first;
    }
    signal.throwIfAborted();
    const retry = await reconciler.admitModel(modality, modelId, signal);
    return retry.kind === "admitted" ? retry : first;
  }
  if (first.kind !== "admitted") return first;
  let current = first.value;
  const ready = current.admission.ready.catch(async (error) => {
    if (!(error instanceof RuntimeMemoryAdmissionError) || signal.aborted)
      throw error;
    current.admission.cancel();
    await current.admission.supervisor.kill();
    signal.throwIfAborted();
    if (error.capacity !== undefined) throw error;
    if (
      !(await reconciler.recoverWithIdleEviction(modality, modelId, signal))
    ) {
      throw error;
    }
    signal.throwIfAborted();
    if (
      !(await reconciler.waitForAdmissionAfterEviction(
        modality,
        modelId,
        signal,
      ))
    ) {
      throw error;
    }
    signal.throwIfAborted();
    const retry = await reconciler.admitModel(modality, modelId, signal);
    if (retry.kind !== "admitted") throw error;
    current = retry.value;
    try {
      await current.admission.ready;
    } catch {
      throw error;
    }
  });
  return {
    ...first,
    value: {
      ...first.value,
      admission: {
        get modality() {
          return current.admission.modality;
        },
        get snapshot() {
          return current.admission.snapshot;
        },
        get supervisor() {
          return current.admission.supervisor;
        },
        ready,
        onPendingDetach: (callback) =>
          current.admission.onPendingDetach(callback),
        onIdleCancellation: (callback) =>
          current.admission.onIdleCancellation(callback),
        markResponseStarted: () => current.admission.markResponseStarted(),
        cancel: () => current.admission.cancel(),
        release: () => current.admission.release(),
      },
    },
  };
}

export async function generateSpeechWithIdleRecovery<T>(
  reconciler: Pick<
    RuntimeReconciler,
    "recoverWithIdleEviction" | "waitForAdmissionAfterEviction"
  >,
  modelId: string,
  signal: AbortSignal,
  generate: () => Promise<T>,
): Promise<T> {
  try {
    return await generate();
  } catch (error) {
    if (
      !(error instanceof RuntimeMemoryAdmissionError) ||
      error.capacity !== undefined ||
      signal.aborted ||
      !(await reconciler.recoverWithIdleEviction("tts", modelId, signal))
    ) {
      throw error;
    }
    signal.throwIfAborted();
    if (
      !(await reconciler.waitForAdmissionAfterEviction("tts", modelId, signal))
    ) {
      throw error;
    }
    signal.throwIfAborted();
    return await generate();
  }
}

export function reportMemoryPressureTransition(
  logger: Pick<ILogger, "event">,
  transition: MemorySafetyTransition,
): void {
  const { current, previous } = transition;
  logger.event({
    severity:
      current.state === "healthy"
        ? "info"
        : current.state === "critical"
          ? "error"
          : "warn",
    eventName: "runtime.memory-pressure-changed",
    category: "runtime",
    component: "memory-safety",
    runtime: "gateway",
    message: "Runtime memory pressure changed.",
    attributes: {
      previous_state: previous.state,
      current_state: current.state,
    },
  });
}

export async function runServe(
  input: ServeInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { exitCode: number }; exitCode: number }> {
  const config = ctx.config;
  const enabled = serveModalityState(config, input);
  const owner = await getGatewayInstanceStateAtRoot(config.root);
  if (owner.state === "active") {
    throw new RootOwnershipError(
      `A LocalBase gateway already owns ${config.root}. Stop the existing gateway before starting another one.`,
    );
  }
  if (owner.state === "unknown") {
    throw new RootOwnershipError(
      `Cannot prove whether a LocalBase gateway owns ${config.root} (${owner.detail}). Stop the existing gateway before starting another one.`,
    );
  }
  await assertServePortsAvailable(config, {
    ...input,
    ...enabled,
    backendPortPreflight:
      process.env.LOCALBASE_TEST_SKIP_BACKEND_PORT_PREFLIGHT !== "1",
  });
  const browserAccess = await loadUiAccessConfig(config.root);
  const magicLinkRegistration =
    browserAccess?.provider.kind === "direct"
      ? browserAccess.provider.registrations.find(
          (registration) => registration.kind === "magic-link",
        )
      : undefined;
  const emailDelivery = magicLinkRegistration
    ? await loadEmailDeliveryConfig(config.root)
    : null;
  if (magicLinkRegistration && !emailDelivery)
    throw new CliInputError(
      "Magic-link authentication requires configured email delivery.",
    );
  const database = ctx.database.get(config.root);
  const runtimeCapabilities = createRuntimeCapabilityStore(database);
  const observedMemory = createObservedMemoryStore(database);
  const readCachedLlmKvGeometry = createLlmKvGeometryReader();
  const modelSupports = async (
    model: ModelSpec,
    capability: Capability,
  ): Promise<boolean> =>
    (
      await resolveCapabilitiesForModel(
        model,
        ctx.runtimeConfig.copy(),
        runtimeCapabilities,
      )
    ).capabilities.some(({ id }) => id === capability);
  if (!magicLinkRegistration) clearMagicLinks(database);
  const uiAccess = createUiAccess({
    config: browserAccess,
    sessionStore: createDatabaseBrowserSessionStore(database),
    authorizeIdentity: (identity) =>
      resolveAccessControl(database, identity, {
        claimManagedUser: true,
      }),
    ...(magicLinkRegistration && emailDelivery && browserAccess
      ? {
          magicLinks: createMagicLinkService({
            db: database,
            origin: browserAccess.origin,
            loadEmailDelivery: async () => {
              const current = await loadEmailDeliveryConfig(config.root);
              if (!current)
                throw new Error("Email delivery is not configured.");
              return current;
            },
            onDeliveryFailure: (error) =>
              ctx.logger.event({
                severity: "error",
                eventName: "authentication.magic-link-delivery-failed",
                category: "gateway",
                component: "authentication",
                runtime: "gateway",
                message: "Magic-link email delivery failed.",
                error:
                  error instanceof Error
                    ? { type: error.name, message: error.message }
                    : undefined,
              }),
          }),
        }
      : {}),
  });
  const wrapperHost = input.host ?? config.gatewayHost;
  const wrapperPort = input.port ?? config.gatewayPort;
  const startupStaticConfig = {
    gatewayHost: wrapperHost,
    gatewayPort: wrapperPort,
    memory: structuredClone(config.memory),
  };
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
  const startupLlmSpec = byId(config.activeLlmModel);
  const recommendedCtx = recommendedLlmContextSize(
    startupLlmSpec,
    ctx.specs.gpuVramGb,
  );
  const ctxSize = configuredLlmContextSize(
    startupLlmSpec,
    config.ctxSize,
    ctx.specs.gpuVramGb,
    input.ctxSize,
    config.ctxMode,
  );
  if (input.ctxSize) {
    console.log(
      `\n💡 Context Size: Using process override --ctx-size ${ctxSize} tokens (configured ceiling: ${config.ctxSize}, mode: ${config.ctxMode}).`,
    );
    console.log(
      `   This override does not persist across managed service restarts; use 'local-base configure --ctx-size ${ctxSize} --ctx-mode explicit' to make it permanent.`,
    );
  } else if (config.ctxMode === "explicit") {
    console.log(
      `\n💡 Context Size: Launching at the configured ceiling of ${ctxSize} tokens (mode: explicit).`,
    );
    if (recommendedCtx < ctxSize) {
      console.warn(
        `   The memory-safe recommendation for "${config.activeLlmModel}" on ${ctx.specs.gpuVramGb} GB hardware is ${recommendedCtx} tokens.`,
      );
    }
  } else {
    console.log(`\n💡 Dynamic Context Size: Initialized to ${ctxSize} tokens.`);
    console.log(
      `   (mode: auto; configured ceiling: ${config.ctxSize} tokens; memory-safe recommendation for "${config.activeLlmModel}" on ${ctx.specs.gpuVramGb} GB hardware: ${recommendedCtx} tokens)`,
    );
    if (recommendedCtx < config.ctxSize) {
      console.log(
        `   Run 'local-base configure --ctx-mode explicit' to launch at the full ${config.ctxSize}-token ceiling instead.`,
      );
    }
  }
  console.log(
    `   Parallel slots divide the launch context per request; the resolved budget is reported under "context_budget" in /v1/models.`,
  );

  const sttPath = input.sttPath ?? "/inference";
  const authRequired = input.auth ?? true;
  const authMode = parseAuthMode(input.authMode);
  const gatewayRequestAuth = (request: Request): GatewayRequestAuth => {
    const token = extractAuthToken(request, authMode);
    const browserRequest = isBrowserRequest(request);
    const credentialPresented =
      token !== null ||
      request.headers.has("cookie") ||
      request.headers.has("cf-access-jwt-assertion");
    let resolved = false;
    let principal: Principal = { kind: "anonymous" };
    const resolveUiPrincipal = (currentRequest: Request): Principal => {
      const uiCredential = uiAccess.credential(currentRequest);
      if (!uiCredential) return { kind: "anonymous" };
      return principalSchema.parse({
        kind: "browser-session",
        ownerId: uiCredential.ownerId,
        permissions: uiCredential.permissions,
      });
    };
    return Object.freeze({
      resolve(config: LocalBaseConfig) {
        if (resolved) return principal;
        resolved = true;
        if (!token || browserRequest) return principal;
        if (token === process.env.LOCALBASE_API_KEY) {
          principal = principalSchema.parse({
            kind: "environment",
            permissions: permissionSchema.options,
          });
          return principal;
        }
        const apiKey = resolveApiKey(ctx.database, config, token);
        if (apiKey) {
          principal = principalSchema.parse({
            kind: "api-key",
            id: apiKey.id,
            name: apiKey.name,
            permissions: apiKey.scopes,
          });
        }
        return principal;
      },
      resolveUi(currentRequest: Request) {
        if (resolved) return principal;
        resolved = true;
        principal = resolveUiPrincipal(currentRequest);
        return principal;
      },
      telemetry(required: boolean): NonNullable<HttpRequestLogInput["auth"]> {
        if (!required) return { outcome: "disabled" };
        switch (principal.kind) {
          case "api-key":
            return {
              outcome: "authenticated",
              principalId: principal.id,
              principalName: principal.name,
              source: "stored_key",
            };
          case "environment":
            return {
              outcome: "authenticated",
              principalId: "environment",
              source: "environment",
            };
          case "browser-session":
            return {
              outcome: "authenticated",
              principalId: principal.ownerId,
              source: "browser_session",
            };
          case "anonymous":
            return { outcome: credentialPresented ? "invalid" : "missing" };
          default: {
            const exhaustive: never = principal;
            return exhaustive;
          }
        }
      },
    });
  };

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
  if (enabled.video && !config.activeVideoModel) {
    throw new Error(
      "Video modality is enabled but no active Video model is configured. Run `local-base configure` first.",
    );
  }
  if (enabled.tts && !config.activeTtsModel) {
    throw new Error(
      "TTS modality is enabled but no active TTS model is configured. Run `local-base configure` first.",
    );
  }

  // Perform memory fit evaluation BEFORE downloading
  const specs = ctx.specs;
  const getModelIdFromFile = (filename: string): string => {
    return filename.replace(/\.(gguf|bin|onnx|safetensors|pth)$/i, "");
  };

  const bypassCheck = input.bypassMemoryCheck;
  const preflightMemoryInputs = async (
    spec: ModelSpec,
  ): Promise<ModelMemoryInputs> => {
    const inputs = await collectModelMemoryInputs({
      catalog: [spec],
      installations: await inspectCatalogInstallations(config, [spec]),
      directoryFor: (model) => directoryForModel(config, model.kind),
      observed: observedMemory,
      readGeometry: readCachedLlmKvGeometry,
    });
    return inputs.get(spec.modelId) ?? {};
  };

  if (enabled.llm) {
    const llmModelId = getModelIdFromFile(llmModelFile);
    const llmSpec = byId(llmModelId);
    if (llmSpec) {
      const fit = evaluateModelFit(
        llmSpec,
        specs.gpuVramGb,
        await preflightMemoryInputs(llmSpec),
      );
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run LLM "${llmSpec.modelId}".`,
        );
        console.error(
          `   Model minimum requirement: ${fit.minVramGb} GB (${fit.source})`,
        );
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
        console.warn(
          `   Model minimum requirement: ${fit.minVramGb} GB (${fit.source})`,
        );
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.warn(
          `   Leaves only ${fit.headroomGb.toFixed(1)} GB headroom. Large context windows may cause slowdowns.`,
        );
      } else {
        console.log(
          `\n✅ Memory check passed: LLM "${llmSpec.modelId}" fits comfortably in ${specs.gpuVramGb} GB (${fit.source} minimum ${fit.minVramGb} GB).`,
        );
      }

      const maxSafeCtx = calculateMaxSafeContextSize(llmSpec, specs.gpuVramGb);
      if (ctxSize < maxSafeCtx) {
        console.log(
          `\n💡 Tip: Your system supports a larger context size of up to ${maxSafeCtx} tokens for "${llmSpec.modelId}".`,
        );
        console.log(
          `   Persist it with 'local-base configure --ctx-size ${maxSafeCtx}' (add '--ctx-mode explicit' to bypass the memory-safe clamp), or start once with '--ctx-size ${maxSafeCtx}'.`,
        );
      }
    }
  }

  if (enabled.stt) {
    const sttModelId = getModelIdFromFile(sttModelFile);
    const sttSpec = byId(sttModelId);
    if (sttSpec) {
      const fit = evaluateModelFit(
        sttSpec,
        specs.gpuVramGb,
        await preflightMemoryInputs(sttSpec),
      );
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run STT "${sttSpec.modelId}".`,
        );
        console.error(
          `   Model minimum requirement: ${fit.minVramGb} GB (${fit.source})`,
        );
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
        console.warn(
          `   Model minimum requirement: ${fit.minVramGb} GB (${fit.source})`,
        );
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  if (enabled.image) {
    const imageModelId = getModelIdFromFile(imageModelFile);
    const imageSpec = byId(imageModelId);
    if (imageSpec) {
      const fit = evaluateModelFit(
        imageSpec,
        specs.gpuVramGb,
        await preflightMemoryInputs(imageSpec),
      );
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run Image model "${imageSpec.modelId}".`,
        );
        console.error(
          `   Model minimum requirement: ${fit.minVramGb} GB (${fit.source})`,
        );
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
        console.warn(
          `   Model minimum requirement: ${fit.minVramGb} GB (${fit.source})`,
        );
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  // Model weights require explicit consent; managed runtime binaries remain automatic.
  if (enabled.llm && !llmModelExists) {
    const spec = byId(config.activeLlmModel);
    const installedPath = await installMissingModel(
      config,
      spec,
      config.activeLlmModel,
      input.installMissing || config.installMissingModels,
      () => {
        console.log(
          `Installing missing LLM model "${config.activeLlmModel}"...`,
        );
        return installSelectedModel(
          ctx,
          config,
          "llm",
          config.activeLlmModel,
          "incomplete",
          input.installMissing || config.installMissingModels,
        );
      },
    );
    llmModelFile = basename(installedPath);
    llmModelExists = true;
  }

  if (enabled.stt && !sttModelExists) {
    const spec = byId(config.activeSttModel);
    const installedPath = await installMissingModel(
      config,
      spec,
      config.activeSttModel,
      input.installMissing || config.installMissingModels,
      () => {
        console.log(
          `Installing missing STT model "${config.activeSttModel}"...`,
        );
        return installSelectedModel(
          ctx,
          config,
          "stt",
          config.activeSttModel,
          "missing",
          input.installMissing || config.installMissingModels,
        );
      },
    );
    sttModelFile = basename(installedPath);
    sttModelExists = true;
  }

  if (enabled.image && !imageModelExists) {
    const spec = byId(config.activeImageModel);
    const installedPath = await installMissingModel(
      config,
      spec,
      config.activeImageModel,
      input.installMissing || config.installMissingModels,
      () => {
        console.log(
          `Installing missing image model "${config.activeImageModel}"...`,
        );
        return installSelectedModel(
          ctx,
          config,
          "image",
          config.activeImageModel,
          "missing",
          input.installMissing || config.installMissingModels,
        );
      },
    );
    imageModelFile = basename(installedPath);
    imageModelExists = true;
  }

  if (
    !enabled.llm &&
    !enabled.stt &&
    !enabled.tts &&
    !enabled.image &&
    !enabled.video
  ) {
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
  if (!enabled.tts && input.tts === undefined) {
    console.log("TTS route auto-disabled (no selected TTS model).");
  }
  if (!enabled.image && input.image === undefined) {
    console.log("Image route auto-disabled (no local Image model file found).");
  }

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
      ...(input.ttsModelFile ? ["activeTtsModel" as const] : []),
      ...(input.imageModelFile ? ["activeImageModel" as const] : []),
    ],
    configuredModalities: {
      ...(input.llm === undefined ? {} : { llm: input.llm }),
      ...(input.stt === undefined ? {} : { stt: input.stt }),
      ...(input.tts === undefined ? {} : { tts: input.tts }),
      ...(input.image === undefined ? {} : { image: input.image }),
      ...(input.video === undefined ? {} : { video: input.video }),
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
  const readCachedLlmTrainingContextLength =
    createLlmTrainingContextLengthReader();
  const memoryProvider = createHostMemoryProvider();
  const memorySafety = new MemorySafetyController(
    memoryProvider,
    ctx.config.memory,
    bypassCheck,
    platformMemorySampler() ?? undefined,
  );
  // Aborted on shutdown so an in-flight memory sample never delays exit.
  const observationStop = new AbortController();
  const factory = createRuntimeSupervisorFactory(ctx, launchOverrides, {
    memorySafety,
    observedMemory,
    installMissing: input.installMissing || config.installMissingModels,
    onBackendReady: ({ modelId, ready }) => {
      const target = observationTarget(byId(modelId), ready.plan);
      const sampler = platformMemorySampler();
      if (!target || !sampler) return;
      void observePeakMemory({
        signal: observationStop.signal,
        pid: ready.pid,
        target,
        exited: ready.exited,
        sampler,
        store: observedMemory,
        onError: (error) =>
          ctx.logger.warn(
            "memory",
            `Could not record the memory peak of ${modelId}: ${error instanceof Error ? error.message : String(error)}`,
          ),
      });
    },
    onLlmReady: async ({ modelId, baseUrl, embedding, decision }) => {
      const spec = byId(modelId);
      const artifactSha256 = spec ? runtimeCapabilityKey(spec) : null;
      if (!artifactSha256) return;
      const record = await probeLlamaServerCapabilities({
        baseUrl,
        modelId,
        artifactSha256,
        embedding,
        decision,
        store: runtimeCapabilities,
      });
      if (record)
        ctx.logger.info(
          "llama-server",
          `Recorded runtime capabilities for ${modelId} (${record.buildInfo}).`,
        );
    },
  });
  const initialSnapshot = ctx.runtimeConfig.read();
  const supervisors = new SupervisorRegistry({
    ...(enabled.llm ? { llm: factory.create("llm", initialSnapshot) } : {}),
    ...(enabled.stt ? { stt: factory.create("stt", initialSnapshot) } : {}),
    ...(enabled.tts ? { tts: factory.create("tts", initialSnapshot) } : {}),
    ...(enabled.image
      ? { image: factory.create("image", initialSnapshot) }
      : {}),
    ...(enabled.video
      ? { video: factory.create("video", initialSnapshot) }
      : {}),
  });
  const videoJobs = new VideoJobManager({
    backend: createStableDiffusionVideoClient({
      baseUrl: factory.baseUrl("video", initialSnapshot),
    }),
    temporaryDirectory: join(config.root, "tmp"),
    prepareArtifact: createVideoArtifactPreparer({
      ensureConverter: (signal) => ensureVideoConverter(config.root, signal),
    }),
    onContainmentFailure: ({ jobId, source }) => {
      ctx.logger.event({
        severity: "error",
        eventName: "video.job-containment-failed",
        category: "runtime",
        component: "video-job-manager",
        runtime: "video",
        message: "A local video job could not be contained.",
        attributes: { job_id: jobId, source },
        error: {
          type: "VideoContainmentFailure",
          message: "A local video job could not be contained.",
        },
      });
    },
  });
  const reconciler = new RuntimeReconciler(
    ctx.runtimeConfig,
    configuredOverrides,
    supervisors,
    factory,
    ctx.logger,
    {
      maxWaiting: input.inferenceQueueCapacity,
      waitMs: input.inferenceQueueTimeoutMs,
    },
    {
      beforeModalityDrain: async (modality) => {
        if (modality === "video") await videoJobs.cancelActive();
      },
    },
  );
  const management = createModelManagement({
    runtimeConfig: ctx.runtimeConfig,
    lifecycle: () => reconciler.lifecycleSnapshot(),
    protectedModelIds: () => reconciler.protectedModelIds(),
    videoTarget: () => videoTargetFromTopology(memoryProvider.topology),
  });
  const authManagement = createAuthManagement({
    root: config.root,
    database: ctx.database,
    configuration: () => ctx.runtimeConfig.copy(),
  });
  const memoryPressureMonitor = new MemoryPressureMonitor({
    controller: memorySafety,
    onElevatedPressure: async (transition) => {
      await applyElevatedMemoryPressure(reconciler, videoJobs, transition);
    },
    onTransition: (transition) =>
      reportMemoryPressureTransition(ctx.logger, transition),
    onError: (error) => {
      ctx.logger.event({
        severity: "error",
        eventName: "runtime.memory-pressure-monitor-failed",
        category: "runtime",
        component: "memory-safety",
        runtime: "gateway",
        message: "Runtime memory pressure monitoring failed.",
        error: {
          type: error instanceof Error ? error.name : "Error",
          message: error instanceof Error ? error.message : String(error),
        },
      });
    },
  });

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
  const readinessSnapshot = () =>
    composeGatewayReadiness({
      stopping: gatewayStopping,
      lifecycles: reconciler.lifecycleSnapshot(),
    });

  const handleRequest = async (
    request: Request,
    pathname: string,
    requestId: string,
    startedAt: number,
    requestAuth: GatewayRequestAuth,
    requirement: AuthorizationRequirement,
    playground?: Response,
  ): Promise<Response> => {
    const uiResult = await uiAccess.handle(request);
    if (uiResult.kind === "response") {
      requestAuth.resolveUi(request);
      return uiResult.response;
    }
    if (uiResult.kind === "forward") {
      request = uiResult.request;
      pathname = uiResult.pathname;
      requestAuth.resolveUi(request);
    }
    const authManagementResponse = await authManagement(
      request,
      requestAuth.resolve(ctx.runtimeConfig.copy()),
    );
    if (authManagementResponse) return authManagementResponse;
    const publicAsset = playground ?? playgroundResponse(request, pathname);
    if (publicAsset) return publicAsset;
    if (pathname === "/_localbase/hf-token") {
      const principal = requestAuth.resolve(ctx.runtimeConfig.copy());
      if (principal.kind === "anonymous") return unauthorized();
      if (request.method !== "GET" && request.method !== "POST")
        return methodNotAllowed("GET, POST");
      if (
        authorize({
          principal,
          requirement: { kind: "permission", permission: "models:manage" },
        }).kind !== "authorized"
      )
        return forbidden();
      const headers = { "cache-control": "no-store" };
      const tokenDeps = {
        saved: () => ctx.runtimeConfig.copy().hfToken || undefined,
        env: () => process.env.HF_TOKEN || undefined,
        save: async (token: string) => {
          await ctx.runtimeConfig.update((fresh) => {
            fresh.hfToken = token;
          });
        },
        check: (token: string) => checkHubToken(token),
      };
      ctx.runtimeConfig.refreshSync();
      if (request.method === "GET")
        return Response.json(hubTokenStatus(tokenDeps), { headers });
      let input;
      try {
        const body = await readBoundedRequestBody(request, 1024);
        input = hubTokenRequestSchema.safeParse(
          JSON.parse(new TextDecoder().decode(body)),
        );
      } catch (error) {
        if (request.signal.aborted) return requestAborted();
        const tooLarge = error instanceof PayloadTooLargeError;
        return openAIErrorResponse(
          {
            message: tooLarge
              ? "Hugging Face token body exceeds the 1024 byte limit."
              : "Invalid Hugging Face token payload.",
            type: "invalid_request_error",
            param: null,
            code: "validation_failed",
          },
          tooLarge ? 413 : 400,
          tooLarge ? { ...headers, connection: "close" } : headers,
        );
      }
      // The parse error would quote the rejected input; keep it generic.
      if (!input.success)
        return openAIErrorResponse(
          {
            message:
              "Expected a Hugging Face access token without spaces (8 to 256 characters).",
            type: "invalid_request_error",
            param: null,
            code: "validation_failed",
          },
          400,
          headers,
        );
      const outcome = await runHubTokenRequest(tokenDeps, input.data);
      if ("error" in outcome.body)
        return openAIErrorResponse(
          {
            message: outcome.body.error.message,
            type: "invalid_request_error",
            param: null,
            code: outcome.body.error.code,
          },
          outcome.status,
          headers,
        );
      return Response.json(outcome.body, { status: outcome.status, headers });
    }
    if (pathname === "/_localbase/model-discovery") {
      const principal = requestAuth.resolve(ctx.runtimeConfig.copy());
      if (principal.kind === "anonymous") return unauthorized();
      if (request.method !== "POST") return methodNotAllowed("POST");
      if (
        authorize({
          principal,
          requirement: { kind: "permission", permission: "models:manage" },
        }).kind !== "authorized"
      )
        return forbidden();
      const headers = { "cache-control": "no-store" };
      let input;
      try {
        const body = await readBoundedRequestBody(request, 2048);
        input = discoveryRequestSchema.safeParse(
          JSON.parse(new TextDecoder().decode(body)),
        );
      } catch (error) {
        if (request.signal.aborted) return requestAborted();
        const tooLarge = error instanceof PayloadTooLargeError;
        return openAIErrorResponse(
          {
            message: tooLarge
              ? "Model discovery body exceeds the 2048 byte limit."
              : "Invalid model discovery payload.",
            type: "invalid_request_error",
            param: null,
            code: "validation_failed",
          },
          tooLarge ? 413 : 400,
          tooLarge ? { ...headers, connection: "close" } : headers,
        );
      }
      if (!input.success) return badRequest("Invalid model discovery payload.");
      ctx.runtimeConfig.refreshSync();
      const runtime = ctx.runtimeConfig.copy();
      const token = hubTokenFrom(runtime);
      const outcome = await runDiscoveryRequest(
        {
          root: runtime.root,
          ...(token ? { token } : {}),
          systemVramGb: ctx.specs.gpuVramGb,
          refresh: async () => {
            await reconciler.refreshConfiguration();
          },
          install: async (modelId) => {
            try {
              return await management.run(modelId, "install");
            } catch (error) {
              if (!(error instanceof ModelManagementError)) throw error;
              return {
                action: "install",
                state: "failed",
                detail: error.message,
              };
            }
          },
        },
        input.data,
      );
      if ("error" in outcome.body) {
        const error = outcome.body.error as { message: string; code: string };
        return openAIErrorResponse(
          {
            message: error.message,
            type: "invalid_request_error",
            param: null,
            code: error.code,
          },
          outcome.status,
          headers,
        );
      }
      return Response.json(outcome.body, { status: outcome.status, headers });
    }
    if (pathname === "/_localbase/model-management") {
      const principal = requestAuth.resolve(ctx.runtimeConfig.copy());
      if (principal.kind === "anonymous") return unauthorized();
      const canRead =
        authorize({
          principal,
          requirement: { kind: "permission", permission: "models:read" },
        }).kind === "authorized";
      const canManage =
        authorize({
          principal,
          requirement: { kind: "permission", permission: "models:manage" },
        }).kind === "authorized";
      const headers = { "cache-control": "no-store" };
      if (request.method === "GET") {
        if (!canRead) return forbidden();
        return Response.json(
          { ...(await management.read()), canManage },
          { headers },
        );
      }
      if (request.method !== "POST") return methodNotAllowed("GET, POST");
      if (!canManage) return forbidden();
      let input;
      try {
        const body = await readBoundedRequestBody(request, 1024);
        input = modelManagementRequestSchema.safeParse(
          JSON.parse(new TextDecoder().decode(body)),
        );
      } catch (error) {
        if (request.signal.aborted) return requestAborted();
        return openAIErrorResponse(
          {
            message:
              error instanceof PayloadTooLargeError
                ? "Model management body exceeds the 1024 byte limit."
                : "Invalid model management payload.",
            type: "invalid_request_error",
            param: null,
            code: "validation_failed",
          },
          error instanceof PayloadTooLargeError ? 413 : 400,
          error instanceof PayloadTooLargeError
            ? { ...headers, connection: "close" }
            : headers,
        );
      }
      if (!input.success)
        return badRequest("Invalid model management payload.");
      try {
        const operation = await management.run(
          input.data.modelId,
          input.data.action,
        );
        if (input.data.action !== "install")
          await reconciler.refreshConfiguration();
        return Response.json(operation, {
          status: operation.state === "running" ? 202 : 200,
          headers,
        });
      } catch (error) {
        if (!(error instanceof ModelManagementError)) throw error;
        return openAIErrorResponse(
          {
            message: error.message,
            type: "invalid_request_error",
            param: null,
            code: error.code,
          },
          error.code === "invalid_request" ? 400 : 409,
          headers,
        );
      }
    }
    const route = selectGatewayRoute(pathname);
    const queueFailure = (error: unknown, modality: RuntimeModality) =>
      inferenceQueueError(error, {
        modality,
        requestId,
        logger: ctx.logger,
        span: trace.getSpan(context.active()),
      });
    const beginInference = (
      modality: RuntimeModality,
      admission: AdmittedModel,
      options: InferenceTelemetryOptions = { streaming: false },
    ) =>
      new InferenceTelemetry({
        metadata: inferenceMetadata(
          modality,
          admission.modelId,
          admission.admissionSnapshot,
        ),
        requestId,
        startedAt,
        queueWaitMs: admission.queueWaitMs,
        ...options,
        logger: ctx.logger,
        span: ctx.otel.startSpan(
          "localbase.inference",
          internalSpanOptions({ "http.request.method": request.method }),
        ),
      });
    const authorization = authorize({
      principal:
        requirement.kind === "public"
          ? { kind: "anonymous" }
          : requestAuth.resolve(ctx.runtimeConfig.copy()),
      requirement,
    });
    if (authorization.kind === "unauthenticated") return unauthorized();
    if (authorization.kind === "forbidden") return forbidden();

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

    if (route === "readiness") {
      if (request.method !== "GET" && request.method !== "HEAD") {
        return methodNotAllowed("GET, HEAD");
      }
      const readiness = readinessSnapshot();
      if (serviceId && serviceToken && readiness.status === "ready") {
        acknowledgeStaticConfiguration(
          ctx.database,
          config.root,
          startupStaticConfig,
        );
      }
      const body = JSON.stringify(readiness);
      return new Response(request.method === "HEAD" ? null : body, {
        status: readiness.status === "ready" ? 200 : 503,
        headers: {
          "content-type": "application/json; charset=utf-8",
          "content-length": String(new TextEncoder().encode(body).byteLength),
        },
      });
    }

    if (route === "modelMetadataList" || route === "modelMetadataDetail") {
      const currentConfig = ctx.runtimeConfig.copy();
      const runtimes = reconciler.lifecycleSnapshot();
      if (request.method !== "GET") return methodNotAllowed("GET");

      const installations = inspectCatalogInstallations(currentConfig, CATALOG);
      const memory = installations.then((resolved) =>
        collectModelMemoryInputs({
          catalog: CATALOG,
          installations: resolved,
          directoryFor: (model) => directoryForModel(currentConfig, model.kind),
          observed: observedMemory,
          readGeometry: readCachedLlmKvGeometry,
        }),
      );
      const capabilities = resolveCatalogCapabilities(
        CATALOG,
        currentConfig,
        runtimeCapabilities,
      );
      if (route === "modelMetadataList") {
        const [
          resolvedInstallations,
          resolvedCapabilities,
          hostMemorySnapshot,
          resolvedMemory,
        ] = await Promise.all([
          installations,
          capabilities,
          memoryProvider.snapshot(),
          memory,
        ]);
        const list = projectModelMetadataList(
          {
            catalog: CATALOG,
            config: currentConfig,
            installations: resolvedInstallations,
            capabilities: resolvedCapabilities,
            runtimes,
            memory: resolvedMemory,
            systemMemoryGb: ctx.specs.gpuVramGb,
          },
          {
            topology: memoryProvider.topology,
            snapshot: hostMemorySnapshot,
          },
        );
        // Optional catalog query parameters filter and sort the list; without
        // them every model is returned in catalog order.
        const query = parseCatalogQuery(
          new URL(request.url).searchParams,
          modelKindSchema.options,
        );
        return Response.json({
          ...list,
          data: queryCatalog(list.data, metadataCatalogRow, query),
        });
      }
      const metadataInput = {
        catalog: CATALOG,
        config: currentConfig,
        installations: await installations,
        capabilities: await capabilities,
        runtimes,
        memory: await memory,
        systemMemoryGb: ctx.specs.gpuVramGb,
      };

      const modelId = modelMetadataIdFromPath(pathname);
      if (!modelId) return routeNotFound();
      const metadata = modelMetadataById(modelId, metadataInput);
      return metadata ? Response.json(metadata) : modelNotFound(modelId);
    }

    await reconciler.refreshConfiguration();
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

    if (requestExceedsSizeLimit(request)) {
      await discardDeclaredOversizedBody(request, MAX_REQUEST_BYTES);
      return payloadTooLarge();
    }

    if (
      (authorization.kind === "authorized" ||
        (authorization.kind === "public" && request.method !== "OPTIONS")) &&
      (route === "videoCreate" ||
        route === "videoStatus" ||
        route === "videoContent" ||
        route === "videoCancel")
    ) {
      return await handleVideoGatewayRequest({
        request,
        pathname,
        route,
        ownerId:
          authorization.kind === "authorized"
            ? principalOwnerId(authorization.principal)
            : LOCAL_VIDEO_OWNER_ID,
        jobs: videoJobs,
        createEnabled: currentConfig.selectedVideoModels.length > 0,
        assertInstallConsent: async (modelId) => {
          const spec = byId(modelId);
          if (!spec || spec.kind !== "video") return;
          const installation = await resolveCatalogInstallation(
            spec,
            currentConfig.videoModelsDir,
          );
          if (!installation.complete)
            assertModelInstallConsent(
              modelId,
              input.installMissing || currentConfig.installMissingModels,
            );
        },
        admissionProvider: {
          admit: async (modelId, signal) => {
            const selection = await admitModelWithIdleRecovery(
              reconciler,
              "video",
              modelId,
              signal,
            );
            // Preserve the memory error so the asynchronous job reports its code.
            if (selection.kind === "insufficient-memory") {
              throw selection.error;
            }
            if (selection.kind !== "admitted") return selection;
            return {
              kind: "admitted",
              admission: selection.value.admission,
            };
          },
        },
        parseCreateRequest: async () => {
          if (
            request.headers
              .get("content-type")
              ?.toLowerCase()
              .startsWith("multipart/form-data;")
          ) {
            try {
              const bytes = await readBoundedRequestBody(
                request,
                9 * 1024 * 1024,
              );
              const form = await new Request(request.url, {
                method: "POST",
                headers: {
                  "content-type": request.headers.get("content-type") ?? "",
                },
                body: requestBodyBuffer(bytes),
              }).formData();
              const reference = form.get("input_reference");
              let input_reference: string | undefined;
              if (reference instanceof File) {
                const media = new Uint8Array(await reference.arrayBuffer());
                if (
                  media.byteLength > 4 * 1024 * 1024 ||
                  imageMimeFromMagic(media) !== "image/png"
                ) {
                  return {
                    success: false,
                    response: badRequest(
                      "input_reference must be a PNG file no larger than 4 MiB.",
                      "input_reference",
                    ),
                  };
                }
                input_reference = `data:image/png;base64,${media.toBase64()}`;
              } else if (typeof reference === "string") {
                input_reference = reference;
              }
              const inputField = form.get("input");
              let input: unknown = { kind: "text" };
              if (typeof inputField === "string") {
                try {
                  input = JSON.parse(inputField);
                } catch {
                  return {
                    success: false,
                    response: badRequest("input must be valid JSON.", "input"),
                  };
                }
              }
              const payload = {
                model: form.get("model"),
                prompt: form.get("prompt"),
                negative_prompt: form.get("negative_prompt") ?? undefined,
                width: Number(form.get("width")),
                height: Number(form.get("height")),
                frames: Number(form.get("frames")),
                fps: Number(form.get("fps")),
                input,
                ...(input_reference === undefined ? {} : { input_reference }),
              };
              const parsed = videoCreateRequestSchema.safeParse(payload);
              return parsed.success
                ? { success: true, data: parsed.data }
                : {
                    success: false,
                    response: badRequest("Invalid video request."),
                  };
            } catch (error) {
              return {
                success: false,
                response:
                  error instanceof PayloadTooLargeError
                    ? payloadTooLarge(9 * 1024 * 1024)
                    : badRequest("Invalid multipart video request."),
              };
            }
          }
          return await parseJsonRequest(request, videoCreateRequestSchema);
        },
        notConfigured: () => notConfigured("Video"),
        modelNotFound,
        badRequest,
        methodNotAllowed,
        routeNotFound,
        requestAborted,
        onTerminal: ({ job, modelId }) => {
          logVideoJobTerminal({
            logger: ctx.logger,
            job,
            modelId,
            requestId,
          });
        },
      });
    }

    if (route === "speechGeneration") {
      if (request.method !== "POST") return methodNotAllowed("POST");
      const parsed = await parseJsonRequest(
        request,
        speechGenerationRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      const speechModel = byId(parsed.data.model);
      if (
        speechModel?.kind === "tts" &&
        !ttsVoiceAllowed(speechModel, parsed.data.voice)
      ) {
        return badRequest(
          `Voice "${parsed.data.voice}" is not supported by model "${parsed.data.model}".`,
        );
      }

      let admission: RuntimeAdmission | undefined;
      let inference: InferenceTelemetry | undefined;
      try {
        const selected = await admitModelWithIdleRecovery(
          reconciler,
          "tts",
          parsed.data.model,
          request.signal,
        );
        if (selected.kind === "not-configured") return notConfigured("TTS");
        if (selected.kind === "model-not-found") {
          return modelNotFound(parsed.data.model);
        }
        if (selected.kind === "unavailable") return serviceUnavailable("TTS");
        if (selected.kind === "insufficient-memory") {
          return resourceUnavailable(selected.error);
        }
        admission = selected.value.admission;
        inference = beginInference("tts", selected.value);
        if (admission.supervisor.kind !== "speech") {
          admission.release();
          const response = serviceUnavailable("TTS");
          inference.finish({ outcome: "error", httpStatus: response.status });
          return response;
        }
        const speechSupervisor = admission.supervisor;

        await waitForRequestAbort(admission.ready, request.signal);
        const streamFormat = parsed.data.stream_format;
        if (streamFormat) {
          const speechInput = {
            text: parsed.data.input,
            voice: parsed.data.voice,
            signal: request.signal,
          };
          const audio = await generateSpeechWithIdleRecovery(
            reconciler,
            parsed.data.model,
            request.signal,
            async () =>
              speechSupervisor.generateSpeechStream
                ? await speechSupervisor.generateSpeechStream(speechInput)
                : bufferedSpeechStream(
                    await speechSupervisor.generateSpeech(speechInput),
                  ),
          );
          admission.markResponseStarted();
          const lease = admission;
          admission = undefined;
          return withResponseLease(
            withServerTiming(
              speechStreamResponse(
                audio,
                parsed.data.response_format,
                streamFormat,
              ),
              inference.serverTiming(),
            ),
            () => lease.release(),
            () => lease.cancel(),
            request.signal,
            (terminal) => inference?.finish(terminal),
          );
        }
        const generate = () =>
          speechSupervisor.generateSpeech({
            text: parsed.data.input,
            voice: parsed.data.voice,
            signal: request.signal,
          });
        const wav = await generateSpeechWithIdleRecovery(
          reconciler,
          parsed.data.model,
          request.signal,
          generate,
        );
        admission.markResponseStarted();
        admission.release();
        admission = undefined;
        const pcmOnly = parsed.data.response_format === "pcm";
        const audioBytes = pcmOnly ? speechPcmFromWav(wav) : wav;
        const body = new ArrayBuffer(audioBytes.byteLength);
        new Uint8Array(body).set(audioBytes);
        const response = withServerTiming(
          new Response(body, {
            headers: {
              "content-type": pcmOnly ? SPEECH_PCM_CONTENT_TYPE : "audio/wav",
              "content-length": String(audioBytes.byteLength),
            },
          }),
          inference.serverTiming(),
        );
        return withResponseLease(
          response,
          () => {},
          () => {},
          request.signal,
          (terminal) => inference?.finish(terminal),
        );
      } catch (error) {
        if (
          request.signal.aborted ||
          error instanceof RuntimeRequestAbortedError ||
          error instanceof SpeechGenerationAbortedError
        ) {
          admission?.cancel();
          const response = requestAborted();
          inference?.finish({
            outcome: "cancelled",
            httpStatus: response.status,
            source: "request_aborted",
          });
          return response;
        }
        admission?.release();
        const queueError = queueFailure(error, "tts");
        if (queueError) return queueError;
        const failure = speechGenerationFailure(error);
        inference?.finish({
          outcome: "error",
          httpStatus: failure.response.status,
          ...(failure.source ? { source: failure.source } : {}),
        });
        return failure.response;
      }
    }

    if (route === "transcription") {
      let admission: RuntimeAdmission | undefined;
      let admitted: AdmittedModel | undefined;
      let transcriptionFormat: TranscriptionResponseFormat = "json";
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
        const selected = await admitModelWithIdleRecovery(
          reconciler,
          "stt",
          parsed.data.model ?? currentConfig.activeSttModel,
          request.signal,
        );
        if (selected.kind === "not-configured") return notConfigured("STT");
        if (selected.kind === "model-not-found") {
          return modelNotFound(parsed.data.model ?? "");
        }
        if (selected.kind === "unavailable") return serviceUnavailable("STT");
        if (selected.kind === "insufficient-memory") {
          return resourceUnavailable(selected.error);
        }
        admitted = selected.value;
        admission = selected.value.admission;
        transcriptionFormat = parsed.data.response_format ?? "json";
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
        // whisper-server has one /inference endpoint; translation is a flag.
        if (new URL(request.url).pathname === "/v1/audio/translations") {
          normalizedForm.set("translate", "true");
        }
        request = new Request(request.url, {
          method: request.method,
          body: normalizedForm,
          signal: request.signal,
        });
      } catch (e) {
        if (request.signal.aborted || e instanceof RuntimeRequestAbortedError) {
          admission?.cancel();
          return requestAborted();
        }
        admission?.release();
        const queueError = queueFailure(e, "stt");
        if (queueError) return queueError;
        return e instanceof PayloadTooLargeError
          ? payloadTooLarge()
          : badRequest("Invalid form data payload.");
      }
      if (!admission || !admitted) return serviceUnavailable("STT");
      const inference = beginInference("stt", admitted);
      const response = await proxyWithAdmission(
        admission,
        "STT",
        request.signal,
        async () =>
          await proxyRequest(
            request,
            factory.baseUrl("stt", admission.snapshot),
            sttPath,
            transcriptionResponseSchema,
            undefined,
            ctx.otel,
            undefined,
            (status) =>
              inference.finish({
                outcome: "error",
                httpStatus: status,
                source: "response_validation",
              }),
            undefined,
            undefined,
            (status) => inference.observeUpstreamStatus(status),
            inference.context(),
            transcriptionFormat,
          ),
        (terminal) => inference.finish(terminal),
      );
      return withServerTiming(response, inference.serverTiming());
    }

    if (route === "imageGeneration") {
      const parsed = await parseJsonRequest(
        request,
        imageGenerationRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      const {
        negative_prompt,
        seed,
        steps,
        cfg_scale,
        lora_strength,
        ...generation
      } = parsed.data;
      const loraError = unusableLoraStrength(
        byId(parsed.data.model ?? currentConfig.activeImageModel ?? ""),
        lora_strength,
      );
      if (loraError) return loraError;
      let selected;
      try {
        selected = await admitModelWithIdleRecovery(
          reconciler,
          "image",
          parsed.data.model ?? currentConfig.activeImageModel,
          request.signal,
        );
      } catch (error) {
        if (error instanceof RuntimeRequestAbortedError)
          return requestAborted();
        const queueError = queueFailure(error, "image");
        if (queueError) return queueError;
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("Image");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "unavailable") return serviceUnavailable("Image");
      if (selected.kind === "insufficient-memory") {
        return resourceUnavailable(selected.error);
      }
      const backendBody = {
        ...generation,
        prompt: promptWithImageTuning(
          generation.prompt,
          { negative_prompt, seed, steps, cfg_scale },
          imageLoraFor(
            byId(selected.value.modelId),
            currentConfig.imageModelsDir,
            lora_strength,
          ),
        ),
      };
      const inference = beginInference("image", selected.value);
      const response = await proxyWithAdmission(
        selected.value.admission,
        "Image",
        request.signal,
        async () =>
          await proxyRequest(
            requestWithJsonBody(request, backendBody),
            factory.baseUrl("image", selected.value.admission.snapshot),
            undefined,
            imageGenerationResponseSchema,
            undefined,
            ctx.otel,
            undefined,
            (status) =>
              inference.finish({
                outcome: "error",
                httpStatus: status,
                source: "response_validation",
              }),
            undefined,
            selected.value.modelId,
            (status) => inference.observeUpstreamStatus(status),
            inference.context(),
          ),
        (terminal) => inference.finish(terminal),
      );
      return withServerTiming(response, inference.serverTiming());
    }

    if (route === "imageEdit") {
      if (request.method !== "POST") return methodNotAllowed("POST");
      const limit = 20 * 1024 * 1024;
      if (requestExceedsSizeLimit(request, limit)) {
        await discardDeclaredOversizedBody(request, limit);
        return payloadTooLarge(limit);
      }
      let form: FormData;
      try {
        const bytes = await readBoundedRequestBody(request, limit);
        const contentType = request.headers.get("content-type");
        if (!contentType?.toLowerCase().startsWith("multipart/form-data;")) {
          return badRequest("Expected multipart/form-data.", "content-type");
        }
        form = await new Request(request.url, {
          method: "POST",
          headers: { "content-type": contentType },
          body: requestBodyBuffer(bytes),
        }).formData();
      } catch (error) {
        return error instanceof PayloadTooLargeError
          ? payloadTooLarge(limit)
          : badRequest("Invalid multipart image edit request.");
      }
      const promptValue = form.get("prompt");
      const prompt = typeof promptValue === "string" ? promptValue.trim() : "";
      if (!prompt || prompt.length > 16_384)
        return badRequest(
          "prompt is required and must not exceed 16384 characters.",
          "prompt",
        );
      if (prompt.toLowerCase().includes("sd_cpp_extra_args"))
        return badRequest(
          "prompt must not contain sd_cpp_extra_args.",
          "prompt",
        );
      const modelValue = form.get("model");
      const modelId =
        typeof modelValue === "string" && modelValue
          ? modelValue
          : currentConfig.activeImageModel;
      if (!modelId) return notConfigured("Image");
      const spec = byId(modelId);
      if (!spec || spec.kind !== "image") return modelNotFound(modelId);
      if (!(await modelSupports(spec, "image-to-image")))
        return badRequest(
          `Model '${modelId}' does not support image-to-image edits.`,
          "model",
        );
      const images = [
        ...form.getAll("image[]"),
        ...form.getAll("image"),
      ].filter((value): value is File => value instanceof File);
      const maskValue = form.get("mask");
      const mask =
        maskValue === null
          ? undefined
          : maskValue instanceof File && maskValue.size > 0
            ? maskValue
            : undefined;
      if (maskValue !== null && !mask)
        return badRequest("mask must be a nonempty image file.", "mask");
      if (images.length < 1 || images.length > 4)
        return badRequest(
          "Provide between 1 and 4 image files using image or image[].",
          "image",
        );
      const nValue = form.get("n");
      const n = nValue === null ? 1 : Number(nValue);
      if (!Number.isInteger(n) || n < 1 || n > 10)
        return badRequest("n must be an integer from 1 to 10.", "n");
      const sizeValue = form.get("size");
      const size =
        typeof sizeValue === "string" && sizeValue ? sizeValue : undefined;
      if (size && !IMAGE_SIZES.some((option) => option === size))
        return badRequest(
          `size must be one of ${IMAGE_SIZES.join(", ")}.`,
          "size",
        );
      const responseFormat = form.get("response_format");
      if (
        responseFormat !== null &&
        responseFormat !== "url" &&
        responseFormat !== "b64_json"
      )
        return badRequest(
          "response_format must be url or b64_json.",
          "response_format",
        );
      const formNumber = (name: string) => {
        const value = form.get(name);
        return typeof value === "string" && value.trim()
          ? Number(value)
          : undefined;
      };
      const negativePrompt = form.get("negative_prompt");
      const tuning = imageTuningSchema.safeParse({
        negative_prompt:
          typeof negativePrompt === "string" && negativePrompt.trim()
            ? negativePrompt
            : undefined,
        seed: formNumber("seed"),
        steps: formNumber("steps"),
        cfg_scale: formNumber("cfg_scale"),
        strength: formNumber("strength"),
        lora_strength: formNumber("lora_strength"),
      });
      if (!tuning.success) {
        const param = String(tuning.error.issues[0]?.path[0] ?? "prompt");
        return badRequest(
          param === "strength"
            ? "strength must be between 0 and 1."
            : `${param} is invalid: ${tuning.error.issues[0]?.message ?? "unexpected value"}.`,
          param,
        );
      }
      const loraError = unusableLoraStrength(spec, tuning.data.lora_strength);
      if (loraError) return loraError;
      const outputFormatValue = form.get("output_format");
      const outputFormat =
        outputFormatValue === null ? "png" : outputFormatValue;
      if (outputFormat !== "png" && outputFormat !== "jpeg")
        return badRequest(
          "output_format must be png or jpeg.",
          "output_format",
        );
      for (const file of [...images, ...(mask ? [mask] : [])]) {
        const param = file === mask ? "mask" : "image";
        if (!file.size || file.size > 8 * 1024 * 1024)
          return badRequest(
            "Each image file must be between 1 byte and 8 MiB.",
            param,
          );
        const bytes = new Uint8Array(await file.arrayBuffer());
        const mime = imageMimeFromMagic(bytes);
        if (!mime)
          return badRequest(
            "Images must be PNG or JPEG by file signature.",
            param,
          );
        if (file.type && file.type !== mime)
          return badRequest(
            "Image content type does not match its file signature.",
            param,
          );
        const dimensions =
          mime === "image/png"
            ? validatePngImage(bytes)
            : validateJpegImage(bytes);
        if (
          !dimensions ||
          dimensions.width < 1 ||
          dimensions.height < 1 ||
          dimensions.width > 2048 ||
          dimensions.height > 2048 ||
          dimensions.width * dimensions.height > 4_194_304
        )
          return badRequest(
            "Images must have valid dimensions no larger than 2048x2048 (4.2 megapixels).",
            param,
          );
      }
      let selected;
      try {
        selected = await admitModelWithIdleRecovery(
          reconciler,
          "image",
          modelId,
          request.signal,
        );
      } catch (error) {
        if (error instanceof RuntimeRequestAbortedError)
          return requestAborted();
        const queueError = queueFailure(error, "image");
        if (queueError) return queueError;
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("Image");
      if (selected.kind === "model-not-found") return modelNotFound(modelId);
      if (selected.kind === "unavailable") return serviceUnavailable("Image");
      if (selected.kind === "insufficient-memory")
        return resourceUnavailable(selected.error);
      const backendForm = new FormData();
      const { lora_strength, ...editTuning } = tuning.data;
      backendForm.set(
        "prompt",
        promptWithImageTuning(
          prompt,
          editTuning,
          imageLoraFor(spec, currentConfig.imageModelsDir, lora_strength),
        ),
      );
      backendForm.set("n", String(n));
      backendForm.set("output_format", outputFormat);
      if (size) backendForm.set("size", size);
      for (const image of images)
        backendForm.append("image[]", image, image.name || "image.png");
      if (mask) backendForm.set("mask", mask, mask.name || "mask.png");
      const inference = beginInference("image", selected.value);
      const response = await proxyWithAdmission(
        selected.value.admission,
        "Image",
        request.signal,
        async () =>
          await proxyRequest(
            new Request(request.url, {
              method: "POST",
              headers: {
                authorization: request.headers.get("authorization") ?? "",
              },
              body: backendForm,
              signal: request.signal,
            }),
            factory.baseUrl("image", selected.value.admission.snapshot),
            undefined,
            imageGenerationResponseSchema,
            undefined,
            ctx.otel,
            undefined,
            (status) =>
              inference.finish({
                outcome: "error",
                httpStatus: status,
                source: "response_validation",
              }),
            undefined,
            selected.value.modelId,
            (status) => inference.observeUpstreamStatus(status),
            inference.context(),
          ),
        (terminal) => inference.finish(terminal),
      );
      return withServerTiming(response, inference.serverTiming());
    }

    if (route === "chatCompletion") {
      const parsed = await parseJsonRequest(
        request,
        chatCompletionRequestSchema,
      );
      if (!parsed.success) return parsed.response;
      const requestedModel = byId(parsed.data.model);
      const unsupportedToolParam = (() => {
        if ((parsed.data.tools?.length ?? 0) > 0) return "tools";
        if (
          parsed.data.tool_choice !== undefined &&
          parsed.data.tool_choice !== "none"
        )
          return "tool_choice";
        if (
          Array.isArray(parsed.data.functions) &&
          parsed.data.functions.length > 0
        )
          return "functions";
        if (
          parsed.data.function_call !== undefined &&
          parsed.data.function_call !== "none"
        )
          return "function_call";
        const messageIndex = parsed.data.messages.findIndex(
          (message) =>
            message.role === "tool" ||
            (message.role === "assistant" &&
              (message.tool_calls?.length ?? 0) > 0),
        );
        if (messageIndex < 0) return undefined;
        const message = parsed.data.messages[messageIndex];
        return message.role === "assistant"
          ? `messages[${messageIndex}].tool_calls`
          : `messages[${messageIndex}]`;
      })();
      const hasToolRequest = unsupportedToolParam !== undefined;
      if (
        hasToolRequest &&
        requestedModel &&
        !(await modelSupports(requestedModel, "tool-calling"))
      ) {
        return openAIErrorResponse(
          {
            message: `Model '${requestedModel.modelId}' does not support tool calling. Choose a tool-capable model from GET /v1/models.`,
            type: "invalid_request_error",
            param: unsupportedToolParam,
            code: "unsupported_model_capability",
          },
          400,
        );
      }
      if (requestedModel?.llmRuntime) {
        return badRequest(
          requestedModel.llmRuntime.capability === "decision-only"
            ? `Model '${requestedModel.modelId}' is a decision model. Send typed questions to POST /v1/systemone instead.`
            : `Model '${requestedModel.modelId}' supports embeddings only.`,
        );
      }
      const preparedRequest = prepareChatCompletionRequest(
        stripNoopToolFields(parsed.data),
      );
      const preparationStartedAt = performance.now();
      const structuredOutput = prepareStructuredOutput(
        parsed.data.response_format,
      );
      const preparationDurationMs = Math.max(
        0,
        performance.now() - preparationStartedAt,
      );
      if (structuredOutput.kind !== "none") {
        recordStructuredOutputPreparation(trace.getSpan(context.active()), {
          outcome:
            structuredOutput.kind === "ready"
              ? "supported"
              : structuredOutput.code === "invalid_json_schema"
                ? "invalid"
                : "unsupported",
          durationMs: preparationDurationMs,
          requestId,
        });
      }
      if (structuredOutput.kind === "rejected") {
        return structuredOutputRequestFailure(
          structuredOutput.message,
          structuredOutput.code,
        );
      }
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
        const queueError = queueFailure(error, "llm");
        if (queueError) return queueError;
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("LLM");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "unavailable") return serviceUnavailable("LLM");
      if (selected.kind === "insufficient-memory") {
        return resourceUnavailable(selected.error);
      }
      // Resolve the per-slot context only after admission is ready: the
      // supervisor learns the runtime's real context length once it starts.
      const resolveBackendRequest = () => {
        if (
          parsed.data.max_tokens !== undefined ||
          parsed.data.max_completion_tokens !== undefined
        ) {
          return preparedRequest;
        }
        const admittedSupervisor = selected.value.admission.supervisor;
        const supervisorProfile = admittedSupervisor.llmProfile?.();
        const admittedConfig = selected.value.admission.snapshot.config;
        const configuredContext = configuredLlmContextSize(
          requestedModel,
          supervisorProfile?.configCtxSize ?? admittedConfig.ctxSize,
          ctx.specs.gpuVramGb,
          supervisorProfile?.ctxSizeOverride,
          supervisorProfile?.ctxMode ?? admittedConfig.ctxMode,
        );
        const configuredSlots =
          supervisorProfile?.parallel === "auto"
            ? 4
            : Number(
                supervisorProfile?.parallel ??
                  selected.value.admission.snapshot.config.parallel,
              );
        const effectiveContextLength = Math.min(
          admittedSupervisor.resolvedContextLength?.() ??
            Math.floor(configuredContext / configuredSlots),
          requestedModel?.contextWindowTokens ?? Number.POSITIVE_INFINITY,
          supervisorProfile?.ctxSizeOverride ?? admittedConfig.ctxSize,
        );
        return {
          ...preparedRequest,
          // llama.cpp exposes tokenization only after applying its chat
          // template; this gateway has no cheap template-aware tokenizer.
          // Keep the default within the resolved per-slot context budget.
          max_tokens: Math.min(ctx.defaultMaxTokens, effectiveContextLength),
        };
      };
      const streaming = parsed.data.stream === true;
      const inference = beginInference("llm", selected.value, {
        streaming,
        ...(structuredOutput.kind === "ready"
          ? {
              structuredOutput: {
                preparationDurationMs,
                streaming,
              },
            }
          : {}),
      });
      const response = await proxyWithAdmission(
        selected.value.admission,
        "LLM",
        request.signal,
        async () =>
          await proxyRequest(
            requestWithJsonBody(request, resolveBackendRequest()),
            factory.baseUrl("llm", selected.value.admission.snapshot),
            undefined,
            chatCompletionResponseSchema,
            chatCompletionStreamEventSchema,
            ctx.otel,
            (value, status) => {
              if (
                typeof value === "object" &&
                value !== null &&
                "error" in value
              ) {
                inference.finish({
                  outcome: "error",
                  httpStatus: status,
                  source: "upstream_error_event",
                });
              } else inference.observeValidatedBackendMetadata(value);
            },
            (status) =>
              inference.finish({
                outcome: "error",
                httpStatus: status,
                source: "response_validation",
              }),
            !streaming && structuredOutput.kind === "ready"
              ? {
                  validator: structuredOutput.validator,
                  onValidation: (result) =>
                    inference.observeStructuredOutputValidation(result),
                }
              : undefined,
            selected.value.modelId,
            (status) => inference.observeUpstreamStatus(status),
            inference.context(),
          ),
        (terminal) => inference.finish(terminal),
      );
      return withServerTiming(response, inference.serverTiming());
    }

    if (route === "embeddings") {
      const parsed = await parseJsonRequest(request, embeddingsRequestSchema);
      if (!parsed.success) return parsed.response;
      const requestedModel = byId(parsed.data.model);
      if (requestedModel && isDecisionModel(requestedModel)) {
        return badRequest(
          `Model '${requestedModel.modelId}' is a decision model. Send typed questions to POST /v1/systemone instead.`,
        );
      }
      const dimensions =
        requestedModel?.llmRuntime?.capability === "embedding-only"
          ? requestedModel.llmRuntime.dimensions
          : undefined;
      if (
        dimensions &&
        parsed.data.dimensions !== undefined &&
        (parsed.data.dimensions < dimensions.minimum ||
          parsed.data.dimensions > dimensions.maximum)
      ) {
        return badRequest(
          dimensions.minimum === dimensions.maximum
            ? `Model '${parsed.data.model}' supports ${dimensions.minimum} dimensions.`
            : `Model '${parsed.data.model}' supports dimensions from ${dimensions.minimum} through ${dimensions.maximum}.`,
        );
      }
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
        const queueError = queueFailure(error, "llm");
        if (queueError) return queueError;
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("LLM");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "unavailable") return serviceUnavailable("LLM");
      if (selected.kind === "insufficient-memory") {
        return resourceUnavailable(selected.error);
      }
      const inference = beginInference("llm", selected.value);
      const response = await proxyWithAdmission(
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
            (value) => inference.observeValidatedBackendMetadata(value),
            (status) =>
              inference.finish({
                outcome: "error",
                httpStatus: status,
                source: "response_validation",
              }),
            undefined,
            selected.value.modelId,
            (status) => inference.observeUpstreamStatus(status),
            inference.context(),
          ),
        (terminal) => inference.finish(terminal),
      );
      return withServerTiming(response, inference.serverTiming());
    }

    if (route === "decision") {
      const parsed = await parseJsonRequest(request, decisionRequestSchema);
      if (!parsed.success) return parsed.response;
      const requestedModel = byId(parsed.data.model);
      if (requestedModel && !isDecisionModel(requestedModel)) {
        return openAIErrorResponse(
          {
            message: `Model '${requestedModel.modelId}' is not a decision model. Choose a model whose architecture.output_modalities include "decisions" from GET /v1/models.`,
            type: "invalid_request_error",
            param: "model",
            code: "unsupported_model_capability",
          },
          400,
        );
      }
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
        const queueError = queueFailure(error, "llm");
        if (queueError) return queueError;
        throw error;
      }
      if (selected.kind === "not-configured") return notConfigured("LLM");
      if (selected.kind === "model-not-found") {
        return modelNotFound(parsed.data.model ?? "");
      }
      if (selected.kind === "unavailable") return serviceUnavailable("LLM");
      if (selected.kind === "insufficient-memory") {
        return resourceUnavailable(selected.error);
      }
      const inference = beginInference("llm", selected.value);
      const body = prepareDecisionRequest(parsed.data);
      const response = await proxyWithAdmission(
        selected.value.admission,
        "LLM",
        request.signal,
        async () =>
          await proxyRequest(
            requestWithJsonBody(request, body),
            factory.baseUrl("llm", selected.value.admission.snapshot),
            undefined,
            decisionResponseSchema,
            undefined,
            ctx.otel,
            (value) => {
              // Decisions report prompt tokens only; map them to usage.
              const usage = decisionResponseSchema.safeParse(value).data?.usage;
              if (usage) {
                inference.observeValidatedBackendMetadata({
                  usage: {
                    prompt_tokens: usage.input_tokens,
                    completion_tokens: usage.output_tokens,
                    total_tokens: usage.input_tokens + usage.output_tokens,
                  },
                });
              }
            },
            (status) =>
              inference.finish({
                outcome: "error",
                httpStatus: status,
                source: "response_validation",
              }),
            undefined,
            selected.value.modelId,
            (status) => inference.observeUpstreamStatus(status),
            inference.context(),
          ),
        (terminal) => inference.finish(terminal),
      );
      return withServerTiming(response, inference.serverTiming());
    }

    if (route === "models" || route === "modelRetrieve") {
      const modelId =
        route === "modelRetrieve" ? servedModelIdFromPath(pathname) : undefined;
      if (route === "modelRetrieve") {
        if (request.method !== "GET") return methodNotAllowed("GET");
        if (modelId === undefined) return routeNotFound();
      }
      const data = await listServedModels(
        currentConfig,
        authorization.kind === "authorized"
          ? authorization.principal.permissions
          : undefined,
        {
          modelId,
          enabled: {
            llm: input.llm !== false,
            stt: input.stt ?? currentConfig.selectedSttModels.length > 0,
            tts: input.tts ?? currentConfig.selectedTtsModels.length > 0,
            image: input.image ?? currentConfig.selectedImageModels.length > 0,
            video: input.video ?? currentConfig.selectedVideoModels.length > 0,
          },
          ctxSizeOverride: launchOverrides.ctxSize,
          llmModelFile: launchOverrides.llmModelFile,
          pinnedContextLength: supervisors
            .get("llm")
            ?.resolvedContextLength?.(),
          pinnedLaunchPlan: supervisors.get("llm")?.resolvedLlmLaunchPlan?.(),
          llmProfile: supervisors.get("llm")?.llmProfile?.(),
          parallel: currentConfig.parallel,
          memoryGb: ctx.specs.gpuVramGb,
          memoryInputsForModel: async (spec) => {
            const inputs = await collectModelMemoryInputs({
              catalog: [spec],
              installations: await inspectCatalogInstallations(currentConfig, [
                spec,
              ]),
              directoryFor: (model) =>
                directoryForModel(currentConfig, model.kind),
              observed: observedMemory,
              readGeometry: readCachedLlmKvGeometry,
            });
            return inputs.get(spec.modelId) ?? {};
          },
          capabilitiesForModel: async (spec) =>
            (
              await resolveCapabilitiesForModel(
                spec,
                currentConfig,
                runtimeCapabilities,
              )
            ).capabilities,
          kvGeometryForModel: async (id) => {
            const spec = byId(id);
            const modelFile =
              launchOverrides.llmModelFile ??
              (spec ? primaryArtifact(spec).filename : `${id}.gguf`);
            const path = join(currentConfig.llmModelsDir, modelFile);
            return readCachedLlmKvGeometry(path);
          },
          trainingContextLengthForModel: async (id) => {
            const spec = byId(id);
            const modelFile =
              launchOverrides.llmModelFile ??
              (spec ? primaryArtifact(spec).filename : `${id}.gguf`);
            return readCachedLlmTrainingContextLength(
              join(currentConfig.llmModelsDir, modelFile),
            );
          },
        },
      );
      if (route === "modelRetrieve") {
        // Unlisted and unauthorized IDs share one answer, so a key cannot
        // probe for models it could not list.
        const [model] = data;
        return model ? Response.json(model) : modelNotFound(modelId ?? "");
      }
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
    fetch: async (request, listener) => {
      const start = performance.now();
      const { pathname } = new URL(request.url);
      // Bun resets sockets idle for 10 s by default; cold loads, queue waits,
      // and LLM prefill can be silent far longer before the first byte.
      if (gatewayRequestDisablesIdleTimeout(pathname))
        listener.timeout(request, 0);
      const method = request.method;
      const requestId = `lbreq_${crypto.randomUUID()}`;
      const requestAuth = gatewayRequestAuth(request);
      const route = selectGatewayRoute(pathname);
      const playground = playgroundResponse(request, pathname);
      const requirement: AuthorizationRequirement = playground
        ? { kind: "public" }
        : gatewayAuthorizationRequirement({ route, method, authRequired });
      const parent = ctx.otel.extract(request.headers);
      const span = ctx.otel.startSpan(
        serverSpanName(method, pathname),
        serverSpanOptions(method, pathname),
        parent,
      );
      span.setAttribute("localbase.request_id", requestId);
      if (canonicalGatewayHttpRoute(pathname) === "unmatched-route") {
        const hint = unmatchedRouteHint(pathname);
        span.setAttribute("localbase.route.family", hint.family);
        span.setAttribute("localbase.route.segments", hint.segments);
      }
      const uiRequest = isUiAccessPath(pathname);
      if (method === "OPTIONS" && !uiRequest) {
        span.setAttribute("http.response.status_code", 403);
        span.end();
        return forbidden();
      }

      const responseAbortController = new AbortController();
      const lifecycleSignal = AbortSignal.any([
        request.signal,
        responseAbortController.signal,
      ]);
      const lifecycleRequest = withLifecycleSignal(request, lifecycleSignal);
      let response: Response;
      try {
        response = await context.with(
          trace.setSpan(parent, span),
          async () =>
            await handleRequest(
              lifecycleRequest,
              pathname,
              requestId,
              start,
              requestAuth,
              requirement,
              playground,
            ),
        );
      } catch (err) {
        if (err instanceof ModelInstallConsentError) {
          response = modelInstallConsentResponse(err);
        } else {
          const error =
            err instanceof Error ? err : new Error("Unknown request failure");
          const parsedMethod =
            logHttpMetadataSchema.shape.method.safeParse(method);
          ctx.logger.event({
            severity: "error",
            eventName: "http.handler-failed",
            category: "http",
            component: "gateway",
            runtime: "gateway",
            message: "Gateway request handler failed.",
            requestId,
            trace: spanCorrelation(span),
            ...(parsedMethod.success
              ? {
                  http: {
                    method: parsedMethod.data,
                    path: pathname,
                    status: 500,
                    durationMs: performance.now() - start,
                  },
                }
              : {}),
            error: { type: error.name, message: error.message },
          });
          response = internalGatewayFailure();
        }
      }

      const headers = new Headers(response.headers);
      if (!uiRequest) {
        headers.set("Access-Control-Allow-Origin", "*");
        headers.set(
          "Access-Control-Allow-Methods",
          "GET, POST, PUT, DELETE, OPTIONS",
        );
        headers.set(
          "Access-Control-Allow-Headers",
          "Content-Type, Authorization, x-api-key",
        );
      } else {
        for (const name of [...headers.keys()]) {
          if (name.startsWith("access-control-")) headers.delete(name);
        }
        headers.set("cache-control", "no-store");
      }
      headers.set("x-localbase-request-id", requestId);

      const durationMs = performance.now() - start;
      if (!isEventStream(response)) {
        const selectedRoute = selectGatewayRoute(pathname);
        const inferenceTiming =
          selectedRoute === "chatCompletion" ||
          selectedRoute === "embeddings" ||
          selectedRoute === "decision" ||
          selectedRoute === "transcription" ||
          selectedRoute === "speechGeneration" ||
          selectedRoute === "imageGeneration" ||
          selectedRoute === "imageEdit"
            ? headers.get("server-timing")
            : undefined;
        headers.set(
          "server-timing",
          `${inferenceTiming ? `${inferenceTiming}, ` : ""}localbase;dur=${durationMs.toFixed(2)}`,
        );
      }
      const body = gatewayRouteServesBufferedJson(pathname)
        ? await compressJsonResponse(request, response, headers)
        : response.body;
      const corsResponse = new Response(body, {
        status: response.status,
        statusText: response.statusText,
        headers,
      });
      span.setAttribute("http.response.status_code", corsResponse.status);
      if (corsResponse.status >= 500) {
        span.setStatus({ code: SpanStatusCode.ERROR });
      }
      const settle = (outcome: InferenceOutcome) => {
        const settledDurationMs = performance.now() - start;
        span.setAttribute(
          "localbase.http.response.duration_ms",
          settledDurationMs,
        );
        if (outcome !== "completed" || corsResponse.status >= 500) {
          span.setStatus({ code: SpanStatusCode.ERROR });
        }
        const auth = requestAuth.telemetry(requirement.kind !== "public");
        const safeAuth = redactLogAttributes({
          auth_outcome: auth.outcome,
          ...(auth.outcome === "authenticated"
            ? { auth_principal_id: auth.principalId }
            : {}),
          ...(auth.outcome === "authenticated" && auth.principalName
            ? { auth_principal_name: auth.principalName }
            : {}),
          ...(auth.outcome === "authenticated"
            ? { auth_source: auth.source }
            : {}),
        });
        const spanAuthNames: Record<string, string> = {
          auth_outcome: "localbase.auth.outcome",
          auth_principal_id: "localbase.auth.principal_id",
          auth_principal_name: "localbase.auth.principal_name",
          auth_source: "localbase.auth.source",
        };
        for (const [name, value] of Object.entries(safeAuth ?? {})) {
          span.setAttribute(spanAuthNames[name], value);
        }
        ctx.logger.request({
          method,
          path: pathname,
          status: corsResponse.status,
          durationMs: settledDurationMs,
          requestId,
          trace: spanCorrelation(span),
          auth,
        });
        span.end();
      };
      return withResponseLease(
        corsResponse,
        () => {},
        () => {
          responseAbortController.abort();
        },
        lifecycleSignal,
        (terminal) => settle(terminal.outcome),
      );
    },
  });

  if (serviceId && serviceToken && readinessSnapshot().status === "ready") {
    acknowledgeStaticConfiguration(
      ctx.database,
      config.root,
      startupStaticConfig,
    );
  }
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
      ttsEnabled: enabled.tts,
      imageEnabled: enabled.image,
    },
  });
  memoryPressureMonitor.start();
  let lastCatalogFeedError: string | null = null;
  const catalogFeed = startCatalogFeedScheduler(config.root, {
    onResult: (result) => {
      if (result.outcome === "updated") {
        lastCatalogFeedError = null;
        ctx.logger.event({
          severity: "info",
          eventName: "catalog_feed.updated",
          category: "gateway",
          component: "catalog-feed",
          runtime: "gateway",
          message: "Loaded a new remote catalog feed.",
          attributes: { url: (result.url ?? "").slice(0, 512) },
        });
      } else if (result.outcome === "failed") {
        if (result.error === lastCatalogFeedError) return;
        lastCatalogFeedError = result.error;
        ctx.logger.event({
          severity: "warn",
          eventName: "catalog_feed.refresh_failed",
          category: "gateway",
          component: "catalog-feed",
          runtime: "gateway",
          message:
            "The remote catalog feed could not be refreshed; the last good copy stays in use.",
          attributes: {
            url: (result.url ?? "").slice(0, 512),
            error: (result.error ?? "").slice(0, 512),
          },
        });
      } else {
        lastCatalogFeedError = null;
      }
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
        observationStop.abort();
        reconciler.closeQueues();
        server.stop(true);
        try {
          await catalogFeed.stop();
          await memoryPressureMonitor.stop();
          await videoJobs.shutdown();
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
