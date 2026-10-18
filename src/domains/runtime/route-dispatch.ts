import { voiceRouteFromPath } from "../voices/voice-http";

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
  | "decision"
  | "models"
  | "modelRetrieve"
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
  "/_localbase/hf-token",
  "/_localbase/voices",
  "/_localbase/voices/{voice_id}",
  "/_localbase/voices/{voice_id}/audio",
  "/_localbase/models",
  "/_localbase/models/{model_id}",
  "/v1/models",
  "/v1/models/{model}",
  "/v1/chat/completions",
  "/v1/embeddings",
  "/v1/systemone",
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
const servedModelPrefix = "/v1/models/";

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

/**
 * Decodes the model ID of an OpenAI `GET /v1/models/{model}` path. Clients
 * that do not percent-encode slash-bearing IDs send them as extra segments,
 * so the whole remainder is the ID.
 */
export function servedModelIdFromPath(pathname: string): string | undefined {
  if (!pathname.startsWith(servedModelPrefix)) return undefined;
  const encodedModelId = pathname.slice(servedModelPrefix.length);
  if (!encodedModelId) return undefined;
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
    case "/v1/systemone":
      return "decision";
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
        : servedModelIdFromPath(pathname)
          ? "modelRetrieve"
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
    case "decision":
      return "/v1/systemone";
    case "models":
      return "/v1/models";
    case "modelRetrieve":
      return "/v1/models/{model}";
    case "notFound": {
      if (
        pathname === "/_localbase/model-management" ||
        pathname === "/_localbase/model-discovery" ||
        pathname === "/_localbase/hf-token"
      )
        return pathname;
      // Voice ids stay out of route labels, logs and metrics.
      const voice = voiceRouteFromPath(pathname);
      if (voice?.kind === "collection") return "/_localbase/voices";
      if (voice?.kind === "voice") return "/_localbase/voices/{voice_id}";
      if (voice?.kind === "audio") return "/_localbase/voices/{voice_id}/audio";
      return undefined;
    }
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

/** Upper bound on the segment count an unmatched-route hint reports. */
export const MAX_UNMATCHED_ROUTE_SEGMENTS = 16;

const knownRouteLiteralPrefixes: readonly (readonly string[])[] =
  gatewayHttpRoutes.map((route) => {
    const literals: string[] = [];
    for (const segment of route.split("/").slice(1)) {
      if (!segment || segment.startsWith("{")) break;
      literals.push(segment);
    }
    return literals;
  });

export type UnmatchedRouteHint = Readonly<{
  /**
   * The longest literal prefix the path shares with a known route, as
   * `/<literal>/.../*`, or `other`. Built only from route-table literals,
   * never from request segments, so the value set stays bounded.
   */
  family: string;
  /** Path segment count, capped at {@link MAX_UNMATCHED_ROUTE_SEGMENTS}. */
  segments: number;
}>;

/**
 * Describes an unmatched path without recording its segments or query, so a
 * 404 shows which route family a client was near (for example a missing
 * `/v1/*` endpoint) while logs and metrics stay low-cardinality.
 */
export function unmatchedRouteHint(value: string): UnmatchedRouteHint {
  const segments = value.split("?", 1)[0].split("/").slice(1);
  let matched = 0;
  for (const literals of knownRouteLiteralPrefixes) {
    let shared = 0;
    while (
      shared < literals.length &&
      shared < segments.length &&
      literals[shared] === segments[shared]
    )
      shared++;
    matched = Math.max(matched, shared);
  }
  return {
    family:
      matched === 0 ? "other" : `/${segments.slice(0, matched).join("/")}/*`,
    segments: Math.min(segments.length, MAX_UNMATCHED_ROUTE_SEGMENTS),
  };
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
  "decision",
]);

/**
 * Management paths without a typed route whose handlers wait on several
 * sequential Hugging Face Hub requests (each bounded by its own timeout)
 * before writing anything, so a slow Hub must not trip the idle reset.
 */
const unboundedIdlePaths: ReadonlySet<string> = new Set([
  "/_localbase/model-discovery",
]);

/** Whether a gateway request should disable the per-connection idle timeout. */
export function gatewayRequestDisablesIdleTimeout(pathname: string): boolean {
  const apiPath = pathname.startsWith(`${uiApiPrefix}/`)
    ? pathname.slice(uiApiPrefix.length)
    : pathname;
  return (
    unboundedIdleRoutes.has(selectGatewayRoute(apiPath)) ||
    unboundedIdlePaths.has(apiPath)
  );
}

/**
 * Metadata routes whose handlers build their whole JSON body in memory, so
 * the gateway can buffer and compress them without delaying a stream.
 */
const bufferedJsonRoutes: ReadonlySet<GatewayRoute> = new Set<GatewayRoute>([
  "models",
  "modelRetrieve",
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
