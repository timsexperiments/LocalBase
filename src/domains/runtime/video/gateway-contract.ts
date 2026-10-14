import { z } from "zod";
import { RuntimeMemoryAdmissionError } from "../memory-controller";
import { ModelInstallConsentError } from "../startup-preflight";
import type { ModelSpec } from "../../../catalog";
import {
  videoConditioningInputSchema,
  videoGenerationInputSchema,
  type VideoGenerationInput,
} from "./video-input";
import type { VideoJob } from "./video-job-manager";
import {
  experimentalModelOptInRequired,
  ModelHardwareIneligibleError,
} from "../../models/model-eligibility";
import { ModelManagementError } from "../../models/model-management-contract";

export const videoCreateRequestSchema = z
  .object({
    model: z.string().min(1),
    prompt: z.string().min(1).max(16_384),
    negative_prompt: z.string().max(16_384).optional(),
    width: z.number().int().positive(),
    height: z.number().int().positive(),
    frames: z.number().int().positive(),
    fps: z.number().int().positive(),
    input: videoConditioningInputSchema,
    input_reference: z.string().max(6_000_000).optional(),
  })
  .strict()
  .superRefine((request, ctx) => {
    if (
      request.input_reference !== undefined &&
      request.input.kind !== "text"
    ) {
      ctx.addIssue({
        code: "custom",
        message: "input_reference can only be combined with input.kind=text.",
        path: ["input_reference"],
      });
    }
  });

export type VideoCreateRequest = z.infer<typeof videoCreateRequestSchema>;

const videoJobResponseBaseSchema = z
  .object({
    object: z.literal("localbase.video.job"),
    id: z.string().uuid(),
    created_at: z.number().int().nonnegative(),
  })
  .strict();

export const videoJobResponseSchema = z.discriminatedUnion("status", [
  videoJobResponseBaseSchema.extend({
    status: z.enum(["queued", "in_progress"]),
  }),
  videoJobResponseBaseSchema.extend({
    status: z.literal("completed"),
    completed_at: z.number().int().nonnegative(),
    content_type: z.string().min(1),
    bytes: z.number().int().nonnegative(),
    fps: z.number().int().positive(),
    frames: z.number().int().positive(),
  }),
  videoJobResponseBaseSchema.extend({
    status: z.literal("failed"),
    completed_at: z.number().int().nonnegative(),
    error: z
      .object({
        code: z.enum([
          "video_generation_failed",
          "insufficient_memory",
          "model_install_consent_required",
          "model_hardware_ineligible",
          "experimental_model_not_allowed",
        ]),
        message: z.string().min(1).optional(),
      })
      .strict(),
  }),
  videoJobResponseBaseSchema.extend({
    status: z.literal("cancelled"),
    completed_at: z.number().int().nonnegative(),
    cancellation_reason: z.enum([
      "backend",
      "cancelled",
      "deadline",
      "shutdown",
    ]),
  }),
]);

export type VideoJobResponse = z.infer<typeof videoJobResponseSchema>;

export function qualifiedVideoInput(
  request: VideoCreateRequest,
  model: ModelSpec,
): VideoGenerationInput | undefined {
  if (model.kind !== "video" || !model.videoRuntime) return undefined;
  const qualification = model.videoRuntime.qualification;
  if (
    request.width !== qualification.maxWidth ||
    request.height !== qualification.maxHeight ||
    request.frames !== qualification.maxFrames ||
    request.fps !== qualification.fps
  ) {
    return undefined;
  }
  if (
    (request.input_reference !== undefined &&
      model.videoRuntime.mode !== "ti2v") ||
    (model.videoRuntime.mode === "t2v" && request.input.kind !== "text") ||
    (model.videoRuntime.mode === "s2v" && request.input.kind !== "speech") ||
    (model.videoRuntime.mode === "ti2v" &&
      request.input.kind !== "text" &&
      request.input.kind !== "image")
  ) {
    return undefined;
  }
  const candidate = {
    kind: request.input_reference !== undefined ? "image" : request.input.kind,
    prompt: request.prompt,
    ...(request.negative_prompt === undefined
      ? {}
      : { negativePrompt: request.negative_prompt }),
    width: qualification.maxWidth,
    height: qualification.maxHeight,
    videoFrames: qualification.maxFrames,
    fps: qualification.fps,
    seed: qualification.generation.seed,
    outputFormat: "avi",
    generation: {
      sampler: qualification.generation.sampler,
      scheduler: qualification.generation.scheduler,
      steps: qualification.generation.steps,
      cfgScale: qualification.generation.cfgScale,
      flowShift: qualification.generation.flowShift,
    },
    ...(request.input_reference !== undefined
      ? { image: parseImageReference(request.input_reference) }
      : request.input.kind === "speech"
        ? { portrait: request.input.portrait, audio: request.input.audio }
        : request.input.kind === "image"
          ? { image: request.input.image }
          : {}),
  };
  const parsed = videoGenerationInputSchema.safeParse(candidate);
  return parsed.success ? parsed.data : undefined;
}

function parseImageReference(value: string) {
  const match = /^data:image\/png;base64,([A-Za-z0-9+/]+=*)$/.exec(value);
  const data = match?.[1] ?? value;
  return { format: "png" as const, data };
}

function unixSeconds(milliseconds: number): number {
  return Math.floor(milliseconds / 1_000);
}

export function projectVideoJob(job: VideoJob): VideoJobResponse {
  switch (job.state) {
    case "queued":
    case "in_progress":
      return videoJobResponseSchema.parse({
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: unixSeconds(job.createdAtMs),
      });
    case "completed":
      return videoJobResponseSchema.parse({
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: unixSeconds(job.createdAtMs),
        completed_at: unixSeconds(job.terminalAtMs),
        content_type: job.artifact.mimeType,
        bytes: job.artifact.byteLength,
        fps: job.artifact.fps,
        frames: job.artifact.frameCount,
      });
    case "failed":
      return videoJobResponseSchema.parse({
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: unixSeconds(job.createdAtMs),
        completed_at: unixSeconds(job.terminalAtMs),
        error: videoFailureError(job.failure),
      });
    case "cancelled":
      return videoJobResponseSchema.parse({
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: unixSeconds(job.createdAtMs),
        completed_at: unixSeconds(job.terminalAtMs),
        cancellation_reason: job.reason,
      });
  }
}

function formatBytes(bytes: number | "unavailable" | undefined): string {
  if (typeof bytes !== "number") return "an unknown amount";
  return `${(bytes / 1024 ** 3).toFixed(1)} GiB`;
}

const FREE_MEMORY_HINT =
  "Free memory (unload other models or close applications) and retry.";

function videoFailureError(
  failure: Error,
): Extract<VideoJobResponse, { status: "failed" }>["error"] {
  if (failure instanceof RuntimeMemoryAdmissionError) {
    return {
      code: "insufficient_memory",
      message: memoryAdmissionMessage(failure),
    };
  }
  if (failure instanceof ModelInstallConsentError) {
    return { code: "model_install_consent_required", message: failure.message };
  }
  if (failure instanceof ModelHardwareIneligibleError) {
    return { code: "model_hardware_ineligible", message: failure.message };
  }
  if (
    failure instanceof ModelManagementError &&
    failure.code === "invalid_request" &&
    failure.message === experimentalModelOptInRequired
  ) {
    return {
      code: "experimental_model_not_allowed",
      message: failure.message,
    };
  }
  return { code: "video_generation_failed" };
}

function memoryAdmissionMessage(failure: RuntimeMemoryAdmissionError): string {
  const { reason } = failure.decision;
  const diagnostics = failure.diagnostics;
  const accelerator = reason === "accelerator-memory";
  const pool = accelerator ? "accelerator (GPU) memory" : "memory";
  if (reason === "memory-pressure") {
    return `The host is under ${accelerator ? "accelerator " : ""}memory pressure, so the video runtime was not started. ${FREE_MEMORY_HINT}`;
  }
  if (reason === "measurement-unavailable") {
    return `Available memory could not be measured reliably, so the video runtime was not started. Retry shortly.`;
  }
  if (failure.gpuWorkingSet) {
    const { requiredBytes, residentBytes, capBytes } = failure.gpuWorkingSet;
    return `Not enough GPU working set to start the video runtime alongside the models already loaded: it needs ${formatBytes(requiredBytes)}, other loaded models use ${formatBytes(residentBytes)}, and the GPU working-set limit is ${formatBytes(capBytes)}. Unload other models and retry.`;
  }
  const requested = diagnostics?.requested_bytes;
  const available = diagnostics?.effective_available_bytes;
  const reserve = diagnostics?.reserve_bytes;
  const usable =
    typeof available === "number" && typeof reserve === "number"
      ? Math.max(0, available - reserve)
      : undefined;
  return `Not enough free ${pool} to start the video runtime: it needs ${formatBytes(requested)} but only ${formatBytes(usable)} is usable after the safety reserve. ${FREE_MEMORY_HINT}`;
}

/** Names the accepted profile so a rejected request is actionable. */
export function videoProfileMismatchMessage(
  request: VideoCreateRequest,
  model: ModelSpec,
): string {
  if (model.kind !== "video" || !model.videoRuntime) {
    return "This model does not support video generation.";
  }
  const { qualification, mode } = model.videoRuntime;
  const inputKind = mode === "s2v" ? "speech" : "text";
  const wrongInput =
    mode === "ti2v"
      ? request.input.kind !== "text" && request.input.kind !== "image"
      : request.input.kind !== inputKind;
  const reference = request.input_reference;
  const decodedReferenceDimensions = (() => {
    if (!reference) return undefined;
    const encoded = reference.startsWith("data:image/png;base64,")
      ? reference.slice("data:image/png;base64,".length)
      : reference;
    try {
      const png = Uint8Array.fromBase64(encoded);
      if (png.length < 24) return undefined;
      const view = new DataView(png.buffer, png.byteOffset, png.byteLength);
      return { width: view.getUint32(16), height: view.getUint32(20) };
    } catch {
      return undefined;
    }
  })();
  const gotDimensions = decodedReferenceDimensions
    ? `image ${decodedReferenceDimensions.width}x${decodedReferenceDimensions.height}`
    : `${request.width}x${request.height}`;
  return (
    `This local video model accepts exactly ${qualification.maxWidth}x${qualification.maxHeight} (width x height), ` +
    `${qualification.maxFrames} frames at ${qualification.fps} fps; ` +
    `got ${gotDimensions}, ${request.frames} frames at ${request.fps} fps.` +
    (wrongInput
      ? mode === "ti2v"
        ? " It accepts text input or a PNG input_reference."
        : ` It also requires input.kind "${inputKind}".`
      : "")
  );
}
