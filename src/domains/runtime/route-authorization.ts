import type { AuthorizationRequirement } from "../auth/authorization";
import type { GatewayRoute } from "./route-dispatch";

export function gatewayAuthorizationRequirement({
  route,
  method,
  authRequired,
}: {
  route: GatewayRoute;
  method: string;
  authRequired: boolean;
}): AuthorizationRequirement {
  if (method === "OPTIONS") return { kind: "public" };
  switch (route) {
    case "health":
    case "readiness":
    // The instance handler separately verifies the private lease token.
    case "instance":
      return { kind: "public" };
    case "modelMetadataList":
    case "modelMetadataDetail":
      return { kind: "permission", permission: "models:read" };
    case "videoCreate":
    case "videoStatus":
    case "videoContent":
    case "videoCancel":
      return { kind: "permission", permission: "inference:video" };
    case "models":
      return authRequired
        ? { kind: "permission", permission: "models:read" }
        : { kind: "public" };
    case "transcription":
      return authRequired
        ? { kind: "permission", permission: "inference:transcription" }
        : { kind: "public" };
    case "speechGeneration":
      return authRequired
        ? { kind: "permission", permission: "inference:speech" }
        : { kind: "public" };
    case "imageGeneration":
      return authRequired
        ? { kind: "permission", permission: "inference:image" }
        : { kind: "public" };
    case "chatCompletion":
      return authRequired
        ? { kind: "permission", permission: "inference:chat" }
        : { kind: "public" };
    case "embeddings":
      return authRequired
        ? { kind: "permission", permission: "inference:embeddings" }
        : { kind: "public" };
    case "notFound":
      return { kind: authRequired ? "authenticated" : "public" };
    default: {
      const exhaustive: never = route;
      return exhaustive;
    }
  }
}
