import { chmodSync, mkdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import type { RuntimeAdmission } from "../runtime-reconciler";

const DEFAULT_DEADLINE_MS = 10 * 60 * 1_000;
const DEFAULT_MAX_ARTIFACT_BYTES = 64 * 1024 * 1024;
const DEFAULT_MAX_ARTIFACT_BYTES_TOTAL = 256 * 1024 * 1024;
const DEFAULT_MAX_TERMINAL_JOBS = 20;
const DEFAULT_TERMINAL_TTL_MS = 15 * 60 * 1_000;
const DEFAULT_POLL_MS = 1_000;

export type VideoJobInput = Readonly<{
  prompt: string;
  negativePrompt?: string;
  width?: number;
  height?: number;
  videoFrames?: number;
  fps?: number;
  seed?: number;
  outputFormat?: "webm" | "webp" | "avi";
}>;

export type VideoBackendJob =
  | Readonly<{ id: string; status: "queued" | "generating" }>
  | Readonly<{
      id: string;
      status: "completed";
      media: Readonly<{
        bytes: Uint8Array;
        mimeType: string;
        outputFormat: "webm" | "webp" | "avi";
        fps: number;
        frameCount: number;
      }>;
    }>
  | Readonly<{
      id: string;
      status: "failed" | "cancelled";
      errorCode?: string;
    }>;

/** Matches the local stable-diffusion.cpp video adapter without exposing it here. */
export type VideoJobBackend = Readonly<{
  submitVideo: (options: {
    input: VideoJobInput;
    signal?: AbortSignal;
  }) => Promise<Readonly<{ id: string; status: "queued" | "generating" }>>;
  getJob: (options: {
    id: string;
    signal?: AbortSignal;
  }) => Promise<VideoBackendJob>;
  cancelJob?: (options: {
    id: string;
    signal?: AbortSignal;
  }) => Promise<VideoBackendJob>;
}>;

export type VideoJobState =
  "queued" | "in_progress" | "completed" | "failed" | "cancelled";

export type VideoJobArtifact = Readonly<{
  byteLength: number;
  mimeType: string;
  outputFormat: "webm" | "webp" | "avi";
  fps: number;
  frameCount: number;
}>;

export type VideoJob =
  | Readonly<{
      id: string;
      state: "queued" | "in_progress";
      createdAtMs: number;
    }>
  | Readonly<{
      id: string;
      state: "completed";
      createdAtMs: number;
      terminalAtMs: number;
      artifact: VideoJobArtifact;
    }>
  | Readonly<{
      id: string;
      state: "failed";
      createdAtMs: number;
      terminalAtMs: number;
      failure: Error;
    }>
  | Readonly<{
      id: string;
      state: "cancelled";
      createdAtMs: number;
      terminalAtMs: number;
      reason: "backend" | "cancelled" | "deadline" | "shutdown";
    }>;

export type VideoJobStart =
  | Readonly<{ kind: "accepted"; job: VideoJob; terminal: Promise<VideoJob> }>
  | Readonly<{ kind: "busy" }>;

type VideoJobAdmission = Pick<RuntimeAdmission, "release">;

type VideoJobDisposition =
  | Readonly<{
      kind: "cancelled";
      reason: "cancelled" | "deadline" | "shutdown";
    }>
  | Readonly<{ kind: "failed"; failure: Error }>;

type StoredJob = {
  id: string;
  ownerId: string;
  createdAtMs: number;
  state: VideoJobState;
  controller: AbortController;
  deadlineController: AbortController;
  admission: VideoJobAdmission;
  directory: string;
  backendId: string | undefined;
  completion: Promise<VideoJobArtifact> | undefined;
  resolveTerminal: (job: VideoJob) => void;
  terminal: Promise<VideoJob>;
  terminalAtMs: number | undefined;
  artifact: VideoJobArtifact | undefined;
  pendingArtifactBytes: number | undefined;
  failure: Error | undefined;
  cancellationReason:
    "backend" | "cancelled" | "deadline" | "shutdown" | undefined;
  termination: Promise<void> | undefined;
};

export class VideoJobArtifactLimitError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "VideoJobArtifactLimitError";
  }
}

export class VideoBackendJobFailureError extends Error {
  constructor(code: string | undefined) {
    super(
      code === undefined
        ? "Video backend job failed."
        : `Video backend job failed: ${code}`,
    );
    this.name = "VideoBackendJobFailureError";
  }
}

export type VideoJobManagerOptions = Readonly<{
  backend: VideoJobBackend;
  temporaryDirectory: string;
  acquireAdmission: () => Promise<VideoJobAdmission | undefined>;
  supervisedStop: () => Promise<void>;
  now?: () => number;
  waitForPoll?: (options: { signal: AbortSignal }) => Promise<void>;
  waitForDeadline?: (options: {
    signal: AbortSignal;
    deadlineMs: number;
  }) => Promise<void>;
  onContainmentFailure: (options: {
    jobId: string;
    source: "runner" | "deadline";
    error: Error;
  }) => void;
  writeArtifact?: (options: {
    path: string;
    bytes: Uint8Array;
  }) => Promise<void>;
  removeArtifact?: (path: string) => void;
  removeJobDirectory?: (path: string) => void;
  deadlineMs?: number;
  maxArtifactBytes?: number;
  maxArtifactBytesTotal?: number;
  maxTerminalJobs?: number;
  terminalTtlMs?: number;
}>;

/** Owns the ephemeral lifecycle and private files for one local video job. */
export class VideoJobManager {
  private readonly jobsRoot: string;
  private readonly jobs = new Map<string, StoredJob>();
  private active: StoredJob | undefined;
  private stopping = false;
  private readonly now: () => number;
  private readonly waitForPoll: (options: {
    signal: AbortSignal;
  }) => Promise<void>;
  private readonly waitForDeadline: (options: {
    signal: AbortSignal;
    deadlineMs: number;
  }) => Promise<void>;
  private readonly deadlineMs: number;
  private readonly maxArtifactBytes: number;
  private readonly maxArtifactBytesTotal: number;
  private readonly maxTerminalJobs: number;
  private readonly terminalTtlMs: number;
  private readonly writeArtifact: (options: {
    path: string;
    bytes: Uint8Array;
  }) => Promise<void>;
  private readonly removeArtifact: (path: string) => void;
  private readonly removeJobDirectory: (path: string) => void;

  constructor(private readonly options: VideoJobManagerOptions) {
    this.jobsRoot = join(options.temporaryDirectory, "video-jobs");
    rmSync(this.jobsRoot, { recursive: true, force: true });
    mkdirSync(this.jobsRoot, { recursive: true, mode: 0o700 });
    chmodSync(this.jobsRoot, 0o700);
    this.now = options.now ?? Date.now;
    this.waitForPoll = options.waitForPoll ?? waitForPoll;
    this.waitForDeadline = options.waitForDeadline ?? waitForDeadline;
    this.writeArtifact = options.writeArtifact ?? writeArtifact;
    this.removeArtifact = options.removeArtifact ?? removeArtifact;
    this.removeJobDirectory = options.removeJobDirectory ?? removeJobDirectory;
    this.deadlineMs = positive(
      options.deadlineMs,
      DEFAULT_DEADLINE_MS,
      "deadlineMs",
    );
    this.maxArtifactBytes = positive(
      options.maxArtifactBytes,
      DEFAULT_MAX_ARTIFACT_BYTES,
      "maxArtifactBytes",
    );
    this.maxArtifactBytesTotal = positive(
      options.maxArtifactBytesTotal,
      DEFAULT_MAX_ARTIFACT_BYTES_TOTAL,
      "maxArtifactBytesTotal",
    );
    this.maxTerminalJobs = positive(
      options.maxTerminalJobs,
      DEFAULT_MAX_TERMINAL_JOBS,
      "maxTerminalJobs",
    );
    this.terminalTtlMs = positive(
      options.terminalTtlMs,
      DEFAULT_TERMINAL_TTL_MS,
      "terminalTtlMs",
    );
  }

  async start(options: {
    ownerId: string;
    input: VideoJobInput;
  }): Promise<VideoJobStart> {
    this.prune();
    if (this.stopping || this.active !== undefined) return { kind: "busy" };
    const admission = await this.options.acquireAdmission();
    if (admission === undefined || this.stopping || this.active !== undefined) {
      admission?.release();
      return { kind: "busy" };
    }

    let job: StoredJob;
    try {
      job = this.createJob(options.ownerId, admission);
    } catch (error) {
      admission.release();
      throw error;
    }
    this.active = job;
    this.jobs.set(job.id, job);
    void this.watchDeadline(job);
    void this.run(job, options.input);
    return Object.freeze({
      kind: "accepted",
      job: this.snapshot(job),
      terminal: job.terminal,
    });
  }

  get(options: { ownerId: string; id: string }): VideoJob | undefined {
    this.prune();
    const job = this.jobs.get(options.id);
    if (!job || job.ownerId !== options.ownerId) return undefined;
    return this.snapshot(job);
  }

  artifact(options: {
    ownerId: string;
    id: string;
  }): Readonly<{ path: string; metadata: VideoJobArtifact }> | undefined {
    this.prune();
    const job = this.jobs.get(options.id);
    if (!job || job.ownerId !== options.ownerId || !job.artifact)
      return undefined;
    return Object.freeze({
      path: join(job.directory, "artifact"),
      metadata: job.artifact,
    });
  }

  async cancel(options: {
    ownerId: string;
    id: string;
  }): Promise<VideoJob | undefined> {
    this.prune();
    const job = this.jobs.get(options.id);
    if (!job || job.ownerId !== options.ownerId) return undefined;
    await this.terminate(job, "cancelled");
    return this.snapshot(job);
  }

  async shutdown(): Promise<void> {
    this.prune();
    this.stopping = true;
    if (this.active) await this.terminate(this.active, "shutdown");
  }

  private createJob(ownerId: string, admission: VideoJobAdmission): StoredJob {
    const id = crypto.randomUUID();
    const directory = join(this.jobsRoot, id);
    mkdirSync(directory, { recursive: true, mode: 0o700 });
    chmodSync(directory, 0o700);
    let resolveTerminal = (_job: VideoJob) => {};
    const terminal = new Promise<VideoJob>((resolve) => {
      resolveTerminal = resolve;
    });
    return {
      id,
      ownerId,
      createdAtMs: this.now(),
      state: "queued",
      controller: new AbortController(),
      deadlineController: new AbortController(),
      admission,
      directory,
      backendId: undefined,
      completion: undefined,
      resolveTerminal,
      terminal,
      terminalAtMs: undefined,
      artifact: undefined,
      pendingArtifactBytes: undefined,
      failure: undefined,
      cancellationReason: undefined,
      termination: undefined,
    };
  }

  private async submit(job: StoredJob, input: VideoJobInput): Promise<void> {
    const submission = await this.options.backend.submitVideo({
      input,
      signal: job.controller.signal,
    });
    if (
      job.terminalAtMs !== undefined ||
      job.termination ||
      job.controller.signal.aborted
    ) {
      return;
    }
    job.backendId = submission.id;
    job.state = submission.status === "generating" ? "in_progress" : "queued";
  }

  private async run(job: StoredJob, input: VideoJobInput): Promise<void> {
    try {
      await this.submit(job, input);
      if (job.termination) return;
      await this.poll(job);
    } catch (error) {
      if (job.termination || job.controller.signal.aborted) return;
      try {
        await this.containFailure(job, toError(error));
      } catch (containmentError) {
        this.reportContainmentFailure(job, "runner", containmentError);
      }
    }
  }

  private async poll(job: StoredJob): Promise<void> {
    while (job.terminalAtMs === undefined && !job.termination) {
      const backendId = job.backendId;
      if (backendId === undefined)
        throw new Error("Video backend job ID is unavailable.");
      const update = await this.options.backend.getJob({
        id: backendId,
        signal: job.controller.signal,
      });
      await this.applyBackendUpdate(job, update);
      if (job.terminalAtMs !== undefined || job.termination) return;
      await this.waitForPoll({ signal: job.controller.signal });
    }
  }

  private async applyBackendUpdate(
    job: StoredJob,
    update: VideoBackendJob,
  ): Promise<void> {
    if (
      job.terminalAtMs !== undefined ||
      job.termination ||
      job.controller.signal.aborted
    ) {
      return;
    }
    if (job.backendId !== update.id) {
      throw new Error("Video backend returned a mismatched job ID.");
    }
    if (update.status === "queued" || update.status === "generating") {
      job.state = update.status === "generating" ? "in_progress" : "queued";
      return;
    }
    if (update.status === "completed") {
      const completion = this.persistArtifact(job, update.media);
      job.completion = completion;
      try {
        const artifact = await completion;
        if (job.terminalAtMs !== undefined || job.termination) return;
        this.finishCompleted(job, artifact);
      } finally {
        job.completion = undefined;
      }
      return;
    }
    if (update.status === "failed") {
      this.finishFailure(
        job,
        new VideoBackendJobFailureError(update.errorCode),
      );
      return;
    }
    this.finishCancelled(job, "backend");
  }

  private async terminate(
    job: StoredJob,
    reason: "cancelled" | "deadline" | "shutdown",
  ): Promise<void> {
    if (job.terminalAtMs !== undefined) return;
    await this.contain(job, { kind: "cancelled", reason });
  }

  private async containFailure(job: StoredJob, failure: Error): Promise<void> {
    if (job.terminalAtMs !== undefined) return;
    await this.contain(job, { kind: "failed", failure });
  }

  private async contain(
    job: StoredJob,
    disposition: VideoJobDisposition,
  ): Promise<void> {
    const termination =
      job.termination ??
      (job.termination = this.finishTermination(job, disposition));
    await termination;
  }

  private async finishTermination(
    job: StoredJob,
    disposition: VideoJobDisposition,
  ): Promise<void> {
    job.controller.abort();
    const completion = job.completion;
    let completionFailure: Error | undefined;
    if (completion) {
      try {
        await completion;
      } catch (error) {
        completionFailure = toError(error);
      }
      if (job.terminalAtMs !== undefined) return;
    }
    try {
      await this.options.supervisedStop();
    } catch (error) {
      job.termination = undefined;
      throw toError(error);
    }
    try {
      this.removeTransientArtifact(job);
    } catch (error) {
      this.finishFailure(job, toError(error));
      return;
    }
    if (completionFailure) {
      this.finishFailure(job, completionFailure);
      return;
    }
    this.finishDisposition(job, disposition);
  }

  private finishDisposition(
    job: StoredJob,
    disposition: VideoJobDisposition,
  ): void {
    if (disposition.kind === "failed") {
      this.finishFailure(job, disposition.failure);
      return;
    }
    this.finishCancelled(job, disposition.reason);
  }

  private async watchDeadline(job: StoredJob): Promise<void> {
    try {
      await this.waitForDeadline({
        signal: job.deadlineController.signal,
        deadlineMs: this.deadlineMs,
      });
    } catch (error) {
      if (isAbortError(error, job.deadlineController.signal)) return;
      try {
        await this.containFailure(job, toError(error));
      } catch (containmentError) {
        this.reportContainmentFailure(job, "deadline", containmentError);
      }
      return;
    }
    if (job.terminalAtMs !== undefined) return;
    if (!job.termination) {
      try {
        await this.terminate(job, "deadline");
      } catch (error) {
        this.reportContainmentFailure(job, "deadline", error);
      }
    }
  }

  private reportContainmentFailure(
    job: StoredJob,
    source: "runner" | "deadline",
    error: unknown,
  ): void {
    this.options.onContainmentFailure({
      jobId: job.id,
      source,
      error: toError(error),
    });
  }

  private async persistArtifact(
    job: StoredJob,
    media: Extract<VideoBackendJob, { status: "completed" }>["media"],
  ): Promise<VideoJobArtifact> {
    if (media.bytes.byteLength > this.maxArtifactBytes) {
      throw new VideoJobArtifactLimitError(
        "Video artifact exceeds the per-job limit.",
      );
    }
    this.makeRoomForArtifact(media.bytes.byteLength);
    job.pendingArtifactBytes = media.bytes.byteLength;
    const path = join(job.directory, "artifact");
    await this.writeArtifact({ path, bytes: media.bytes });
    chmodSync(path, 0o600);
    return Object.freeze({
      byteLength: media.bytes.byteLength,
      mimeType: media.mimeType,
      outputFormat: media.outputFormat,
      fps: media.fps,
      frameCount: media.frameCount,
    });
  }

  private removeTransientArtifact(job: StoredJob): void {
    const path = join(job.directory, "artifact");
    this.removeArtifact(path);
    job.pendingArtifactBytes = undefined;
  }

  private makeRoomForArtifact(byteLength: number): void {
    if (byteLength > this.maxArtifactBytesTotal) {
      throw new VideoJobArtifactLimitError(
        "Video artifact exceeds the aggregate limit.",
      );
    }
    this.prune();
    while (
      this.terminalJobs().length >= this.maxTerminalJobs ||
      this.artifactBytes() + byteLength > this.maxArtifactBytesTotal
    ) {
      const oldest = this.terminalJobs().at(0);
      if (!oldest) {
        throw new VideoJobArtifactLimitError(
          "Video artifact exceeds the aggregate limit.",
        );
      }
      this.remove(oldest);
    }
  }

  private finishCompleted(job: StoredJob, artifact: VideoJobArtifact): void {
    job.artifact = artifact;
    job.state = "completed";
    this.finish(job);
  }

  private finishFailure(job: StoredJob, failure: Error): void {
    job.failure = failure;
    job.state = "failed";
    this.finish(job);
  }

  private finishCancelled(
    job: StoredJob,
    reason: "backend" | "cancelled" | "deadline" | "shutdown",
  ): void {
    job.cancellationReason = reason;
    job.state = "cancelled";
    this.finish(job);
  }

  private finish(job: StoredJob): void {
    if (job.terminalAtMs !== undefined) return;
    job.terminalAtMs = this.now();
    job.deadlineController.abort();
    job.admission.release();
    if (this.active === job) this.active = undefined;
    const snapshot = this.snapshot(job);
    job.resolveTerminal(snapshot);
    this.prune();
  }

  private snapshot(job: StoredJob): VideoJob {
    if (job.state === "queued" || job.state === "in_progress") {
      return Object.freeze({
        id: job.id,
        state: job.state,
        createdAtMs: job.createdAtMs,
      });
    }
    if (job.state === "completed") {
      if (!job.artifact || job.terminalAtMs === undefined) {
        throw new Error("Completed video job is missing its artifact.");
      }
      return Object.freeze({
        id: job.id,
        state: "completed",
        createdAtMs: job.createdAtMs,
        terminalAtMs: job.terminalAtMs,
        artifact: job.artifact,
      });
    }
    if (job.state === "failed") {
      if (!job.failure || job.terminalAtMs === undefined) {
        throw new Error("Failed video job is missing its failure.");
      }
      return Object.freeze({
        id: job.id,
        state: "failed",
        createdAtMs: job.createdAtMs,
        terminalAtMs: job.terminalAtMs,
        failure: job.failure,
      });
    }
    if (job.terminalAtMs === undefined || !job.cancellationReason) {
      throw new Error("Cancelled video job is missing its terminal state.");
    }
    return Object.freeze({
      id: job.id,
      state: "cancelled",
      createdAtMs: job.createdAtMs,
      terminalAtMs: job.terminalAtMs,
      reason: job.cancellationReason,
    });
  }

  private prune(): void {
    const now = this.now();
    for (const job of this.terminalJobs()) {
      if (
        job.terminalAtMs !== undefined &&
        now - job.terminalAtMs >= this.terminalTtlMs
      ) {
        this.remove(job);
      }
    }
    while (this.terminalJobs().length > this.maxTerminalJobs) {
      const oldest = this.terminalJobs().at(0);
      if (!oldest) return;
      this.remove(oldest);
    }
  }

  private terminalJobs(): StoredJob[] {
    return [...this.jobs.values()]
      .filter((job) => job.terminalAtMs !== undefined)
      .sort(
        (left, right) => (left.terminalAtMs ?? 0) - (right.terminalAtMs ?? 0),
      );
  }

  private artifactBytes(): number {
    return this.terminalJobs().reduce(
      (total, job) =>
        total + (job.artifact?.byteLength ?? job.pendingArtifactBytes ?? 0),
      0,
    );
  }

  private remove(job: StoredJob): void {
    this.removeJobDirectory(job.directory);
    this.jobs.delete(job.id);
  }
}

function positive(
  value: number | undefined,
  fallback: number,
  name: string,
): number {
  const parsed = value ?? fallback;
  if (!Number.isSafeInteger(parsed) || parsed < 1) {
    throw new RangeError(`${name} must be a positive safe integer.`);
  }
  return parsed;
}

async function writeArtifact(options: {
  path: string;
  bytes: Uint8Array;
}): Promise<void> {
  await Bun.write(options.path, options.bytes);
}

function removeArtifact(path: string): void {
  rmSync(path, { force: true });
}

function removeJobDirectory(path: string): void {
  rmSync(path, { recursive: true, force: true });
}

async function waitForPoll(options: { signal: AbortSignal }): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const finish = () => {
      options.signal.removeEventListener("abort", abort);
      resolve();
    };
    const timer = setTimeout(finish, DEFAULT_POLL_MS);
    const abort = () => {
      clearTimeout(timer);
      options.signal.removeEventListener("abort", abort);
      reject(options.signal.reason);
    };
    options.signal.addEventListener("abort", abort, { once: true });
    if (options.signal.aborted) abort();
  });
}

async function waitForDeadline(options: {
  signal: AbortSignal;
  deadlineMs: number;
}): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const finish = () => {
      options.signal.removeEventListener("abort", abort);
      resolve();
    };
    const timer = setTimeout(finish, options.deadlineMs);
    const abort = () => {
      clearTimeout(timer);
      options.signal.removeEventListener("abort", abort);
      reject(options.signal.reason);
    };
    options.signal.addEventListener("abort", abort, { once: true });
    if (options.signal.aborted) abort();
  });
}

function toError(error: unknown): Error {
  return error instanceof Error ? error : new Error(String(error));
}

function isAbortError(error: unknown, signal: AbortSignal): boolean {
  if (error === signal.reason) return true;
  return error instanceof Error && error.name === "AbortError";
}
