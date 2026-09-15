import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  createApiKey,
  defaultConfig,
  resolveApiKey,
  saveConfig,
} from "../../../manager";
import { DatabaseSession } from "../../../db/client";
import {
  handleVideoGatewayRequest,
  type VideoGatewayHandlerDependencies,
  type VideoModelAdmissionProvider,
} from "./gateway-handler";
import {
  videoCreateRequestSchema,
  videoJobResponseSchema,
} from "./gateway-contract";
import {
  VideoJobManager,
  type VideoBackendJob,
  type VideoJobBackend,
  type VideoJobInput,
} from "./video-job-manager";

const VIDEO_MODEL = "wan2.1-t2v-1.3b-q8_0";

function deferred<Value>() {
  let resolve: (value: Value) => void = () => {};
  let reject: (error: unknown) => void = () => {};
  const promise = new Promise<Value>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function createCredentials(root: string) {
  const database = new DatabaseSession();
  const config = defaultConfig(root);
  saveConfig(database, config);
  const first = createApiKey(database, config, "first");
  const second = createApiKey(database, config, "second");
  const ownerId = (rawKey: string) => {
    const record = resolveApiKey(database, config, rawKey);
    if (!record) throw new Error("Expected active key.");
    return `api-key:${record.id}`;
  };
  return {
    database,
    config,
    firstOwnerId: ownerId(first.rawKey),
    secondOwnerId: ownerId(second.rawKey),
  };
}

function completed(id: string): VideoBackendJob {
  return {
    id,
    status: "completed",
    media: {
      bytes: Uint8Array.from([7, 8, 9]),
      mimeType: "video/x-msvideo",
      outputFormat: "avi",
      fps: 16,
      frameCount: 33,
    },
  };
}

function createAdmissionProvider(options: {
  released: () => void;
  stopped: () => Promise<void> | void;
  available?: () => boolean;
  ready?: Promise<void>;
}): VideoModelAdmissionProvider {
  return {
    admit: async () => {
      if (options.available && !options.available()) {
        return { kind: "not-configured" };
      }
      return {
        kind: "admitted",
        admission: {
          ready: options.ready ?? Promise.resolve(),
          release: options.released,
          cancel: () => {},
          supervisor: {
            kill: async () => await options.stopped(),
          },
        },
      };
    },
  };
}

function endpointDependencies(options: {
  request: Request;
  pathname: string;
  route: VideoGatewayHandlerDependencies["route"];
  ownerId: string;
  jobs: VideoJobManager;
  admissionProvider: VideoModelAdmissionProvider;
  onTerminal?: VideoGatewayHandlerDependencies["onTerminal"];
}): VideoGatewayHandlerDependencies {
  return {
    ...options,
    parseCreateRequest: async () => {
      const parsed = videoCreateRequestSchema.safeParse(
        await options.request.json(),
      );
      return parsed.success
        ? { success: true, data: parsed.data }
        : {
            success: false,
            response: Response.json(parsed.error, { status: 400 }),
          };
    },
    queueFailure: () => undefined,
    notConfigured: () => new Response(null, { status: 501 }),
    serviceUnavailable: () => new Response(null, { status: 503 }),
    modelNotFound: () => new Response(null, { status: 404 }),
    badRequest: () => new Response(null, { status: 400 }),
    methodNotAllowed: (allow) =>
      new Response(null, { status: 405, headers: { allow } }),
    routeNotFound: () => new Response(null, { status: 404 }),
    requestAborted: () => new Response(null, { status: 499 }),
    resourceUnavailable: () => new Response(null, { status: 503 }),
    onTerminal: options.onTerminal ?? (() => {}),
  };
}

function createRequest(ownerPrompt: string, signal?: AbortSignal): Request {
  return new Request("http://local.test/v1/videos", {
    method: "POST",
    headers: { "content-type": "application/json" },
    signal,
    body: JSON.stringify({
      model: VIDEO_MODEL,
      prompt: ownerPrompt,
      width: 320,
      height: 320,
      frames: 33,
      fps: 16,
    }),
  });
}

test("serves one owner’s completed local video through the typed route and deletes it", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-gateway-"));
  const credentials = createCredentials(root);
  const pollEntered = deferred<void>();
  const completion = deferred<VideoBackendJob>();
  const terminal = deferred<void>();
  let released = 0;
  let stopped = 0;
  let videoEnabled = false;
  const readiness = deferred<void>();
  const clientAbort = new AbortController();
  let submitted: VideoJobInput | undefined;
  const backend: VideoJobBackend = {
    async submitVideo({ input }) {
      submitted = input;
      return { id: "native-completed", status: "queued" };
    },
    async getJob() {
      pollEntered.resolve();
      return await completion.promise;
    },
  };
  const jobs = new VideoJobManager({
    backend,
    temporaryDirectory: root,
    onContainmentFailure: () => {},
    waitForDeadline: async ({ signal }) =>
      await new Promise<void>((_resolve, reject) => {
        signal.addEventListener("abort", () => reject(signal.reason), {
          once: true,
        });
      }),
  });

  try {
    const admissionProvider = createAdmissionProvider({
      released: () => (released += 1),
      stopped: () => {
        stopped += 1;
      },
      available: () => videoEnabled,
      ready: readiness.promise,
    });
    expect(await Bun.file(join(root, "video-jobs")).exists()).toBe(false);
    videoEnabled = true;
    const created = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest(
          "A paper kite over a field.",
          clientAbort.signal,
        ),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
        onTerminal: () => terminal.resolve(),
      }),
    );
    expect(created.status).toBe(202);
    const body = videoJobResponseSchema.parse(await created.json());
    const jobId = body.id;
    expect(body).toMatchObject({
      object: "localbase.video.job",
      status: "queued",
      created_at: expect.any(Number),
    });
    expect(submitted).toBeUndefined();
    clientAbort.abort();
    readiness.resolve();
    await pollEntered.promise;
    expect(submitted).toMatchObject({
      outputFormat: "avi",
      seed: 42,
      generation: { sampler: "euler", steps: 20, cfgScale: 6, flowShift: 3 },
    });

    completion.resolve(completed("native-completed"));
    await terminal.promise;

    const status = await handleVideoGatewayRequest(
      endpointDependencies({
        request: new Request(`http://local.test/v1/videos/${jobId}`),
        pathname: `/v1/videos/${jobId}`,
        route: "videoStatus",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(await status.json()).toMatchObject({
      id: jobId,
      status: "completed",
      content_type: "video/x-msvideo",
      bytes: 3,
      completed_at: expect.any(Number),
    });

    const crossOwner = await handleVideoGatewayRequest(
      endpointDependencies({
        request: new Request(`http://local.test/v1/videos/${jobId}`),
        pathname: `/v1/videos/${jobId}`,
        route: "videoStatus",
        ownerId: credentials.secondOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(crossOwner.status).toBe(404);

    const content = await handleVideoGatewayRequest(
      endpointDependencies({
        request: new Request(`http://local.test/v1/videos/${jobId}/content`),
        pathname: `/v1/videos/${jobId}/content`,
        route: "videoContent",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(content.headers.get("content-type")).toBe("video/x-msvideo");
    expect(content.headers.get("x-content-type-options")).toBe("nosniff");
    expect(await content.bytes()).toEqual(Uint8Array.from([7, 8, 9]));

    const deleted = await handleVideoGatewayRequest(
      endpointDependencies({
        request: new Request(`http://local.test/v1/videos/${jobId}`, {
          method: "DELETE",
        }),
        pathname: `/v1/videos/${jobId}`,
        route: "videoStatus",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(deleted.status).toBe(204);
    expect(released).toBe(1);
    expect(stopped).toBe(0);
  } finally {
    credentials.database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not create a video job for a request aborted before acceptance", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-preaccept-"));
  const credentials = createCredentials(root);
  const requestAbort = new AbortController();
  let submissions = 0;
  const jobs = new VideoJobManager({
    backend: {
      async submitVideo() {
        submissions += 1;
        return { id: "must-not-submit", status: "queued" };
      },
      async getJob() {
        return { id: "must-not-submit", status: "cancelled" };
      },
    },
    temporaryDirectory: root,
    onContainmentFailure: () => {},
  });
  const admissionProvider = createAdmissionProvider({
    released: () => {},
    stopped: () => {},
  });

  try {
    requestAbort.abort();
    const response = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest("Do not accept this job.", requestAbort.signal),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(response.status).toBe(499);
    expect(submissions).toBe(0);
    expect(await Bun.file(join(root, "video-jobs")).exists()).toBe(false);
  } finally {
    credentials.database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("reserves one accepted video job while admission warms and clears it after rejection", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-admission-"));
  const credentials = createCredentials(root);
  const admissionEntered = deferred<void>();
  const rejectAdmission = deferred<never>();
  const firstTerminal = deferred<void>();
  let admissions = 0;
  const jobs = new VideoJobManager({
    backend: {
      async submitVideo() {
        return { id: "native-after-admission", status: "queued" };
      },
      async getJob() {
        return { id: "native-after-admission", status: "cancelled" };
      },
    },
    temporaryDirectory: root,
    onContainmentFailure: () => {},
  });
  const admissionProvider: VideoModelAdmissionProvider = {
    admit: async () => {
      admissions += 1;
      if (admissions === 1) {
        admissionEntered.resolve();
        return await rejectAdmission.promise;
      }
      return {
        kind: "admitted",
        admission: {
          ready: Promise.resolve(),
          release: () => {},
          cancel: () => {},
          supervisor: { kill: async () => {} },
        },
      };
    },
  };

  try {
    const first = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest("Wait for admission."),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
        onTerminal: () => firstTerminal.resolve(),
      }),
    );
    expect(first.status).toBe(202);
    await admissionEntered.promise;

    const busy = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest("Do not queue behind admission."),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(busy.status).toBe(429);
    expect(admissions).toBe(1);

    rejectAdmission.reject(new Error("admission rejected"));
    await firstTerminal.promise;

    const recovered = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest("Accept after admission rejection."),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(recovered.status).toBe(202);
  } finally {
    credentials.database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("cancels a generating video through the route only after supervised stop", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-cancel-route-"));
  const credentials = createCredentials(root);
  const pollEntered = deferred<void>();
  const stopEntered = deferred<void>();
  const allowStop = deferred<void>();
  const recoveredTerminal = deferred<void>();
  let released = 0;
  let submissions = 0;
  const backend: VideoJobBackend = {
    async submitVideo() {
      submissions += 1;
      if (submissions === 2) {
        return { id: "native-recovered", status: "queued" };
      }
      return { id: "native-cancelled", status: "generating" };
    },
    async getJob({ id, signal }) {
      if (id === "native-recovered") return completed(id);
      pollEntered.resolve();
      return await new Promise<VideoBackendJob>((_resolve, reject) => {
        signal?.addEventListener("abort", () => reject(signal.reason), {
          once: true,
        });
      });
    },
  };
  const jobs = new VideoJobManager({
    backend,
    temporaryDirectory: root,
    onContainmentFailure: () => {},
    waitForDeadline: async ({ signal }) =>
      await new Promise<void>((_resolve, reject) => {
        signal.addEventListener("abort", () => reject(signal.reason), {
          once: true,
        });
      }),
  });

  try {
    const admissionProvider = createAdmissionProvider({
      released: () => (released += 1),
      stopped: async () => {
        stopEntered.resolve();
        await allowStop.promise;
      },
    });
    const created = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest("Cancel a local render."),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    const jobId = videoJobResponseSchema.parse(await created.json()).id;
    await pollEntered.promise;

    const cancellation = handleVideoGatewayRequest(
      endpointDependencies({
        request: new Request(`http://local.test/v1/videos/${jobId}/cancel`, {
          method: "POST",
        }),
        pathname: `/v1/videos/${jobId}/cancel`,
        route: "videoCancel",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    await stopEntered.promise;
    expect(released).toBe(0);
    allowStop.resolve();
    const cancelled = await cancellation;
    expect(await cancelled.json()).toMatchObject({
      id: jobId,
      status: "cancelled",
      cancellation_reason: "cancelled",
    });
    expect(released).toBe(1);

    const deleted = await handleVideoGatewayRequest(
      endpointDependencies({
        request: new Request(`http://local.test/v1/videos/${jobId}`, {
          method: "DELETE",
        }),
        pathname: `/v1/videos/${jobId}`,
        route: "videoStatus",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
      }),
    );
    expect(deleted.status).toBe(204);

    const recovered = await handleVideoGatewayRequest(
      endpointDependencies({
        request: createRequest("Render after the supervised stop."),
        pathname: "/v1/videos",
        route: "videoCreate",
        ownerId: credentials.firstOwnerId,
        jobs,
        admissionProvider,
        onTerminal: () => recoveredTerminal.resolve(),
      }),
    );
    expect(recovered.status).toBe(202);
    await recoveredTerminal.promise;
    expect(submissions).toBe(2);
    expect(released).toBe(2);
  } finally {
    credentials.database.close();
    rmSync(root, { recursive: true, force: true });
  }
});
