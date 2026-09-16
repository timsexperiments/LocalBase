import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  VideoJobArtifactLimitError,
  VideoJobManager,
  type VideoBackendJob,
  type VideoJobBackend,
  type VideoJobManagerOptions,
} from "./video-job-manager";

type VideoJobManagerTestOptions = Omit<
  VideoJobManagerOptions,
  "onContainmentFailure"
> & {
  onContainmentFailure?: VideoJobManagerOptions["onContainmentFailure"];
};

function createManager(options: VideoJobManagerTestOptions): VideoJobManager {
  return new VideoJobManager({
    ...options,
    onContainmentFailure: options.onContainmentFailure ?? (() => {}),
  });
}

function deferred<Value>() {
  let resolve: (value: Value) => void = () => {};
  let reject: (error: unknown) => void = () => {};
  const promise = new Promise<Value>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function completed(id: string, bytes = Uint8Array.from([1, 2, 3])) {
  return {
    id,
    status: "completed",
    media: {
      bytes,
      mimeType: "video/webm",
      outputFormat: "webm",
      fps: 16,
      frameCount: 33,
    },
  } satisfies VideoBackendJob;
}

function admissionCounter() {
  let acquired = 0;
  let released = 0;
  return {
    acquire: async () => {
      acquired += 1;
      return {
        ready: Promise.resolve(),
        release: () => (released += 1),
      };
    },
    snapshot: () => ({ acquired, released }),
  };
}

test("keeps completed jobs private, clears them on restart, and prunes terminal artifacts", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-job-"));
  const pollEntered = deferred<void>();
  const result = deferred<VideoBackendJob>();
  const admission = admissionCounter();
  let now = 0;
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-1", status: "queued" };
    },
    async getJob() {
      pollEntered.resolve();
      return await result.promise;
    },
    async cancelJob() {
      return { id: "native-1", status: "cancelled" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {},
    now: () => now,
    terminalTtlMs: 15,
  });

  try {
    expect(await Bun.file(join(root, "video-jobs")).exists()).toBe(false);
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "A paper kite over a field." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    expect(
      await manager.start({ ownerId: "key-b", input: { prompt: "busy" } }),
    ).toEqual({
      kind: "busy",
    });

    await pollEntered.promise;
    result.resolve(completed("native-1"));
    const terminal = await started.terminal;
    expect(terminal).toMatchObject({
      state: "completed",
      artifact: { byteLength: 3 },
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });

    const artifact = manager.artifact({ ownerId: "key-a", id: started.job.id });
    if (!artifact) throw new Error("Expected completed artifact.");
    expect(await Bun.file(artifact.path).bytes()).toEqual(
      Uint8Array.from([1, 2, 3]),
    );
    expect(statSync(join(root, "video-jobs")).mode & 0o777).toBe(0o700);
    expect(statSync(artifact.path).mode & 0o777).toBe(0o600);
    expect(
      manager.get({ ownerId: "key-b", id: started.job.id }),
    ).toBeUndefined();
    expect(
      manager.artifact({ ownerId: "key-b", id: started.job.id }),
    ).toBeUndefined();
    expect(manager.delete({ ownerId: "key-b", id: started.job.id })).toBe(
      false,
    );
    expect(manager.delete({ ownerId: "key-a", id: started.job.id })).toBe(true);
    expect(
      manager.get({ ownerId: "key-a", id: started.job.id }),
    ).toBeUndefined();
    expect(await Bun.file(artifact.path).exists()).toBe(false);

    const restarted = createManager({
      backend: {
        async submitVideo() {
          return { id: "native-2", status: "queued" };
        },
        async getJob() {
          return completed("native-2");
        },
        async cancelJob() {
          return { id: "native-2", status: "cancelled" };
        },
      },
      temporaryDirectory: root,
      acquireAdmission: admission.acquire,
      supervisedStop: async () => {},
      now: () => now,
      terminalTtlMs: 15,
    });
    expect(await Bun.file(join(root, "video-jobs")).exists()).toBe(false);
    expect(
      restarted.get({ ownerId: "key-a", id: started.job.id }),
    ).toBeUndefined();
    expect(await Bun.file(artifact.path).exists()).toBe(false);

    const fresh = await restarted.start({
      ownerId: "key-a",
      input: { prompt: "A fresh job after restart." },
    });
    if (fresh.kind !== "accepted") throw new Error("Expected admission.");
    await fresh.terminal;
    const freshArtifact = restarted.artifact({
      ownerId: "key-a",
      id: fresh.job.id,
    });
    if (!freshArtifact) throw new Error("Expected completed artifact.");
    now = 15;
    expect(
      restarted.get({ ownerId: "key-a", id: fresh.job.id }),
    ).toBeUndefined();
    expect(await Bun.file(freshArtifact.path).exists()).toBe(false);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("holds admission through cancellation until the backend is supervised stopped", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-cancel-"));
  const pollEntered = deferred<void>();
  const stopEntered = deferred<void>();
  const releaseStop = deferred<void>();
  const admission = admissionCounter();
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-2", status: "generating" };
    },
    async getJob(options) {
      pollEntered.resolve();
      return await new Promise<VideoBackendJob>((_resolve, reject) => {
        const abort = () => reject(options.signal?.reason);
        options.signal?.addEventListener("abort", abort, { once: true });
      });
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      await releaseStop.promise;
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Cancel this render." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await pollEntered.promise;

    const cancellation = manager.cancel({
      ownerId: "key-a",
      id: started.job.id,
    });
    await stopEntered.promise;
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });

    releaseStop.resolve();
    await expect(cancellation).resolves.toMatchObject({
      state: "cancelled",
      reason: "cancelled",
    });
    await expect(started.terminal).resolves.toMatchObject({
      state: "cancelled",
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("cancels an admission warmup before backend submission", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-warmup-cancel-"));
  const admissionStarted = deferred<void>();
  let submissions = 0;
  const manager = createManager({
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
    acquireAdmission: async ({ signal }) => {
      admissionStarted.resolve();
      return await new Promise<undefined>((_resolve, reject) => {
        signal.addEventListener("abort", () => reject(signal.reason), {
          once: true,
        });
      });
    },
    supervisedStop: async () => {},
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Cancel during warmup." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await admissionStarted.promise;
    await expect(
      manager.cancel({ ownerId: "key-a", id: started.job.id }),
    ).resolves.toMatchObject({ state: "cancelled" });
    expect(submissions).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("stops a late-acquired admission before releasing it", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-late-admission-"));
  const admissionEntered = deferred<void>();
  const lateAdmission = deferred<{
    ready: Promise<void>;
    release: () => void;
  }>();
  const stopEntered = deferred<void>();
  const releaseStop = deferred<void>();
  let releases = 0;
  let stops = 0;
  let submissions = 0;
  const manager = createManager({
    backend: {
      async submitVideo() {
        submissions += 1;
        throw new Error("A cancelled warmup must not submit.");
      },
      async getJob() {
        throw new Error("A cancelled warmup must not poll.");
      },
    },
    temporaryDirectory: root,
    acquireAdmission: async () => {
      admissionEntered.resolve();
      return await lateAdmission.promise;
    },
    supervisedStop: async () => {
      stops += 1;
      stopEntered.resolve();
      await releaseStop.promise;
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Contain a late admission." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await admissionEntered.promise;

    const cancellation = manager.cancel({
      ownerId: "key-a",
      id: started.job.id,
    });
    lateAdmission.resolve({
      ready: Promise.resolve(),
      release: () => (releases += 1),
    });
    await stopEntered.promise;
    expect(releases).toBe(0);
    releaseStop.resolve();
    await expect(cancellation).resolves.toMatchObject({ state: "cancelled" });
    await expect(started.terminal).resolves.toMatchObject({
      state: "cancelled",
    });
    expect(stops).toBe(1);
    expect(submissions).toBe(0);
    expect(releases).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("waits for a late successful submission before cancelling and releasing", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-submit-race-"));
  const submitEntered = deferred<void>();
  const submitResult = deferred<{ id: string; status: "queued" }>();
  const stopEntered = deferred<void>();
  const releaseStop = deferred<void>();
  const admission = admissionCounter();
  const backend: VideoJobBackend = {
    async submitVideo() {
      submitEntered.resolve();
      return await submitResult.promise;
    },
    async getJob() {
      throw new Error("Cancelled submission must not poll.");
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      await releaseStop.promise;
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Cancel before submission settles." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await submitEntered.promise;

    const cancellation = manager.cancel({
      ownerId: "key-a",
      id: started.job.id,
    });
    expect(
      await manager.start({ ownerId: "key-b", input: { prompt: "busy" } }),
    ).toEqual({ kind: "busy" });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });

    await stopEntered.promise;
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });

    releaseStop.resolve();
    await expect(cancellation).resolves.toMatchObject({
      state: "cancelled",
      reason: "cancelled",
    });
    submitResult.resolve({ id: "native-late", status: "queued" });
    await Promise.resolve();
    expect(manager.get({ ownerId: "key-a", id: started.job.id })).toMatchObject(
      { state: "cancelled" },
    );
    expect(
      await Bun.file(
        join(root, "video-jobs", started.job.id, "artifact"),
      ).exists(),
    ).toBe(false);
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("ignores late poll updates after cancellation terminalizes the job", async () => {
  const updates: readonly VideoBackendJob[] = [
    { id: "native-late-poll", status: "generating" },
    { id: "native-late-poll", status: "failed" },
    completed("native-late-poll"),
  ];

  for (const update of updates) {
    const root = mkdtempSync(join(tmpdir(), "localbase-video-late-poll-"));
    const pollEntered = deferred<void>();
    const result = deferred<VideoBackendJob>();
    const admission = admissionCounter();
    const backend: VideoJobBackend = {
      async submitVideo() {
        return { id: "native-late-poll", status: "generating" };
      },
      async getJob() {
        pollEntered.resolve();
        return await result.promise;
      },
    };
    const manager = createManager({
      backend,
      temporaryDirectory: root,
      acquireAdmission: admission.acquire,
      supervisedStop: async () => {},
    });

    try {
      const started = await manager.start({
        ownerId: "key-a",
        input: { prompt: "Ignore late backend state." },
      });
      if (started.kind !== "accepted") throw new Error("Expected admission.");
      await pollEntered.promise;
      await expect(
        manager.cancel({ ownerId: "key-a", id: started.job.id }),
      ).resolves.toMatchObject({ state: "cancelled" });

      result.resolve(update);
      await Promise.resolve();
      expect(
        manager.get({ ownerId: "key-a", id: started.job.id }),
      ).toMatchObject({ state: "cancelled" });
      expect(
        manager.artifact({ ownerId: "key-a", id: started.job.id }),
      ).toBeUndefined();
      expect(
        await Bun.file(
          join(root, "video-jobs", started.job.id, "artifact"),
        ).exists(),
      ).toBe(false);
      expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  }
});

test("contains a poll failure before releasing admission", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-poll-failure-"));
  const stopEntered = deferred<void>();
  const releaseStop = deferred<void>();
  const admission = admissionCounter();
  const pollFailure = new Error("backend status request failed");
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-poll-failure", status: "generating" };
    },
    async getJob() {
      throw pollFailure;
    },
    async cancelJob() {
      return { id: "native-poll-failure", status: "generating" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      await releaseStop.promise;
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Contain the unknown backend state." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");

    await stopEntered.promise;
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });
    releaseStop.resolve();
    await expect(started.terminal).resolves.toMatchObject({
      state: "failed",
      failure: pollFailure,
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("waits for completed artifact persistence before settling cancellation", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-completion-race-"));
  const writeEntered = deferred<void>();
  const releaseWrite = deferred<void>();
  const admission = admissionCounter();
  let cancels = 0;
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-completed", status: "generating" };
    },
    async getJob() {
      return completed("native-completed");
    },
    async cancelJob() {
      cancels += 1;
      return { id: "native-completed", status: "cancelled" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {},
    writeArtifact: async (options) => {
      writeEntered.resolve();
      await releaseWrite.promise;
      await Bun.write(options.path, options.bytes);
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Finish before cancellation." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await writeEntered.promise;

    const cancellation = manager.cancel({
      ownerId: "key-a",
      id: started.job.id,
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });
    releaseWrite.resolve();

    await expect(cancellation).resolves.toMatchObject({ state: "cancelled" });
    expect(await started.terminal).toMatchObject({ state: "cancelled" });
    expect(cancels).toBe(0);
    expect(manager.get({ ownerId: "key-a", id: started.job.id })).toMatchObject(
      { state: "cancelled" },
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("contains an artifact write failure before releasing cancellation admission", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-write-failure-"));
  const writeEntered = deferred<void>();
  const writeCompleted = deferred<void>();
  const rejectWrite = deferred<void>();
  const stopEntered = deferred<void>();
  const releaseStop = deferred<void>();
  const admission = admissionCounter();
  const writeFailure = new Error("artifact write failed");
  let submissions = 0;
  const backend: VideoJobBackend = {
    async submitVideo() {
      submissions += 1;
      return {
        id:
          submissions === 1 ? "native-write-failure" : "native-write-recovered",
        status: "generating",
      };
    },
    async getJob({ id }) {
      return completed(id);
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      await releaseStop.promise;
    },
    writeArtifact: async (options) => {
      if (submissions !== 1) {
        await Bun.write(options.path, options.bytes);
        return;
      }
      writeEntered.resolve();
      await Bun.write(options.path, options.bytes);
      writeCompleted.resolve();
      await rejectWrite.promise;
      throw writeFailure;
    },
    maxArtifactBytesTotal: 3,
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Contain artifact persistence failure." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await writeEntered.promise;
    await writeCompleted.promise;
    expect(
      await Bun.file(
        join(root, "video-jobs", started.job.id, "artifact"),
      ).exists(),
    ).toBe(true);

    const cancellation = manager.cancel({
      ownerId: "key-a",
      id: started.job.id,
    });
    rejectWrite.resolve();
    await stopEntered.promise;
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });

    releaseStop.resolve();
    await expect(cancellation).resolves.toMatchObject({
      state: "failed",
      failure: writeFailure,
    });
    expect(await started.terminal).toMatchObject({
      state: "failed",
      failure: writeFailure,
    });
    expect(
      await Bun.file(
        join(root, "video-jobs", started.job.id, "artifact"),
      ).exists(),
    ).toBe(false);
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });

    const recovered = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Reuse the released artifact capacity." },
    });
    if (recovered.kind !== "accepted") throw new Error("Expected admission.");
    await expect(recovered.terminal).resolves.toMatchObject({
      state: "completed",
    });
    expect(admission.snapshot()).toEqual({ acquired: 2, released: 2 });
    expect(manager.delete({ ownerId: "key-a", id: started.job.id })).toBe(true);
    expect(
      manager.get({ ownerId: "key-a", id: started.job.id }),
    ).toBeUndefined();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("retains a failed transient cleanup's known bytes until terminal pruning succeeds", async () => {
  const root = mkdtempSync(
    join(tmpdir(), "localbase-video-cleanup-accounting-"),
  );
  const writeCompleted = deferred<void>();
  const releaseWrite = deferred<void>();
  const admission = admissionCounter();
  let submissions = 0;
  let now = 0;
  let failArtifactCleanup = true;
  let failTerminalPrune = true;
  const manager = createManager({
    backend: {
      async submitVideo() {
        submissions += 1;
        return { id: `native-cleanup-${submissions}`, status: "generating" };
      },
      async getJob({ id }) {
        return completed(
          id,
          submissions === 1 ? Uint8Array.from([1, 2, 3]) : Uint8Array.of(4),
        );
      },
    },
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {},
    writeArtifact: async (options) => {
      await Bun.write(options.path, options.bytes);
      if (submissions === 1) {
        writeCompleted.resolve();
        await releaseWrite.promise;
      }
    },
    removeArtifact(path) {
      if (failArtifactCleanup) {
        throw new Error("artifact cleanup denied");
      }
      rmSync(path, { force: true });
    },
    removeJobDirectory(path) {
      if (failTerminalPrune) {
        throw new Error("terminal prune denied");
      }
      rmSync(path, { recursive: true, force: true });
    },
    maxArtifactBytesTotal: 3,
    terminalTtlMs: 1,
    now: () => now,
  });

  let firstDirectory = "";
  try {
    const first = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Keep the partial private file accounted." },
    });
    if (first.kind !== "accepted") throw new Error("Expected admission.");
    firstDirectory = join(root, "video-jobs", first.job.id);
    await writeCompleted.promise;
    const cancellation = manager.cancel({ ownerId: "key-a", id: first.job.id });
    releaseWrite.resolve();
    await expect(cancellation).resolves.toMatchObject({ state: "failed" });
    expect(await Bun.file(join(firstDirectory, "artifact")).exists()).toBe(
      true,
    );

    const second = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Must not bypass the retained artifact budget." },
    });
    if (second.kind !== "accepted") throw new Error("Expected admission.");
    await expect(second.terminal).resolves.toMatchObject({ state: "failed" });
    expect(manager.get({ ownerId: "key-a", id: first.job.id })).toMatchObject({
      state: "failed",
    });

    now = 1;
    expect(() => manager.get({ ownerId: "key-a", id: first.job.id })).toThrow();
    failArtifactCleanup = false;
    failTerminalPrune = false;
    expect(manager.get({ ownerId: "key-a", id: first.job.id })).toBeUndefined();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("stops a hung submission without waiting for its backend ID", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-submit-deadline-"));
  const submitEntered = deferred<void>();
  const stopEntered = deferred<void>();
  const releaseStop = deferred<void>();
  const admission = admissionCounter();
  let submissionAborted = false;
  const backend: VideoJobBackend = {
    async submitVideo(options) {
      submitEntered.resolve();
      options.signal?.addEventListener(
        "abort",
        () => (submissionAborted = true),
        { once: true },
      );
      return await new Promise(() => {});
    },
    async getJob() {
      throw new Error("Hung submission must not poll.");
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      await releaseStop.promise;
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Bound this submission." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await submitEntered.promise;

    const cancellation = manager.cancel({
      ownerId: "key-a",
      id: started.job.id,
    });
    await stopEntered.promise;
    expect(submissionAborted).toBe(true);
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });

    releaseStop.resolve();
    await expect(cancellation).resolves.toMatchObject({
      state: "cancelled",
      reason: "cancelled",
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves the admission when supervised stop fails", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-stop-failure-"));
  const pollEntered = deferred<void>();
  const lateResult = deferred<VideoBackendJob>();
  const admission = admissionCounter();
  const stopFailure = new Error("runtime stop failed");
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-stop", status: "generating" };
    },
    async getJob() {
      pollEntered.resolve();
      return await lateResult.promise;
    },
    async cancelJob() {
      return { id: "native-stop", status: "generating" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      throw stopFailure;
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Keep the lease on failure." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await pollEntered.promise;
    await expect(
      manager.cancel({ ownerId: "key-a", id: started.job.id }),
    ).rejects.toBe(stopFailure);
    lateResult.resolve(completed("native-stop"));
    await Promise.resolve();
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });
    expect(manager.get({ ownerId: "key-a", id: started.job.id })).toMatchObject(
      { state: "in_progress" },
    );
    expect(
      manager.artifact({ ownerId: "key-a", id: started.job.id }),
    ).toBeUndefined();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("retains ownership when background poll containment cannot stop", async () => {
  const root = mkdtempSync(
    join(tmpdir(), "localbase-video-poll-stop-failure-"),
  );
  const stopEntered = deferred<void>();
  const reported = deferred<{
    jobId: string;
    source: "runner" | "deadline";
    error: Error;
  }>();
  const admission = admissionCounter();
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-poll-stop", status: "generating" };
    },
    async getJob() {
      throw new Error("status transport failed");
    },
    async cancelJob() {
      return { id: "native-poll-stop", status: "generating" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      throw new Error("stop failed");
    },
    onContainmentFailure: (failure) => reported.resolve(failure),
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Do not release after a failed stop." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await stopEntered.promise;
    await expect(reported.promise).resolves.toMatchObject({
      jobId: started.job.id,
      source: "runner",
      error: new Error("stop failed"),
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });
    expect(manager.get({ ownerId: "key-a", id: started.job.id })).toMatchObject(
      { state: "in_progress" },
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("retains ownership when deadline containment cannot stop", async () => {
  const root = mkdtempSync(
    join(tmpdir(), "localbase-video-deadline-stop-failure-"),
  );
  const pollEntered = deferred<void>();
  const deadline = deferred<void>();
  const stopEntered = deferred<void>();
  const reported = deferred<{
    jobId: string;
    source: "runner" | "deadline";
    error: Error;
  }>();
  const admission = admissionCounter();
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-deadline-stop", status: "generating" };
    },
    async getJob() {
      pollEntered.resolve();
      return await new Promise(() => {});
    },
    async cancelJob() {
      return { id: "native-deadline-stop", status: "generating" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {
      stopEntered.resolve();
      throw new Error("stop failed");
    },
    onContainmentFailure: (failure) => reported.resolve(failure),
    waitForDeadline: async () => await deadline.promise,
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Keep the deadline lease on stop failure." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await pollEntered.promise;
    deadline.resolve();
    await stopEntered.promise;
    await expect(reported.promise).resolves.toMatchObject({
      jobId: started.job.id,
      source: "deadline",
      error: new Error("stop failed"),
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 0 });
    expect(manager.get({ ownerId: "key-a", id: started.job.id })).toMatchObject(
      { state: "in_progress" },
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("cancels at its injected deadline and preserves artifact limits", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-deadline-"));
  const pollWaitEntered = deferred<void>();
  const deadline = deferred<void>();
  const admission = admissionCounter();
  let now = 0;
  const backend: VideoJobBackend = {
    async submitVideo() {
      return { id: "native-3", status: "queued" };
    },
    async getJob() {
      return { id: "native-3", status: "queued" };
    },
  };
  const manager = createManager({
    backend,
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {},
    now: () => now,
    deadlineMs: 10,
    waitForDeadline: async () => await deadline.promise,
    waitForPoll: async (options) => {
      pollWaitEntered.resolve();
      await new Promise<void>((_resolve, reject) => {
        options.signal.addEventListener("abort", () => reject(), {
          once: true,
        });
      });
    },
  });

  try {
    const started = await manager.start({
      ownerId: "key-a",
      input: { prompt: "Time out this render." },
    });
    if (started.kind !== "accepted") throw new Error("Expected admission.");
    await pollWaitEntered.promise;
    now = 10;
    deadline.resolve();
    await expect(started.terminal).resolves.toMatchObject({
      state: "cancelled",
      reason: "deadline",
    });
    expect(admission.snapshot()).toEqual({ acquired: 1, released: 1 });

    const oversized = createManager({
      backend: {
        ...backend,
        async getJob() {
          return completed("native-3", Uint8Array.from([1, 2, 3]));
        },
      },
      temporaryDirectory: root,
      acquireAdmission: admission.acquire,
      supervisedStop: async () => {},
      maxArtifactBytes: 2,
    });
    const tooLarge = await oversized.start({
      ownerId: "key-a",
      input: { prompt: "Too large." },
    });
    if (tooLarge.kind !== "accepted") throw new Error("Expected admission.");
    await expect(tooLarge.terminal).resolves.toMatchObject({
      state: "failed",
      failure: expect.any(VideoJobArtifactLimitError),
    });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects before admission when private job directory creation fails", async () => {
  const root = mkdtempSync(
    join(tmpdir(), "localbase-video-directory-failure-"),
  );
  const admission = admissionCounter();
  const manager = createManager({
    backend: {
      async submitVideo() {
        return { id: "never", status: "queued" };
      },
      async getJob() {
        return { id: "never", status: "cancelled" };
      },
      async cancelJob() {
        return { id: "never", status: "cancelled" };
      },
    },
    temporaryDirectory: root,
    acquireAdmission: admission.acquire,
    supervisedStop: async () => {},
  });

  try {
    await Bun.write(join(root, "video-jobs"), "not a directory");
    await expect(
      manager.start({
        ownerId: "key-a",
        input: { prompt: "Fail before submitting." },
      }),
    ).rejects.toThrow();
    expect(admission.snapshot()).toEqual({ acquired: 0, released: 0 });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
