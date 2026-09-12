import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { ILogger } from "../observability/logging";
import { guardianProcessCommand } from "./backend-guardian";
import type { ModalityLifecycleState } from "./health";
import type {
  MemorySafetyController,
  RuntimeMemoryReservation,
} from "./memory-controller";
import { gibibyte, type RuntimeMemoryDemand } from "./memory-safety";
import { stopNativeProcess } from "./native-process";

export { NativeProcessStopError as SpeechChildStopError } from "./native-process";

export const SPEECH_MAX_INPUT_CHARACTERS = 256;
export const SPEECH_MAX_FRAMES = 256;
export const SPEECH_SAMPLES_PER_FRAME = 1_920;
export const SPEECH_SAMPLE_RATE_HZ = 24_000;
export const SPEECH_MAX_OUTPUT_BYTES = 1024 * 1024;
export const SPEECH_TIMEOUT_MS = 120_000;

const CHILD_STOP_GRACE_MS = 500;
const MAX_NATIVE_DIAGNOSTIC_BYTES = 64 * 1024;
const SPEECH_MEMORY_DEMAND: RuntimeMemoryDemand = Object.freeze({
  unifiedBytes: 8 * gibibyte,
  hostBytes: 8 * gibibyte,
  acceleratorBytes: 8 * gibibyte,
  confidence: "estimated",
});

export type SpeechGenerationInput = Readonly<{
  text: string;
  signal?: AbortSignal;
}>;

export type SpeechPreparation = Readonly<{
  binaryPath: string;
  modelPath: string;
  projectorPath: string;
}>;

export class SpeechGenerationAbortedError extends Error {
  constructor() {
    super("Speech generation was cancelled.");
    this.name = "SpeechGenerationAbortedError";
  }
}

export class SpeechGenerationTimeoutError extends Error {
  constructor() {
    super("Speech generation exceeded the 120 second limit.");
    this.name = "SpeechGenerationTimeoutError";
  }
}

export class SpeechOutputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "SpeechOutputError";
  }
}

export type ValidatedWav = Readonly<{
  bytes: Uint8Array;
  sampleCount: number;
}>;

type ActiveGeneration = {
  cancellation: AbortController;
  process?: Bun.Subprocess;
  guardian?: Bun.Subprocess;
  done: Promise<void>;
  finish: () => void;
  stopFailed: Promise<unknown>;
  reportStopFailure: (error: unknown) => void;
};

type SpeechSupervisorOptions = Readonly<{
  runtimeId: string;
  root: string;
  modelId: string;
  logger: ILogger;
  memorySafety: MemorySafetyController;
  prepare: () => Promise<SpeechPreparation>;
  spawn?: (command: string[], cwd: string) => Bun.Subprocess;
  spawnGuardian?: (command: string[]) => Bun.Subprocess | undefined;
  writePrivateFile?: (path: string, contents: string) => Promise<void>;
  removeTemporaryDirectory?: (path: string) => Promise<void>;
  childStopGraceMs?: number;
}>;

function fourCc(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(...bytes.subarray(offset, offset + 4));
}

export function validateSpeechWav(bytes: Uint8Array): ValidatedWav {
  if (bytes.byteLength > SPEECH_MAX_OUTPUT_BYTES) {
    throw new SpeechOutputError("Speech output exceeds the 1 MiB limit.");
  }
  if (
    bytes.byteLength < 44 ||
    fourCc(bytes, 0) !== "RIFF" ||
    fourCc(bytes, 8) !== "WAVE"
  ) {
    throw new SpeechOutputError("Speech output is not a RIFF/WAVE file.");
  }

  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (view.getUint32(4, true) + 8 !== bytes.byteLength) {
    throw new SpeechOutputError(
      "Speech WAV length does not match its RIFF header.",
    );
  }

  let foundFormat = false;
  let dataLength: number | undefined;
  let offset = 12;
  for (; offset + 8 <= bytes.byteLength;) {
    const chunkType = fourCc(bytes, offset);
    const chunkLength = view.getUint32(offset + 4, true);
    const payloadOffset = offset + 8;
    const paddedEnd = payloadOffset + chunkLength + (chunkLength % 2);
    if (paddedEnd > bytes.byteLength) {
      throw new SpeechOutputError(
        `Speech WAV chunk ${chunkType} extends beyond the file.`,
      );
    }
    if (chunkType === "fmt ") {
      if (foundFormat || chunkLength !== 16) {
        throw new SpeechOutputError("Speech WAV has an invalid fmt chunk.");
      }
      foundFormat = true;
      const audioFormat = view.getUint16(payloadOffset, true);
      const channels = view.getUint16(payloadOffset + 2, true);
      const sampleRate = view.getUint32(payloadOffset + 4, true);
      const byteRate = view.getUint32(payloadOffset + 8, true);
      const blockAlign = view.getUint16(payloadOffset + 12, true);
      const bitsPerSample = view.getUint16(payloadOffset + 14, true);
      if (
        audioFormat !== 1 ||
        channels !== 1 ||
        sampleRate !== SPEECH_SAMPLE_RATE_HZ ||
        byteRate !== SPEECH_SAMPLE_RATE_HZ * 2 ||
        blockAlign !== 2 ||
        bitsPerSample !== 16
      ) {
        throw new SpeechOutputError(
          "Speech output must be PCM16 mono WAV at 24 kHz.",
        );
      }
    } else if (chunkType === "data") {
      if (dataLength !== undefined || chunkLength === 0 || chunkLength % 2) {
        throw new SpeechOutputError("Speech WAV has an invalid data chunk.");
      }
      dataLength = chunkLength;
    }
    offset = paddedEnd;
  }

  if (offset !== bytes.byteLength) {
    throw new SpeechOutputError("Speech WAV has an incomplete trailing chunk.");
  }

  if (!foundFormat || dataLength === undefined) {
    throw new SpeechOutputError("Speech WAV is missing fmt or data audio.");
  }
  const sampleCount = dataLength / 2;
  if (sampleCount > SPEECH_MAX_FRAMES * SPEECH_SAMPLES_PER_FRAME) {
    throw new SpeechOutputError("Speech WAV exceeds the native frame limit.");
  }
  return Object.freeze({ bytes, sampleCount });
}

export function speechNativeArguments(
  preparation: SpeechPreparation,
  promptPath: string,
  outputPath: string,
): string[] {
  return [
    preparation.binaryPath,
    "--offline",
    "-m",
    preparation.modelPath,
    "-mm",
    preparation.projectorPath,
    "-f",
    promptPath,
    "-n",
    String(SPEECH_MAX_FRAMES),
    "-t",
    "4",
    "-tb",
    "4",
    "--tts-lang",
    "en",
    "-o",
    outputPath,
  ];
}

async function readBoundedDiagnostics(
  stream: ReadableStream<Uint8Array> | number | undefined,
): Promise<string> {
  if (!stream || typeof stream === "number") return "";
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_NATIVE_DIAGNOSTIC_BYTES) {
        throw new SpeechOutputError(
          "Native speech diagnostics exceeded 64 KiB.",
        );
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(bytes);
}

function generatedFrameCount(stderr: string): number {
  const matches = [
    ...stderr.matchAll(/generated (\d+) frames, \d+ bytes of WAV audio/g),
  ];
  const value = matches.at(-1)?.[1];
  if (!value) {
    throw new SpeechOutputError(
      "Native speech output did not report a completed frame count.",
    );
  }
  const frames = Number(value);
  if (!Number.isSafeInteger(frames) || frames < 1) {
    throw new SpeechOutputError(
      "Native speech output reported invalid frames.",
    );
  }
  if (frames >= SPEECH_MAX_FRAMES) {
    throw new SpeechOutputError(
      "Speech generation reached the native frame cap and may be truncated.",
    );
  }
  return frames;
}

function operation(): ActiveGeneration {
  let finish: () => void;
  let reportStopFailure: (error: unknown) => void;
  let stopFailureReported = false;
  const done = new Promise<void>((resolve) => {
    finish = resolve;
  });
  const stopFailed = new Promise<unknown>((resolve) => {
    reportStopFailure = resolve;
  });
  return {
    cancellation: new AbortController(),
    done,
    finish: () => finish(),
    stopFailed,
    reportStopFailure: (error) => {
      if (stopFailureReported) return;
      stopFailureReported = true;
      reportStopFailure(error);
    },
  };
}

/** Owns prepared speech assets and one bounded native child per generation. */
export class SpeechSupervisor {
  readonly kind = "speech" as const;
  private lifecycleState: ModalityLifecycleState = "idle";
  private preparation: SpeechPreparation | undefined;
  private preparationPromise: Promise<void> | undefined;
  private preparationGeneration = 0;
  private shuttingDown = false;
  private generation = 0;
  private readonly active = new Set<ActiveGeneration>();

  constructor(private readonly options: SpeechSupervisorOptions) {}

  runtimeId(): string {
    return this.options.runtimeId;
  }

  state(): ModalityLifecycleState {
    return this.lifecycleState;
  }

  async ensureRunning(): Promise<void> {
    if (this.shuttingDown) throw new Error("llama-tts is shutting down.");
    if (this.lifecycleState === "failed" && this.active.size > 0) {
      throw new Error("llama-tts has an unresolved child process.");
    }
    if (this.lifecycleState === "stopping") {
      throw new SpeechGenerationAbortedError();
    }
    if (this.preparation) {
      this.lifecycleState = "running";
      return;
    }
    if (!this.preparationPromise) {
      this.lifecycleState = "starting";
      const generation = ++this.preparationGeneration;
      this.preparationPromise = this.prepare(generation);
    }
    await this.preparationPromise;
  }

  private async prepare(generation: number): Promise<void> {
    this.options.logger.event({
      severity: "info",
      eventName: "backend.starting",
      category: "runtime",
      component: "llama-tts",
      runtime: "tts",
      message: "Preparing speech runtime assets.",
      attributes: { model_id: this.options.modelId },
    });
    try {
      const preparation = await this.options.prepare();
      if (
        this.shuttingDown ||
        this.lifecycleState === "stopping" ||
        generation !== this.preparationGeneration
      ) {
        return;
      }
      this.preparation = preparation;
      this.lifecycleState = "running";
      this.options.logger.event({
        severity: "info",
        eventName: "backend.ready",
        category: "runtime",
        component: "llama-tts",
        runtime: "tts",
        message: "Speech runtime assets are ready.",
        attributes: { model_id: this.options.modelId },
      });
    } catch (error) {
      if (generation === this.preparationGeneration) {
        this.lifecycleState = "failed";
      }
      throw error;
    } finally {
      if (generation === this.preparationGeneration) {
        this.preparationPromise = undefined;
      }
    }
  }

  async generateSpeech(input: SpeechGenerationInput): Promise<Uint8Array> {
    const characterCount = Array.from(input.text).length;
    if (characterCount < 1 || characterCount > SPEECH_MAX_INPUT_CHARACTERS) {
      throw new SpeechOutputError(
        `Speech input must contain between 1 and ${SPEECH_MAX_INPUT_CHARACTERS} characters.`,
      );
    }
    await this.ensureRunning();
    const preparation = this.preparation;
    if (!preparation || this.lifecycleState !== "running") {
      throw new SpeechGenerationAbortedError();
    }

    const active = operation();
    this.active.add(active);
    let reservation: RuntimeMemoryReservation | undefined;
    let temporaryDirectory: string | undefined;
    let childExited = false;
    let output: Uint8Array | undefined;
    let failure: unknown;
    let cleanupDeferredUntilExit = false;
    let resourcesFinalized = false;
    let resourceCleanupFinished = false;
    let guardianExitConfirmed = false;
    let ownershipCompleted = false;
    const completeOwnership = (): void => {
      if (
        ownershipCompleted ||
        !resourceCleanupFinished ||
        !guardianExitConfirmed
      ) {
        return;
      }
      ownershipCompleted = true;
      this.active.delete(active);
      active.finish();
    };
    const finalize = async (): Promise<unknown[]> => {
      if (resourcesFinalized) return [];
      resourcesFinalized = true;
      const errors: unknown[] = [];
      let guardianStopFailed = false;
      if (active.guardian) {
        try {
          await stopNativeProcess(
            active.guardian,
            this.options.childStopGraceMs ?? CHILD_STOP_GRACE_MS,
          );
        } catch (error) {
          errors.push(error);
          guardianStopFailed = true;
          active.reportStopFailure(error);
          this.lifecycleState = "failed";
          void active.guardian.exited.then(
            () => {
              guardianExitConfirmed = true;
              completeOwnership();
            },
            () => {
              // Exit remains unconfirmed; guardian ownership stays attached.
            },
          );
        }
      } else {
        guardianExitConfirmed = true;
      }
      if (reservation) {
        try {
          reservation.release();
        } catch (error) {
          errors.push(error);
        }
      }
      if (temporaryDirectory) {
        try {
          await (
            this.options.removeTemporaryDirectory ?? removeTemporaryDirectory
          )(temporaryDirectory);
        } catch (error) {
          errors.push(error);
        }
      }
      if (!guardianStopFailed) guardianExitConfirmed = true;
      resourceCleanupFinished = true;
      completeOwnership();
      return errors;
    };
    try {
      const signals = [active.cancellation.signal, input.signal].filter(
        (signal): signal is AbortSignal => signal !== undefined,
      );
      const aborted = () => signals.some((signal) => signal.aborted);
      if (aborted()) throw new SpeechGenerationAbortedError();
      reservation = await this.options.memorySafety.reserve({
        runtimeId: `${this.options.runtimeId}:generation:${++this.generation}`,
        demand: SPEECH_MEMORY_DEMAND,
      });
      if (aborted()) throw new SpeechGenerationAbortedError();

      const temporaryRoot = join(this.options.root, "tmp");
      await mkdir(temporaryRoot, { recursive: true, mode: 0o700 });
      await chmod(temporaryRoot, 0o700);
      temporaryDirectory = await mkdtemp(join(temporaryRoot, "speech-"));
      await chmod(temporaryDirectory, 0o700);
      const promptPath = join(temporaryDirectory, "prompt.txt");
      const outputPath = join(temporaryDirectory, "output.wav");
      const writePrivate = this.options.writePrivateFile ?? writePrivateFile;
      await writePrivate(promptPath, input.text);
      await writePrivate(outputPath, "");

      const command = speechNativeArguments(
        preparation,
        promptPath,
        outputPath,
      );
      if (aborted()) throw new SpeechGenerationAbortedError();
      const process = (this.options.spawn ?? defaultSpawn)(
        command,
        temporaryDirectory,
      );
      active.process = process;
      const exitOutcome = waitForExit(process, signals);
      active.guardian = (this.options.spawnGuardian ?? defaultSpawnGuardian)(
        guardianProcessCommand(globalThis.process.pid, process.pid),
      );

      const diagnostics = readBoundedDiagnostics(process.stderr).then(
        (value) => ({ kind: "complete" as const, value }),
        (error: unknown) => ({ kind: "failed" as const, error }),
      );
      const first = await Promise.race([
        exitOutcome.then((outcome) => ({ kind: "exit" as const, outcome })),
        diagnostics.then((outcome) =>
          outcome.kind === "failed"
            ? { kind: "diagnostic-failure" as const, error: outcome.error }
            : { kind: "diagnostics-complete" as const },
        ),
      ]);
      if (first.kind === "diagnostic-failure") {
        await stopNativeProcess(
          process,
          this.options.childStopGraceMs ?? CHILD_STOP_GRACE_MS,
        );
      }
      const outcome = first.kind === "exit" ? first.outcome : await exitOutcome;
      if (outcome !== "exited") {
        await stopNativeProcess(
          process,
          this.options.childStopGraceMs ?? CHILD_STOP_GRACE_MS,
        );
      }
      const [exitCode, diagnosticOutcome] = await Promise.all([
        process.exited,
        diagnostics,
      ]);
      childExited = true;
      if (outcome === "aborted") throw new SpeechGenerationAbortedError();
      if (outcome === "timeout") throw new SpeechGenerationTimeoutError();
      if (aborted()) throw new SpeechGenerationAbortedError();
      if (diagnosticOutcome.kind === "failed") throw diagnosticOutcome.error;
      if (exitCode !== 0) {
        throw new SpeechOutputError("Native speech generation failed.");
      }

      const frames = generatedFrameCount(diagnosticOutcome.value);
      const file = Bun.file(outputPath);
      const stat = await file.stat();
      if (stat.size > SPEECH_MAX_OUTPUT_BYTES) {
        throw new SpeechOutputError("Speech output exceeds the 1 MiB limit.");
      }
      const wav = validateSpeechWav(await file.bytes());
      if (wav.sampleCount !== frames * SPEECH_SAMPLES_PER_FRAME) {
        throw new SpeechOutputError(
          "Speech WAV sample count does not match the native frame count.",
        );
      }
      output = wav.bytes;
    } catch (error) {
      failure = error;
      if (active.process && !childExited) {
        try {
          await stopNativeProcess(
            active.process,
            this.options.childStopGraceMs ?? CHILD_STOP_GRACE_MS,
          );
          childExited = true;
        } catch (stopError) {
          active.reportStopFailure(stopError);
          this.lifecycleState = "failed";
          cleanupDeferredUntilExit = true;
          void active.process.exited.then(
            async () => {
              const cleanupErrors = await finalize();
              this.reportCleanupErrors(cleanupErrors);
            },
            () => {
              // Exit remains unconfirmed; ownership and resources stay attached.
            },
          );
          failure = new AggregateError(
            [error, stopError],
            "Speech generation failed and its child could not be stopped.",
            { cause: error },
          );
        }
      }
    }

    const cleanupErrors = cleanupDeferredUntilExit ? [] : await finalize();

    if (cleanupErrors.length > 0) {
      const cleanupFailure = new AggregateError(
        cleanupErrors,
        "Speech generation cleanup failed.",
      );
      if (failure === undefined) throw cleanupFailure;
      this.reportCleanupErrors(cleanupErrors);
    }
    if (failure !== undefined) throw failure;
    if (!output) throw new SpeechOutputError("Speech output was not produced.");
    return output;
  }

  private reportCleanupErrors(errors: readonly unknown[]): void {
    if (errors.length === 0) return;
    try {
      this.options.logger.event({
        severity: "error",
        eventName: "runtime.cleanup-failed",
        category: "runtime",
        component: "llama-tts",
        runtime: "tts",
        message: "Speech generation cleanup failed.",
        error: {
          type: "AggregateError",
          message: "Speech generation cleanup failed.",
        },
      });
    } catch {
      // Cleanup reporting cannot replace the generation result.
    }
  }

  async kill(): Promise<void> {
    if (!this.shuttingDown) this.lifecycleState = "stopping";
    this.preparationGeneration += 1;
    const preparation = this.preparationPromise;
    const active = [...this.active];
    for (const generation of active) generation.cancellation.abort();
    try {
      await Promise.all([
        preparation?.catch(() => {}),
        ...active.map(async ({ done, stopFailed }) => {
          const outcome = await Promise.race([
            done.then(() => ({ kind: "done" as const })),
            stopFailed.then((error) => ({
              kind: "stop-failed" as const,
              error,
            })),
          ]);
          if (outcome.kind === "stop-failed") throw outcome.error;
        }),
      ]);
    } catch (error) {
      this.lifecycleState = "failed";
      throw error;
    }
    this.preparationPromise = undefined;
    this.preparation = undefined;
    if (!this.shuttingDown) this.lifecycleState = "idle";
  }

  async shutdown(): Promise<void> {
    this.shuttingDown = true;
    this.lifecycleState = "stopping";
    await this.kill();
  }
}

function defaultSpawn(command: string[], cwd: string): Bun.Subprocess {
  return Bun.spawn(command, {
    cwd,
    stdin: "ignore",
    stdout: "ignore",
    stderr: "pipe",
  });
}

function defaultSpawnGuardian(command: string[]): Bun.Subprocess {
  return Bun.spawn(command, {
    stdin: "ignore",
    stdout: "ignore",
    stderr: "ignore",
    detached: true,
  });
}

async function removeTemporaryDirectory(path: string): Promise<void> {
  await rm(path, { recursive: true, force: true });
}

async function writePrivateFile(path: string, contents: string): Promise<void> {
  await writeFile(path, contents, { mode: 0o600, flag: "wx" });
}

function waitForExit(
  process: Bun.Subprocess,
  signals: readonly AbortSignal[],
): Promise<"exited" | "aborted" | "timeout"> {
  return new Promise((resolve) => {
    let settled = false;
    let timeout: ReturnType<typeof setTimeout>;
    const finish = (outcome: "exited" | "aborted" | "timeout") => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      for (const signal of signals) signal.removeEventListener("abort", abort);
      resolve(outcome);
    };
    const abort = () => finish("aborted");
    timeout = setTimeout(() => finish("timeout"), SPEECH_TIMEOUT_MS);
    for (const signal of signals) {
      signal.addEventListener("abort", abort, { once: true });
    }
    if (signals.some((signal) => signal.aborted)) abort();
    void process.exited.then(
      () => finish("exited"),
      () => finish("exited"),
    );
  });
}
