import { expect, test } from "bun:test";
import {
  canonicalGatewayHttpRoute,
  selectGatewayRoute,
  videoJobIdFromPath,
} from "./route-dispatch";

test("selects each supported gateway route by its exact path", () => {
  expect(selectGatewayRoute("/health")).toBe("health");
  expect(selectGatewayRoute("/_localbase/instance")).toBe("instance");
  expect(selectGatewayRoute("/_localbase/access-management")).toBe(
    "accessManagement",
  );
  expect(selectGatewayRoute("/_localbase/api-keys")).toBe("keyManagement");
  expect(selectGatewayRoute("/_localbase/models")).toBe("modelMetadataList");
  expect(selectGatewayRoute("/_localbase/models/qwen%2Ftest")).toBe(
    "modelMetadataDetail",
  );
  expect(selectGatewayRoute("/v1/audio/transcriptions")).toBe("transcription");
  expect(selectGatewayRoute("/v1/audio/translations")).toBe("transcription");
  expect(selectGatewayRoute("/v1/audio/speech")).toBe("speechGeneration");
  expect(selectGatewayRoute("/v1/images/generations")).toBe("imageGeneration");
  expect(selectGatewayRoute("/v1/videos")).toBe("videoCreate");
  expect(
    selectGatewayRoute("/v1/videos/00000000-0000-4000-8000-000000000000"),
  ).toBe("videoStatus");
  expect(
    selectGatewayRoute(
      "/v1/videos/00000000-0000-4000-8000-000000000000/content",
    ),
  ).toBe("videoContent");
  expect(
    selectGatewayRoute(
      "/v1/videos/00000000-0000-4000-8000-000000000000/cancel",
    ),
  ).toBe("videoCancel");
  expect(selectGatewayRoute("/v1/chat/completions")).toBe("chatCompletion");
  expect(selectGatewayRoute("/v1/embeddings")).toBe("embeddings");
  expect(selectGatewayRoute("/v1/models")).toBe("models");
});

test("normalizes dynamic gateway paths without recording identifiers", () => {
  expect(canonicalGatewayHttpRoute("/_localbase/models/qwen%2Ftest")).toBe(
    "/_localbase/models/{model_id}",
  );
  expect(canonicalGatewayHttpRoute("/_localbase/access-management")).toBe(
    "/_localbase/access-management",
  );
  expect(canonicalGatewayHttpRoute("/_localbase/api-keys")).toBe(
    "/_localbase/api-keys",
  );
  expect(
    canonicalGatewayHttpRoute(
      "/v1/videos/00000000-0000-4000-8000-000000000000/cancel",
    ),
  ).toBe("/v1/videos/{job_id}/cancel");
  expect(canonicalGatewayHttpRoute("/private/model-name")).toBe(
    "unmatched-route",
  );
});

test("classifies unexposed and near-match paths as not found", () => {
  expect(selectGatewayRoute("/v1/completions")).toBe("notFound");
  expect(selectGatewayRoute("/health/")).toBe("notFound");
  expect(selectGatewayRoute("/v1/models/")).toBe("notFound");
  expect(selectGatewayRoute("/_localbase/models/")).toBe("notFound");
  expect(selectGatewayRoute("/_localbase/access-management/")).toBe("notFound");
  expect(selectGatewayRoute("/_localbase/api-keys/extra")).toBe("notFound");
  expect(selectGatewayRoute("/_localbase/models/not/a-model")).toBe("notFound");
  expect(selectGatewayRoute("/v1/videos/not-a-job")).toBe("notFound");
  expect(
    videoJobIdFromPath("/v1/videos/00000000-0000-4000-8000-000000000000/other"),
  ).toBeUndefined();
});
