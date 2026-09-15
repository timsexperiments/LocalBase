import { z } from "zod";
import type { ModelSpec } from "../../../catalog";
import type { VideoJob, VideoJobInput } from "./video-job-manager";

export const videoCreateRequestSchema = z
  .object({
    model: z.string().min(1),
    prompt: z.string().min(1).max(16_384),
    negative_prompt: z.string().max(16_384).optional(),
    width: z.number().int().positive(),
    height: z.number().int().positive(),
    frames: z.number().int().positive(),
    fps: z.number().int().positive(),
  })
  .strict();

export type VideoCreateRequest = z.infer<typeof videoCreateRequestSchema>;

export function qualifiedVideoInput(
  request: VideoCreateRequest,
  model: ModelSpec,
): VideoJobInput | undefined {
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
  return {
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
      steps: qualification.generation.steps,
      cfgScale: qualification.generation.cfgScale,
    },
  };
}

export function projectVideoJob(job: VideoJob): Record<string, unknown> {
  switch (job.state) {
    case "queued":
    case "in_progress":
      return {
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: job.createdAtMs,
      };
    case "completed":
      return {
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: job.createdAtMs,
        completed_at: job.terminalAtMs,
        content_type: job.artifact.mimeType,
        bytes: job.artifact.byteLength,
        fps: job.artifact.fps,
        frames: job.artifact.frameCount,
      };
    case "failed":
      return {
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: job.createdAtMs,
        completed_at: job.terminalAtMs,
        error: { code: "video_generation_failed" },
      };
    case "cancelled":
      return {
        object: "localbase.video.job",
        id: job.id,
        status: job.state,
        created_at: job.createdAtMs,
        completed_at: job.terminalAtMs,
        cancellation_reason: job.reason,
      };
  }
}
