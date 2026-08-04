export type GatewayRoute =
  | "health"
  | "instance"
  | "transcription"
  | "imageGeneration"
  | "chatCompletion"
  | "embeddings"
  | "models"
  | "notFound";

/** Selects the gateway handler for an exact public path. */
export function selectGatewayRoute(pathname: string): GatewayRoute {
  switch (pathname) {
    case "/health":
      return "health";
    case "/_localbase/instance":
      return "instance";
    case "/v1/audio/transcriptions":
    case "/v1/audio/translations":
      return "transcription";
    case "/v1/images/generations":
      return "imageGeneration";
    case "/v1/chat/completions":
      return "chatCompletion";
    case "/v1/embeddings":
      return "embeddings";
    case "/v1/models":
      return "models";
    default:
      return "notFound";
  }
}
