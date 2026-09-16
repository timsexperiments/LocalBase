import { z } from "zod";
import {
  videoGenerationInputSchema,
  type VideoGenerationInput,
} from "./video-input";

export type { VideoGenerationInput } from "./video-input";

// DNS names are not accepted here. The configured backend must be pinned to a
// numeric loopback address so a later DNS change cannot redirect video traffic.
const LOOPBACK_LITERALS = new Set(["127.0.0.1", "[::1]"]);
const MAX_JOB_ID_LENGTH = 128;
const DEFAULT_MAX_RESPONSE_BYTES = 32 * 1024 * 1024;
const DEFAULT_MAX_MEDIA_BYTES = 23 * 1024 * 1024;
const COMPLETED_JOB_ENVELOPE_BYTES = 1024;

const videoOutputFormatSchema = z.enum(["webm", "webp", "avi"]);
const jobStatusSchema = z.enum([
  "queued",
  "generating",
  "completed",
  "failed",
  "cancelled",
]);
const jobIdSchema = z
  .string()
  .min(1)
  .max(MAX_JOB_ID_LENGTH)
  .regex(/^[A-Za-z0-9_-]+$/);

const capabilitiesSchema = z
  .object({
    supported_modes: z.array(z.string()),
    output_formats_by_mode: z
      .object({ vid_gen: z.array(videoOutputFormatSchema).optional() })
      .passthrough(),
  })
  .passthrough();

const jobEnvelopeSchema = z
  .object({
    id: jobIdSchema,
    kind: z.literal("vid_gen"),
    status: jobStatusSchema,
    created: z.number().int().nonnegative(),
    started: z.number().int().nonnegative().nullable().optional(),
    completed: z.number().int().nonnegative().nullable().optional(),
    queue_position: z.number().int().nonnegative().optional(),
    result: z.unknown().nullable().optional(),
    error: z.unknown().nullable().optional(),
  })
  .passthrough();

const completedResultBaseSchema = z.object({
  fps: z.number().int().positive(),
  frame_count: z.number().int().positive(),
  b64_json: z.string().min(1),
});

const completedResultSchema = z.discriminatedUnion("output_format", [
  completedResultBaseSchema
    .extend({
      output_format: z.literal("webm"),
      mime_type: z.literal("video/webm"),
    })
    .strict(),
  completedResultBaseSchema
    .extend({
      output_format: z.literal("webp"),
      mime_type: z.literal("image/webp"),
    })
    .strict(),
  completedResultBaseSchema
    .extend({
      output_format: z.literal("avi"),
      mime_type: z.literal("video/x-msvideo"),
    })
    .strict(),
]);

export type VideoOutputFormat = z.infer<typeof videoOutputFormatSchema>;

export type VideoCapabilities = {
  available: boolean;
  outputFormats: VideoOutputFormat[];
};

export type VideoSubmission = {
  id: string;
  status: "queued" | "generating";
};

export type VideoJob =
  | {
      id: string;
      status: "queued" | "generating";
      queuePosition: number | undefined;
    }
  | {
      id: string;
      status: "completed";
      media: {
        bytes: Uint8Array;
        mimeType: string;
        outputFormat: VideoOutputFormat;
        fps: number;
        frameCount: number;
      };
    }
  | {
      id: string;
      status: "failed" | "cancelled";
    };

export type StableDiffusionVideoClient = {
  getCapabilities(options?: {
    signal?: AbortSignal;
  }): Promise<VideoCapabilities>;
  submitVideo(options: {
    input: VideoGenerationInput;
    signal?: AbortSignal;
  }): Promise<VideoSubmission>;
  getJob(options: { id: string; signal?: AbortSignal }): Promise<VideoJob>;
  cancelJob(options: { id: string; signal?: AbortSignal }): Promise<VideoJob>;
};

export class StableDiffusionVideoClientError extends Error {
  constructor(
    readonly code:
      | "invalid_base_url"
      | "invalid_input"
      | "invalid_job_id"
      | "backend_response_too_large"
      | "backend_media_too_large"
      | "backend_response_invalid"
      | "backend_request_failed",
    message: string,
    readonly status: number | undefined = undefined,
  ) {
    super(message);
    this.name = "StableDiffusionVideoClientError";
  }
}

type ClientOptions = {
  baseUrl: string;
  fetch?: typeof fetch;
  maxResponseBytes?: number;
  maxMediaBytes?: number;
};

export function createStableDiffusionVideoClient(
  options: ClientOptions,
): StableDiffusionVideoClient {
  const baseUrl = parseLocalBaseUrl(options.baseUrl);
  const maxResponseBytes = parseByteLimit(
    options.maxResponseBytes,
    DEFAULT_MAX_RESPONSE_BYTES,
    "maxResponseBytes",
  );
  const maxMediaBytes = parseByteLimit(
    options.maxMediaBytes,
    DEFAULT_MAX_MEDIA_BYTES,
    "maxMediaBytes",
  );
  ensureResponseBudget({ maxResponseBytes, maxMediaBytes });
  const request = options.fetch ?? fetch;

  async function getCapabilities(options: { signal?: AbortSignal } = {}) {
    const response = await request(endpointUrl(baseUrl, "capabilities"), {
      signal: options.signal,
      redirect: "error",
    });
    const payload = await parseJsonResponse(response, maxResponseBytes);
    const parsed = capabilitiesSchema.safeParse(payload);
    if (!parsed.success) throw invalidBackendResponse();
    const available = parsed.data.supported_modes.includes("vid_gen");
    const outputFormats = parsed.data.output_formats_by_mode.vid_gen ?? [];
    if (available && outputFormats.length === 0) throw invalidBackendResponse();
    return { available, outputFormats };
  }

  async function submitVideo({
    input,
    signal,
  }: {
    input: VideoGenerationInput;
    signal?: AbortSignal;
  }): Promise<VideoSubmission> {
    const parsedInput = videoGenerationInputSchema.safeParse(input);
    if (!parsedInput.success) {
      throw new StableDiffusionVideoClientError(
        "invalid_input",
        "Video input is invalid.",
      );
    }
    const response = await request(endpointUrl(baseUrl, "vid_gen"), {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(toBackendVideoInput(parsedInput.data)),
      signal,
      redirect: "error",
    });
    const job = await parseJobResponse(response, maxResponseBytes);
    if (job.status !== "queued" && job.status !== "generating") {
      throw invalidBackendResponse();
    }
    return { id: job.id, status: job.status };
  }

  async function getJob({
    id,
    signal,
  }: {
    id: string;
    signal?: AbortSignal;
  }): Promise<VideoJob> {
    return parseVideoJob({
      response: await request(jobUrl(baseUrl, id), {
        signal,
        redirect: "error",
      }),
      requestedId: id,
      maxResponseBytes,
      maxMediaBytes,
    });
  }

  async function cancelJob({
    id,
    signal,
  }: {
    id: string;
    signal?: AbortSignal;
  }): Promise<VideoJob> {
    return parseVideoJob({
      response: await request(cancelUrl(baseUrl, id), {
        method: "POST",
        signal,
        redirect: "error",
      }),
      requestedId: id,
      maxResponseBytes,
      maxMediaBytes,
    });
  }

  return { getCapabilities, submitVideo, getJob, cancelJob };
}

function parseLocalBaseUrl(value: string): URL {
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new StableDiffusionVideoClientError(
      "invalid_base_url",
      "Video backend URL must be a localhost HTTP URL.",
    );
  }
  if (
    parsed.protocol !== "http:" ||
    !LOOPBACK_LITERALS.has(parsed.hostname) ||
    parsed.username !== "" ||
    parsed.password !== "" ||
    parsed.search !== "" ||
    parsed.hash !== "" ||
    parsed.pathname !== "/"
  ) {
    throw new StableDiffusionVideoClientError(
      "invalid_base_url",
      "Video backend URL must use a numeric loopback HTTP URL without a path.",
    );
  }
  return parsed;
}

function parseByteLimit(
  value: number | undefined,
  fallback: number,
  name: string,
) {
  const parsed = value ?? fallback;
  if (!Number.isSafeInteger(parsed) || parsed < 1) {
    throw new RangeError(`${name} must be a positive safe integer.`);
  }
  return parsed;
}

function endpointUrl(baseUrl: URL, endpoint: "capabilities" | "vid_gen") {
  return new URL(`/sdcpp/v1/${endpoint}`, baseUrl);
}

function jobUrl(baseUrl: URL, id: string) {
  return new URL(`/sdcpp/v1/jobs/${validatedJobId(id)}`, baseUrl);
}

function cancelUrl(baseUrl: URL, id: string) {
  return new URL(`/sdcpp/v1/jobs/${validatedJobId(id)}/cancel`, baseUrl);
}

function validatedJobId(value: string) {
  const parsed = jobIdSchema.safeParse(value);
  if (parsed.success) return encodeURIComponent(parsed.data);
  throw new StableDiffusionVideoClientError(
    "invalid_job_id",
    "Video job ID is invalid.",
  );
}

function toBackendVideoInput(input: VideoGenerationInput) {
  return {
    prompt: input.prompt,
    ...(input.negativePrompt === undefined
      ? {}
      : { negative_prompt: input.negativePrompt }),
    ...(input.width === undefined ? {} : { width: input.width }),
    ...(input.height === undefined ? {} : { height: input.height }),
    ...(input.videoFrames === undefined
      ? {}
      : { video_frames: input.videoFrames }),
    ...(input.fps === undefined ? {} : { fps: input.fps }),
    ...(input.seed === undefined ? {} : { seed: input.seed }),
    ...(input.outputFormat === undefined
      ? {}
      : { output_format: input.outputFormat }),
    ...(input.generation === undefined
      ? {}
      : {
          sample_params: {
            sample_method: input.generation.sampler,
            sample_steps: input.generation.steps,
            flow_shift: input.generation.flowShift,
            guidance: { txt_cfg: input.generation.cfgScale },
          },
        }),
    ...(input.kind === "speech"
      ? {
          init_image: input.portrait.data,
          audio: { format: "wav", data: input.audio.data },
        }
      : {}),
  };
}

async function parseJobResponse(response: Response, maxResponseBytes: number) {
  const payload = await parseJsonResponse(response, maxResponseBytes);
  const parsed = jobEnvelopeSchema.safeParse(payload);
  if (!parsed.success) throw invalidBackendResponse();
  return parsed.data;
}

async function parseVideoJob({
  response,
  requestedId,
  maxResponseBytes,
  maxMediaBytes,
}: {
  response: Response;
  requestedId: string;
  maxResponseBytes: number;
  maxMediaBytes: number;
}): Promise<VideoJob> {
  const requestedEncodedId = validatedJobId(requestedId);
  const job = await parseJobResponse(response, maxResponseBytes);
  if (job.id !== requestedEncodedId) throw invalidBackendResponse();

  switch (job.status) {
    case "queued":
    case "generating":
      return {
        id: job.id,
        status: job.status,
        queuePosition: job.queue_position,
      };
    case "completed":
      return {
        id: job.id,
        status: "completed",
        media: parseCompletedMedia(job.result, maxMediaBytes),
      };
    case "failed":
    case "cancelled":
      return {
        id: job.id,
        status: job.status,
      };
    default: {
      const exhaustive: never = job.status;
      return exhaustive;
    }
  }
}

function parseCompletedMedia(payload: unknown, maxMediaBytes: number) {
  const parsed = completedResultSchema.safeParse(payload);
  if (!parsed.success) throw invalidBackendResponse();
  const maxEncodedBytes = encodedMediaByteLimit(maxMediaBytes);
  if (parsed.data.b64_json.length > maxEncodedBytes) {
    throw new StableDiffusionVideoClientError(
      "backend_media_too_large",
      "Video backend media exceeds the configured limit.",
    );
  }
  let bytes: Uint8Array;
  try {
    bytes = Uint8Array.fromBase64(parsed.data.b64_json);
  } catch {
    throw invalidBackendResponse();
  }
  if (bytes.byteLength > maxMediaBytes) {
    throw new StableDiffusionVideoClientError(
      "backend_media_too_large",
      "Video backend media exceeds the configured limit.",
    );
  }
  if (!hasContainerSignature(parsed.data.output_format, bytes)) {
    throw invalidBackendResponse();
  }
  return {
    bytes,
    mimeType: parsed.data.mime_type,
    outputFormat: parsed.data.output_format,
    fps: parsed.data.fps,
    frameCount: parsed.data.frame_count,
  };
}

async function parseJsonResponse(response: Response, maxBytes: number) {
  if (!response.ok) {
    await response.body?.cancel();
    throw new StableDiffusionVideoClientError(
      "backend_request_failed",
      `Video backend request failed with HTTP ${response.status}.`,
      response.status,
    );
  }
  const bytes = await readBoundedBytes(response, maxBytes);
  try {
    const parsed: unknown = JSON.parse(new TextDecoder().decode(bytes));
    return parsed;
  } catch {
    throw invalidBackendResponse();
  }
}

function encodedMediaByteLimit(mediaBytes: number) {
  return Math.ceil(mediaBytes / 3) * 4;
}

function ensureResponseBudget({
  maxResponseBytes,
  maxMediaBytes,
}: {
  maxResponseBytes: number;
  maxMediaBytes: number;
}) {
  const required =
    encodedMediaByteLimit(maxMediaBytes) + COMPLETED_JOB_ENVELOPE_BYTES;
  if (maxResponseBytes < required) {
    throw new RangeError(
      "maxResponseBytes must leave room for a padded video payload and its job envelope.",
    );
  }
}

function hasContainerSignature(format: VideoOutputFormat, bytes: Uint8Array) {
  switch (format) {
    case "webm":
      return startsWith(bytes, [0x1a, 0x45, 0xdf, 0xa3]);
    case "webp":
      return (
        startsWith(bytes, [0x52, 0x49, 0x46, 0x46]) &&
        matchesAt(bytes, 8, [0x57, 0x45, 0x42, 0x50])
      );
    case "avi":
      return (
        startsWith(bytes, [0x52, 0x49, 0x46, 0x46]) &&
        matchesAt(bytes, 8, [0x41, 0x56, 0x49, 0x20])
      );
    default: {
      const exhaustive: never = format;
      return exhaustive;
    }
  }
}

function startsWith(bytes: Uint8Array, prefix: number[]) {
  return matchesAt(bytes, 0, prefix);
}

function matchesAt(bytes: Uint8Array, offset: number, expected: number[]) {
  return expected.every((value, index) => bytes[offset + index] === value);
}

async function readBoundedBytes(response: Response, maxBytes: number) {
  const contentLength = response.headers.get("content-length");
  if (contentLength !== null) {
    const parsed = Number(contentLength);
    if (Number.isSafeInteger(parsed) && parsed > maxBytes) {
      await response.body?.cancel();
      throw new StableDiffusionVideoClientError(
        "backend_response_too_large",
        "Video backend response exceeds the configured limit.",
      );
    }
  }
  if (response.body === null) throw invalidBackendResponse();
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      total += chunk.value.byteLength;
      if (total > maxBytes) {
        await reader.cancel();
        throw new StableDiffusionVideoClientError(
          "backend_response_too_large",
          "Video backend response exceeds the configured limit.",
        );
      }
      chunks.push(chunk.value);
    }
  } finally {
    reader.releaseLock();
  }
  const result = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return result;
}

function invalidBackendResponse() {
  return new StableDiffusionVideoClientError(
    "backend_response_invalid",
    "Video backend returned an invalid response.",
  );
}
