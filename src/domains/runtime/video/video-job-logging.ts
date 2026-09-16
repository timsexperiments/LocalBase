import type { ILogger } from "../../observability/logging";
import type { VideoJob } from "./video-job-manager";

export function logVideoJobTerminal({
  logger,
  job,
  modelId,
  requestId,
}: {
  logger: Pick<ILogger, "event">;
  job: VideoJob;
  modelId: string;
  requestId: string;
}): void {
  logger.event({
    severity: job.state === "failed" ? "error" : "info",
    eventName: "video.job-terminal",
    category: "runtime",
    component: "video-job-manager",
    runtime: "video",
    requestId,
    message: "A local video job reached a terminal state.",
    ...(job.state === "failed"
      ? {
          error: {
            type: "VideoJobFailure",
            message: "A local video job failed.",
          },
        }
      : {}),
    attributes: {
      job_id: job.id,
      model_id: modelId,
      state: job.state,
      duration_ms:
        "terminalAtMs" in job ? job.terminalAtMs - job.createdAtMs : 0,
    },
  });
}
