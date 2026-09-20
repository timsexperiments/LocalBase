export type GatewayRoute =
  | "health"
  | "readiness"
  | "instance"
  | "transcription"
  | "speechGeneration"
  | "imageGeneration"
  | "videoCreate"
  | "videoStatus"
  | "videoContent"
  | "videoCancel"
  | "chatCompletion"
  | "embeddings"
  | "models"
  | "modelMetadataList"
  | "modelMetadataDetail"
  | "notFound";

export const gatewayHttpRoutes = [
  "/health",
  "/health/ready",
  "/_localbase/instance",
  "/_localbase/models",
  "/_localbase/models/{model_id}",
  "/v1/models",
  "/v1/chat/completions",
  "/v1/embeddings",
  "/v1/audio/transcriptions",
  "/v1/audio/translations",
  "/v1/audio/speech",
  "/v1/images/generations",
  "/v1/videos",
  "/v1/videos/{job_id}",
  "/v1/videos/{job_id}/content",
  "/v1/videos/{job_id}/cancel",
  "unmatched-route",
] as const;

export type GatewayHttpRoute = (typeof gatewayHttpRoutes)[number];

const modelMetadataPrefix = "/_localbase/models/";
const videoPrefix = "/v1/videos/";

export function videoJobIdFromPath(pathname: string): string | undefined {
  if (!pathname.startsWith(videoPrefix)) return undefined;
  const suffix = pathname.slice(videoPrefix.length);
  const id = suffix.split("/")[0];
  if (!id || !/^[0-9a-f-]{36}$/i.test(id)) return undefined;
  return suffix === id ||
    suffix === `${id}/content` ||
    suffix === `${id}/cancel`
    ? id
    : undefined;
}

export function modelMetadataIdFromPath(pathname: string): string | undefined {
  if (!pathname.startsWith(modelMetadataPrefix)) return undefined;
  const encodedModelId = pathname.slice(modelMetadataPrefix.length);
  if (!encodedModelId || encodedModelId.includes("/")) return undefined;
  try {
    return decodeURIComponent(encodedModelId);
  } catch {
    return undefined;
  }
}

/** Selects the gateway handler for an exact public path. */
export function selectGatewayRoute(pathname: string): GatewayRoute {
  switch (pathname) {
    case "/health":
      return "health";
    case "/health/ready":
      return "readiness";
    case "/_localbase/instance":
      return "instance";
    case "/_localbase/models":
      return "modelMetadataList";
    case "/v1/audio/transcriptions":
    case "/v1/audio/translations":
      return "transcription";
    case "/v1/audio/speech":
      return "speechGeneration";
    case "/v1/images/generations":
      return "imageGeneration";
    case "/v1/videos":
      return "videoCreate";
    case "/v1/chat/completions":
      return "chatCompletion";
    case "/v1/embeddings":
      return "embeddings";
    case "/v1/models":
      return "models";
    default:
      if (videoJobIdFromPath(pathname)) {
        if (pathname.endsWith("/content")) return "videoContent";
        if (pathname.endsWith("/cancel")) return "videoCancel";
        return pathname.startsWith(videoPrefix) ? "videoStatus" : "notFound";
      }
      return modelMetadataIdFromPath(pathname)
        ? "modelMetadataDetail"
        : "notFound";
  }
}

/** Returns a bounded route template suitable for logs, traces, and metrics. */
export function canonicalGatewayHttpRoute(value: string): GatewayHttpRoute {
  const pathname = value.split("?", 1)[0];
  switch (selectGatewayRoute(pathname)) {
    case "health":
      return "/health";
    case "readiness":
      return "/health/ready";
    case "instance":
      return "/_localbase/instance";
    case "modelMetadataList":
      return "/_localbase/models";
    case "modelMetadataDetail":
      return "/_localbase/models/{model_id}";
    case "transcription":
      return pathname === "/v1/audio/translations"
        ? "/v1/audio/translations"
        : "/v1/audio/transcriptions";
    case "speechGeneration":
      return "/v1/audio/speech";
    case "imageGeneration":
      return "/v1/images/generations";
    case "videoCreate":
      return "/v1/videos";
    case "videoStatus":
      return "/v1/videos/{job_id}";
    case "videoContent":
      return "/v1/videos/{job_id}/content";
    case "videoCancel":
      return "/v1/videos/{job_id}/cancel";
    case "chatCompletion":
      return "/v1/chat/completions";
    case "embeddings":
      return "/v1/embeddings";
    case "models":
      return "/v1/models";
    case "notFound":
      return "unmatched-route";
  }
}
