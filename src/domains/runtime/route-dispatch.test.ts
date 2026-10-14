import { expect, test } from "bun:test";
import {
  canonicalGatewayHttpRoute,
  gatewayRequestDisablesIdleTimeout,
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
  expect(selectGatewayRoute("/v1/images/edits")).toBe("imageEdit");
  expect(canonicalGatewayHttpRoute("/v1/images/edits?x=1")).toBe(
    "/v1/images/edits",
  );
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

test("disables the idle timeout only for long-running inference routes", () => {
  const videoId = "00000000-0000-4000-8000-000000000000";
  for (const pathname of [
    "/v1/chat/completions",
    "/v1/embeddings",
    "/v1/audio/transcriptions",
    "/v1/audio/translations",
    "/v1/audio/speech",
    "/v1/images/generations",
    "/v1/images/edits",
    "/v1/videos",
    `/v1/videos/${videoId}/content`,
    "/app/api/v1/chat/completions",
    "/app/api/v1/images/generations",
  ])
    expect(gatewayRequestDisablesIdleTimeout(pathname)).toBe(true);
  for (const pathname of [
    "/health",
    "/health/ready",
    "/v1/models",
    "/_localbase/models",
    "/_localbase/api-keys",
    `/v1/videos/${videoId}`,
    `/v1/videos/${videoId}/cancel`,
    "/app/session",
    "/app/login",
    "/app/apiv1/chat/completions",
    "/nope",
  ])
    expect(gatewayRequestDisablesIdleTimeout(pathname)).toBe(false);
});

test("a silent long-running request survives the listener idle timeout", async () => {
  // Bun checks idle sockets on a coarse (~4 s) timer, so idleTimeout: 1
  // resets silent connections after roughly 4 s.
  const silenceMs = 5_500;
  const listener = Bun.serve({
    hostname: "127.0.0.1",
    port: 26541,
    idleTimeout: 1,
    fetch: async (request, server) => {
      const { pathname } = new URL(request.url);
      if (gatewayRequestDisablesIdleTimeout(pathname))
        server.timeout(request, 0);
      await Bun.sleep(silenceMs);
      return new Response("done");
    },
  });
  try {
    const base = `http://127.0.0.1:${listener.port}`;
    const [inference, control] = await Promise.allSettled([
      fetch(`${base}/v1/chat/completions`, { method: "POST" }).then((r) =>
        r.text(),
      ),
      fetch(`${base}/v1/models`).then((r) => r.text()),
    ]);
    expect(inference).toEqual({ status: "fulfilled", value: "done" });
    // The control proves the short idle timeout would otherwise reset it.
    expect(control.status).toBe("rejected");
  } finally {
    listener.stop(true);
  }
}, 15_000);
