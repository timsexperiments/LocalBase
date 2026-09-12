export type GatewayRoute =
  | "health"
  | "readiness"
  | "instance"
  | "transcription"
  | "speechGeneration"
  | "imageGeneration"
  | "chatCompletion"
  | "embeddings"
  | "models"
  | "modelMetadataList"
  | "modelMetadataDetail"
  | "notFound";

const modelMetadataPrefix = "/_localbase/models/";

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
    case "/v1/chat/completions":
      return "chatCompletion";
    case "/v1/embeddings":
      return "embeddings";
    case "/v1/models":
      return "models";
    default:
      return modelMetadataIdFromPath(pathname)
        ? "modelMetadataDetail"
        : "notFound";
  }
}
