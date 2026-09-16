import { expect, spyOn, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ensureLocalBaseRootMarker } from "../../../utils/root";
import { LocalBaseLogger, readLogSnapshot } from "../../observability/logging";
import { VideoJobManager } from "./video-job-manager";
import { logVideoJobTerminal } from "./video-job-logging";
import {
  createStableDiffusionVideoClient,
  StableDiffusionVideoClientError,
} from "./stable-diffusion-video-client";

const VIDEO_ID = "job_01HTXYZVID";

function startBackend(
  handler: (request: Request) => Response | Promise<Response>,
) {
  const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: handler });
  return {
    client(
      options: { maxResponseBytes?: number; maxMediaBytes?: number } = {},
    ) {
      return createStableDiffusionVideoClient({
        baseUrl: `http://127.0.0.1:${server.port}`,
        ...options,
      });
    },
    stop() {
      server.stop(true);
    },
  };
}

function acceptedJob(id = VIDEO_ID) {
  return {
    id,
    kind: "vid_gen",
    status: "queued",
    created: 1,
    poll_url: "http://untrusted.example.invalid/jobs/elsewhere",
  };
}

function completedJob(id = VIDEO_ID) {
  return {
    id,
    kind: "vid_gen",
    status: "completed",
    created: 1,
    started: 2,
    completed: 3,
    queue_position: 0,
    result: {
      output_format: "webm",
      mime_type: "video/webm",
      fps: 16,
      frame_count: 33,
      b64_json: Uint8Array.from([0x1a, 0x45, 0xdf, 0xa3]).toBase64(),
    },
    error: null,
  };
}

test("keeps native failure details out of job errors and emitted logs", async () => {
  const sentinel =
    "A confidential acquisition discussion between Alice and Bob";
  const root = mkdtempSync(join(tmpdir(), "localbase-video-log-"));
  ensureLocalBaseRootMarker(root);
  const backend = startBackend((request) =>
    Response.json(
      request.method === "POST"
        ? acceptedJob()
        : {
            ...acceptedJob(),
            status: "failed",
            error: { code: sentinel, message: sentinel },
          },
      { status: request.method === "POST" ? 202 : 200 },
    ),
  );
  const logger = new LocalBaseLogger("json");
  const output = spyOn(console, "log").mockImplementation(() => {});
  const jobs = new VideoJobManager({
    backend: backend.client(),
    temporaryDirectory: root,
    acquireAdmission: async () => ({
      ready: Promise.resolve(),
      release: () => {},
    }),
    supervisedStop: async () => {},
    onContainmentFailure: () => {},
  });
  try {
    await logger.enableFileLogging(root);
    const started = await jobs.start({
      ownerId: "test-owner",
      input: { prompt: sentinel },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    const job = await started.terminal;
    if (job.state !== "failed") throw new Error("Expected failed job.");
    expect(job.failure).toMatchObject({
      name: "VideoBackendJobFailureError",
      message: "Video backend job failed.",
    });
    for (const failure of [job.failure, new Error(sentinel)]) {
      logVideoJobTerminal({
        logger,
        job: { ...job, failure },
        modelId: "wan2.1-t2v-1.3b-q8_0",
        requestId: "video-log-test",
      });
    }
    await logger.close();
    const events = await readLogSnapshot(root);
    expect(events).toHaveLength(2);
    for (const event of events) {
      expect(event).toMatchObject({
        eventName: "video.job-terminal",
        error: {
          type: "VideoJobFailure",
          message: "A local video job failed.",
        },
      });
    }
    expect(JSON.stringify(events)).not.toContain(sentinel);
    expect(JSON.stringify(output.mock.calls)).not.toContain(sentinel);
  } finally {
    await jobs.shutdown();
    await logger.close();
    output.mockRestore();
    backend.stop();
    rmSync(root, { recursive: true, force: true });
  }
});

test.each(["discrete", "lcm"] as const)(
  "submits, polls, and cancels with %s scheduler",
  async (scheduler) => {
    const requests: { method: string; path: string; body: unknown }[] = [];
    const backend = startBackend(async (request) => {
      const url = new URL(request.url);
      const body: unknown =
        request.headers.get("content-type") === "application/json"
          ? await request.json()
          : null;
      requests.push({ method: request.method, path: url.pathname, body });
      if (url.pathname === "/sdcpp/v1/capabilities") {
        return Response.json({
          supported_modes: ["img_gen", "vid_gen"],
          output_formats_by_mode: { vid_gen: ["webm", "webp", "avi"] },
        });
      }
      if (url.pathname === "/sdcpp/v1/vid_gen") {
        return Response.json(acceptedJob(), { status: 202 });
      }
      if (url.pathname === `/sdcpp/v1/jobs/${VIDEO_ID}`) {
        return Response.json(completedJob());
      }
      if (url.pathname === `/sdcpp/v1/jobs/${VIDEO_ID}/cancel`) {
        return Response.json({
          ...acceptedJob(),
          status: "cancelled",
          error: { code: "cancelled", message: "private backend detail" },
        });
      }
      return new Response(null, { status: 404 });
    });
    try {
      const client = backend.client();
      await expect(client.getCapabilities()).resolves.toEqual({
        available: true,
        outputFormats: ["webm", "webp", "avi"],
      });
      await expect(
        client.submitVideo({
          input: {
            prompt: "A small sailboat on a lake.",
            negativePrompt: "text overlay",
            width: 832,
            height: 480,
            videoFrames: 33,
            fps: 16,
            seed: -1,
            outputFormat: "webm",
            generation: {
              sampler: "euler",
              scheduler,
              steps: 20,
              cfgScale: 6,
              flowShift: 3,
            },
          },
        }),
      ).resolves.toEqual({ id: VIDEO_ID, status: "queued" });
      await expect(client.getJob({ id: VIDEO_ID })).resolves.toEqual({
        id: VIDEO_ID,
        status: "completed",
        media: {
          bytes: Uint8Array.from([0x1a, 0x45, 0xdf, 0xa3]),
          mimeType: "video/webm",
          outputFormat: "webm",
          fps: 16,
          frameCount: 33,
        },
      });
      await expect(client.cancelJob({ id: VIDEO_ID })).resolves.toEqual({
        id: VIDEO_ID,
        status: "cancelled",
      });
      expect(requests).toEqual([
        { method: "GET", path: "/sdcpp/v1/capabilities", body: null },
        {
          method: "POST",
          path: "/sdcpp/v1/vid_gen",
          body: {
            prompt: "A small sailboat on a lake.",
            negative_prompt: "text overlay",
            width: 832,
            height: 480,
            video_frames: 33,
            fps: 16,
            seed: -1,
            output_format: "webm",
            sample_params: {
              sample_method: "euler",
              scheduler,
              sample_steps: 20,
              flow_shift: 3,
              guidance: { txt_cfg: 6 },
            },
          },
        },
        { method: "GET", path: `/sdcpp/v1/jobs/${VIDEO_ID}`, body: null },
        {
          method: "POST",
          path: `/sdcpp/v1/jobs/${VIDEO_ID}/cancel`,
          body: null,
        },
      ]);
    } finally {
      backend.stop();
    }
  },
);

test("rejects malformed backend responses and responses over the configured limit", async () => {
  const backend = startBackend((request) => {
    const path = new URL(request.url).pathname;
    if (path === "/sdcpp/v1/vid_gen") {
      return Response.json(
        { kind: "vid_gen", status: "queued", created: 1 },
        { status: 202 },
      );
    }
    return new Response("x".repeat(4_096));
  });
  try {
    const client = backend.client({
      maxResponseBytes: 2_048,
      maxMediaBytes: 1,
    });
    await expect(
      client.submitVideo({ input: { prompt: "A test." } }),
    ).rejects.toMatchObject({ code: "backend_response_invalid" });
    await expect(client.getCapabilities()).rejects.toMatchObject({
      code: "backend_response_too_large",
    });
  } finally {
    backend.stop();
  }
});

test("rejects oversized completed media and invalid job IDs before a request", async () => {
  let requests = 0;
  const backend = startBackend((request) => {
    requests += 1;
    return Response.json(
      completedJob(new URL(request.url).pathname.split("/").at(-1) ?? ""),
    );
  });
  try {
    const client = backend.client({ maxMediaBytes: 2 });
    await expect(client.getJob({ id: VIDEO_ID })).rejects.toMatchObject({
      code: "backend_media_too_large",
    });
    await expect(client.getJob({ id: "../outside" })).rejects.toMatchObject({
      code: "invalid_job_id",
    });
    expect(requests).toBe(1);
  } finally {
    backend.stop();
  }
});

test("rejects malformed base64, mismatched MIME types, and invalid containers", async () => {
  const malformedBase64 = completedJob();
  malformedBase64.result.b64_json = "%%%";
  const mismatchedMimeType = completedJob();
  mismatchedMimeType.result.mime_type = "text/html";
  const invalidContainer = completedJob();
  invalidContainer.result.b64_json = Uint8Array.from([1, 2, 3, 4]).toBase64();
  const jobs = [malformedBase64, mismatchedMimeType, invalidContainer];
  const backend = startBackend(() => {
    const job = jobs.shift();
    if (job === undefined) throw new Error("Expected a fixture job.");
    return Response.json(job);
  });
  try {
    const client = backend.client();
    for (let index = 0; index < 3; index += 1) {
      await expect(client.getJob({ id: VIDEO_ID })).rejects.toMatchObject({
        code: "backend_response_invalid",
      });
    }
  } finally {
    backend.stop();
  }
});

test("requires a response budget that can hold padded media and its envelope", () => {
  const backend = startBackend(() => Response.json({}));
  try {
    expect(() =>
      backend.client({ maxResponseBytes: 1_024, maxMediaBytes: 1_024 }),
    ).toThrow(RangeError);
  } finally {
    backend.stop();
  }
});

test("retains backend HTTP status without reading backend error details", async () => {
  const backend = startBackend(() => new Response(null, { status: 410 }));
  try {
    await expect(
      backend.client().getJob({ id: VIDEO_ID }),
    ).rejects.toMatchObject({
      code: "backend_request_failed",
      status: 410,
    });
  } finally {
    backend.stop();
  }
});

test("propagates caller disconnects without retrying", async () => {
  let requests = 0;
  const backend = startBackend(async () => {
    requests += 1;
    return await new Promise<Response>(() => {});
  });
  try {
    const client = backend.client();
    await expect(
      client.getCapabilities({ signal: AbortSignal.timeout(20) }),
    ).rejects.toBeDefined();
    expect(requests).toBe(1);
  } finally {
    backend.stop();
  }
});

test("rejects non-local base URLs", () => {
  expect(() =>
    createStableDiffusionVideoClient({ baseUrl: "https://backend.example" }),
  ).toThrow(StableDiffusionVideoClientError);
});

test("rejects backend redirects", async () => {
  const backend = startBackend(() =>
    Response.redirect("http://127.0.0.1:1/not-a-video-route", 302),
  );
  try {
    await expect(backend.client().getCapabilities()).rejects.toBeDefined();
  } finally {
    backend.stop();
  }
});
