import type { RuntimeAdmission } from "../runtime-reconciler";
import { byId } from "../../../catalog";
import { openAIErrorResponseSchema } from "../openai-error";
import { videoJobIdFromPath } from "../route-dispatch";
import {
  projectVideoJob,
  qualifiedVideoInput,
  type VideoCreateRequest,
} from "./gateway-contract";
import type { VideoJob, VideoJobManager } from "./video-job-manager";

type VideoRoute =
  "videoCreate" | "videoStatus" | "videoContent" | "videoCancel";
type VideoJobs = Pick<
  VideoJobManager,
  "start" | "get" | "artifact" | "cancel" | "delete"
>;

type VideoAdmission = Readonly<{
  ready: RuntimeAdmission["ready"];
  release: RuntimeAdmission["release"];
  cancel: RuntimeAdmission["cancel"];
  supervisor: Pick<RuntimeAdmission["supervisor"], "kill">;
}>;

export type VideoModelAdmissionProvider = Readonly<{
  admit: (
    modelId: string,
    signal: AbortSignal,
  ) => Promise<
    | Readonly<{ kind: "admitted"; admission: VideoAdmission }>
    | Readonly<{ kind: "not-configured" | "model-not-found" | "unavailable" }>
  >;
}>;

type ParsedCreateRequest =
  | Readonly<{ success: true; data: VideoCreateRequest }>
  | Readonly<{ success: false; response: Response }>;

export type VideoGatewayHandlerDependencies = Readonly<{
  request: Request;
  pathname: string;
  route: VideoRoute;
  ownerId: string;
  jobs: VideoJobs;
  createEnabled: boolean;
  admissionProvider: VideoModelAdmissionProvider;
  parseCreateRequest: () => Promise<ParsedCreateRequest>;
  notConfigured: () => Response;
  modelNotFound: (model: string) => Response;
  badRequest: (message: string) => Response;
  methodNotAllowed: (allow: string) => Response;
  routeNotFound: () => Response;
  requestAborted: () => Response;
  onTerminal: (options: { job: VideoJob; modelId: string }) => void;
}>;

export async function handleVideoGatewayRequest(
  dependencies: VideoGatewayHandlerDependencies,
): Promise<Response> {
  const { request, route, jobs } = dependencies;
  const jobId = videoJobIdFromPath(dependencies.pathname);

  if (route === "videoStatus") {
    if (!jobId) return dependencies.routeNotFound();
    if (request.method === "DELETE") {
      return jobs.delete({ ownerId: dependencies.ownerId, id: jobId })
        ? new Response(null, { status: 204 })
        : videoJobNotFound();
    }
    if (request.method !== "GET")
      return dependencies.methodNotAllowed("GET, DELETE");
    const job = jobs.get({ ownerId: dependencies.ownerId, id: jobId });
    return job ? Response.json(projectVideoJob(job)) : videoJobNotFound();
  }

  if (route === "videoContent") {
    if (request.method !== "GET") return dependencies.methodNotAllowed("GET");
    if (!jobId) return dependencies.routeNotFound();
    const artifact = jobs.artifact({
      ownerId: dependencies.ownerId,
      id: jobId,
    });
    if (!artifact || !(await Bun.file(artifact.path).exists())) {
      return videoJobNotFound();
    }
    return new Response(Bun.file(artifact.path), {
      headers: {
        "content-type": artifact.metadata.mimeType,
        "content-length": String(artifact.metadata.byteLength),
        "x-content-type-options": "nosniff",
        "cache-control": "private, no-store",
      },
    });
  }

  if (route === "videoCancel") {
    if (request.method !== "POST") return dependencies.methodNotAllowed("POST");
    if (!jobId) return dependencies.routeNotFound();
    const job = await jobs.cancel({ ownerId: dependencies.ownerId, id: jobId });
    return job ? Response.json(projectVideoJob(job)) : videoJobNotFound();
  }

  if (request.method !== "POST") return dependencies.methodNotAllowed("POST");
  if (request.signal.aborted) return dependencies.requestAborted();
  if (!dependencies.createEnabled) return dependencies.notConfigured();
  const parsed = await dependencies.parseCreateRequest();
  if (!parsed.success) return parsed.response;
  if (request.signal.aborted) return dependencies.requestAborted();
  const spec = byId(parsed.data.model);
  if (!spec || spec.kind !== "video" || !spec.videoRuntime) {
    return dependencies.modelNotFound(parsed.data.model);
  }
  const videoInput = qualifiedVideoInput(parsed.data, spec);
  if (!videoInput) {
    return dependencies.badRequest(
      "This local video model only accepts its qualified width, height, frames, and fps profile.",
    );
  }

  let admission: VideoAdmission | undefined;
  const started = await jobs.start({
    ownerId: dependencies.ownerId,
    input: videoInput,
    jobDeadlineMs: spec.videoRuntime.qualification.jobDeadlineMs,
    acquireAdmission: async ({ signal }) => {
      const selection = await dependencies.admissionProvider.admit(
        parsed.data.model,
        signal,
      );
      if (selection.kind !== "admitted") return undefined;
      admission = selection.admission;
      return admission;
    },
    supervisedStop: async () => {
      await admission?.supervisor.kill();
    },
  });
  if (started.kind === "busy") {
    return videoJobBusy();
  }
  void started.terminal.then((job) => {
    dependencies.onTerminal({ job, modelId: parsed.data.model });
  });
  return Response.json(projectVideoJob(started.job), { status: 202 });
}

function videoJobNotFound(): Response {
  return Response.json(
    openAIErrorResponseSchema.parse({
      error: {
        message: "Video job not found.",
        type: "invalid_request_error",
        param: null,
        code: "video_job_not_found",
      },
    }),
    { status: 404 },
  );
}

function videoJobBusy(): Response {
  return Response.json(
    openAIErrorResponseSchema.parse({
      error: {
        message: "A local video job is already in progress.",
        type: "rate_limit_error",
        param: null,
        code: "video_job_busy",
      },
    }),
    { status: 429, headers: { "Retry-After": "1" } },
  );
}
