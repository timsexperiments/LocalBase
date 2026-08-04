import { expect, test } from "bun:test";
import { selectGatewayRoute } from "./route-dispatch";

test("selects each supported gateway route by its exact path", () => {
  expect(selectGatewayRoute("/health")).toBe("health");
  expect(selectGatewayRoute("/_localbase/instance")).toBe("instance");
  expect(selectGatewayRoute("/v1/audio/transcriptions")).toBe("transcription");
  expect(selectGatewayRoute("/v1/audio/translations")).toBe("transcription");
  expect(selectGatewayRoute("/v1/images/generations")).toBe("imageGeneration");
  expect(selectGatewayRoute("/v1/chat/completions")).toBe("chatCompletion");
  expect(selectGatewayRoute("/v1/embeddings")).toBe("embeddings");
  expect(selectGatewayRoute("/v1/models")).toBe("models");
});

test("classifies unexposed and near-match paths as not found", () => {
  expect(selectGatewayRoute("/v1/completions")).toBe("notFound");
  expect(selectGatewayRoute("/health/")).toBe("notFound");
  expect(selectGatewayRoute("/v1/models/")).toBe("notFound");
});
