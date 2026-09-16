import { z } from "zod";
import type { ModelSpec } from "../../../catalog";
import {
  videoConditioningInputSchema,
  videoGenerationInputSchema,
  type VideoGenerationInput,
} from "./video-input";
import type { VideoJob } from "./video-job-manager";

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
  })
  .strict();

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
    error: z.object({ code: z.literal("video_generation_failed") }).strict(),
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
    (model.videoRuntime.mode === "t2v" && request.input.kind !== "text") ||
    (model.videoRuntime.mode === "s2v" && request.input.kind !== "speech")
  ) {
    return undefined;
  }
  const candidate = {
    kind: request.input.kind,
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
    ...(request.input.kind === "speech"
      ? { portrait: request.input.portrait, audio: request.input.audio }
      : {}),
  };
  const parsed = videoGenerationInputSchema.safeParse(candidate);
  return parsed.success ? parsed.data : undefined;
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
        error: { code: "video_generation_failed" },
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
