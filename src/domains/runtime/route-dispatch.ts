export type GatewayRoute =
  | "health"
  | "readiness"
  | "instance"
  | "accessManagement"
  | "keyManagement"
  | "transcription"
  | "speechGeneration"
  | "imageGeneration"
  | "imageEdit"
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

const apiHttpRoutes = [
  "/health",
  "/health/ready",
  "/_localbase/instance",
  "/_localbase/access-management",
  "/_localbase/api-keys",
  "/_localbase/model-management",
  "/_localbase/model-discovery",
  "/_localbase/models",
  "/_localbase/models/{model_id}",
  "/v1/models",
  "/v1/chat/completions",
  "/v1/embeddings",
  "/v1/audio/transcriptions",
  "/v1/audio/translations",
  "/v1/audio/speech",
  "/v1/images/generations",
  "/v1/images/edits",
  "/v1/videos",
  "/v1/videos/{job_id}",
  "/v1/videos/{job_id}/content",
  "/v1/videos/{job_id}/cancel",
] as const;

type ApiHttpRoute = (typeof apiHttpRoutes)[number];

/** Browser shell, static asset, and sign-in routes served outside the API. */
const uiHttpRoutes = [
  "/",
  "/app",
  "/app/assets/{asset}",
  "/app/login",
  "/app/login/magic",
  "/app/logout",
  "/app/session",
  "/oidc/callback",
  "/github/callback",
  "/magic-link/callback",
] as const;

type UiHttpRoute = (typeof uiHttpRoutes)[number];
type UiApiHttpRoute = `/app/api${ApiHttpRoute}`;

export type GatewayHttpRoute =
  ApiHttpRoute | UiHttpRoute | UiApiHttpRoute | "unmatched-route";

export const gatewayHttpRoutes = [
  ...apiHttpRoutes,
  ...uiHttpRoutes,
  ...apiHttpRoutes.map((route): UiApiHttpRoute => `/app/api${route}`),
  "unmatched-route",
] as [GatewayHttpRoute, ...GatewayHttpRoute[]];

const uiApiPrefix = "/app/api";
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
    case "/_localbase/access-management":
      return "accessManagement";
    case "/_localbase/api-keys":
      return "keyManagement";
    case "/_localbase/models":
      return "modelMetadataList";
    case "/v1/audio/transcriptions":
    case "/v1/audio/translations":
      return "transcription";
    case "/v1/audio/speech":
      return "speechGeneration";
    case "/v1/images/generations":
      return "imageGeneration";
    case "/v1/images/edits":
      return "imageEdit";
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

function apiHttpRoute(pathname: string): ApiHttpRoute | undefined {
  switch (selectGatewayRoute(pathname)) {
    case "health":
      return "/health";
    case "readiness":
      return "/health/ready";
    case "instance":
      return "/_localbase/instance";
    case "accessManagement":
      return "/_localbase/access-management";
    case "keyManagement":
      return "/_localbase/api-keys";
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
    case "imageEdit":
      return "/v1/images/edits";
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
      return pathname === "/_localbase/model-management" ||
        pathname === "/_localbase/model-discovery"
        ? pathname
        : undefined;
  }
}

function isUiHttpRoute(pathname: string): pathname is UiHttpRoute {
  return uiHttpRoutes.some((route) => route === pathname);
}

/** Returns a bounded route template suitable for logs, traces, and metrics. */
export function canonicalGatewayHttpRoute(value: string): GatewayHttpRoute {
  const pathname = value.split("?", 1)[0];
  const apiRoute = apiHttpRoute(pathname);
  if (apiRoute) return apiRoute;
  if (isUiHttpRoute(pathname)) return pathname;
  if (pathname === "/app/") return "/app";
  if (pathname.startsWith("/app/assets/")) return "/app/assets/{asset}";
  if (pathname.startsWith(`${uiApiPrefix}/`)) {
    const uiApiRoute = apiHttpRoute(pathname.slice(uiApiPrefix.length));
    if (uiApiRoute) return `${uiApiPrefix}${uiApiRoute}`;
  }
  return "unmatched-route";
}

/**
 * Routes whose responses can legitimately stay silent for longer than Bun's
 * default 10 s socket idle timeout: queue admission, cold model loads, and
 * LLM prefill or non-streaming generation all happen before the first byte.
 * These requests are bounded by client disconnect (propagated upstream via
 * `request.signal`), the inference queue wait deadline, and upstream failure,
 * so the listener's idle timer must not reset the client connection.
 */
const unboundedIdleRoutes: ReadonlySet<GatewayRoute> = new Set<GatewayRoute>([
  "transcription",
  "speechGeneration",
  "imageGeneration",
  "imageEdit",
  "videoCreate",
  "videoContent",
  "chatCompletion",
  "embeddings",
]);

/** Whether a gateway request should disable the per-connection idle timeout. */
export function gatewayRequestDisablesIdleTimeout(pathname: string): boolean {
  const apiPath = pathname.startsWith(`${uiApiPrefix}/`)
    ? pathname.slice(uiApiPrefix.length)
    : pathname;
  return unboundedIdleRoutes.has(selectGatewayRoute(apiPath));
}

/**
 * Metadata routes whose handlers build their whole JSON body in memory, so
 * the gateway can buffer and compress them without delaying a stream.
 */
const bufferedJsonRoutes: ReadonlySet<GatewayRoute> = new Set<GatewayRoute>([
  "models",
  "modelMetadataList",
  "modelMetadataDetail",
]);

/** Whether a gateway response is a compressible in-memory JSON document. */
export function gatewayRouteServesBufferedJson(pathname: string): boolean {
  const apiPath = pathname.startsWith(`${uiApiPrefix}/`)
    ? pathname.slice(uiApiPrefix.length)
    : pathname;
  return bufferedJsonRoutes.has(selectGatewayRoute(apiPath));
}
