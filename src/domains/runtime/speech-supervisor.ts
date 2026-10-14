import {
  chmod,
  mkdir,
  mkdtemp,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { join } from "node:path";
import type {
  KokoroSpeechVoice,
  ReferenceSpeechVoice,
  SpeechVoice,
} from "../../catalog";
import type { ILogger } from "../observability/logging";
import { guardianProcessCommand } from "./backend-guardian";
import type { ModalityLifecycleState } from "./health";
import {
  RuntimeMemoryAdmissionError,
  type MemorySafetyController,
  type RuntimeMemoryReservation,
} from "./memory-controller";
import { gibibyte, type RuntimeMemoryDemand } from "./memory-safety";
import { stopNativeProcess } from "./native-process";

export { NativeProcessStopError as SpeechChildStopError } from "./native-process";

export const SPEECH_MAX_INPUT_CHARACTERS = 256;
export const SPEECH_MAX_FRAMES = 256;
/**
 * Talker context passed to llama-tts as `-c`. Without it the talker allocates
 * its full 32K trained context (3.5 GiB f16 KV). The text prompt fills the
 * context first, then each generated 12 Hz codec frame takes one cell. The
 * tokenizer is byte-level BPE, so the prompt is at most 4 UTF-8 bytes per
 * code point plus the template. With a reference voice, 256 four-byte
 * private-use code points measured ~1050 cells and 256 characters of
 * English ~60. Worst case ~1050 + SPEECH_MAX_FRAMES = ~1310
 * cells. The prompt must also fit one batch, and llama.cpp caps n_batch at
 * n_ctx, so 1024 would reject the worst-case prompt.
 */
export const SPEECH_CONTEXT_TOKENS = 2048;

export const SPEECH_SAMPLES_PER_FRAME = 1_920;
export const SPEECH_SAMPLE_RATE_HZ = 24_000;
export const SPEECH_MAX_OUTPUT_BYTES = 1024 * 1024;
export const SPEECH_TIMEOUT_MS = 120_000;

const CHILD_STOP_GRACE_MS = 500;
const MAX_NATIVE_DIAGNOSTIC_BYTES = 64 * 1024;
/** Fallback when the caller has no catalog-derived TTS estimate. */
export const SPEECH_MEMORY_DEMAND: RuntimeMemoryDemand = Object.freeze({
  unifiedBytes: 8 * gibibyte,
  hostBytes: 8 * gibibyte,
  acceleratorBytes: 8 * gibibyte,
  confidence: "estimated",
});

export type SpeechGenerationInput = Readonly<{
  text: string;
  voice?: SpeechVoice;
  signal?: AbortSignal;
}>;

export type TtsServerPreparation = Readonly<{
  kind: "tts-server";
  binaryPath: string;
  modelPath: string;
  projectorPath: string;
  speakerFiles: Readonly<Record<ReferenceSpeechVoice, string>>;
}>;

/**
 * A persistent tts-server process owned by a supervised, memory-admitted
 * service. It keeps the talker and code2wav weights resident between
 * requests and streams raw PCM16 audio per generation.
 */
export type WarmSpeechServer = Readonly<{
  generateUrl: string;
  ensureRunning(): Promise<void>;
  state(): ModalityLifecycleState;
  processingSlots(): Promise<number | undefined>;
  kill(): Promise<void>;
  shutdown(): Promise<void>;
}>;

export type SpeechAudioStream = Readonly<{
  sampleRate: number;
  /** PCM16 mono little-endian chunks; every chunk has an even byte length. */
  pcm: ReadableStream<Uint8Array>;
}>;

export type SpeechPreparation =
  | TtsServerPreparation
  | Readonly<{
      kind: "llama-tts";
      binaryPath: string;
      modelPath: string;
      projectorPath: string;
      speakerFiles: Readonly<Record<ReferenceSpeechVoice, string>>;
    }>
  | Readonly<{
      kind: "kokoro-onnx";
      command: readonly string[];
      modelFiles: Readonly<{
        model: string;
        config: string;
        tokenizer: string;
        tokenizerConfig: string;
      }>;
      inventorySha256: string;
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
  dataOffset: number;
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
  /** Per-generation reservation; defaults to a conservative 8 GiB allowance. */
  memoryDemand?: RuntimeMemoryDemand;
  spawn?: (
    command: string[],
    cwd: string,
    env?: Readonly<Record<string, string>>,
  ) => Bun.Subprocess;
  spawnGuardian?: (command: string[]) => Bun.Subprocess | undefined;
  writePrivateFile?: (path: string, contents: string) => Promise<void>;
  removeTemporaryDirectory?: (path: string) => Promise<void>;
  childStopGraceMs?: number;
  /** Builds the persistent server for a tts-server preparation. */
  createWarmServer?: (preparation: TtsServerPreparation) => WarmSpeechServer;
  /** Delay between retries while the warm server reports it is busy. */
  busyRetryMs?: number;
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
  let dataOffset = 0;
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
      dataOffset = payloadOffset;
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
  return Object.freeze({ bytes, sampleCount, dataOffset });
}

const SPEECH_MAX_PCM_BYTES = SPEECH_MAX_FRAMES * SPEECH_SAMPLES_PER_FRAME * 2;
const DEFAULT_BUSY_RETRY_MS = 50;
const MAX_RUNTIME_ERROR_BYTES = 4 * 1024;

/** Wraps PCM16 mono 24 kHz samples in a canonical 44-byte RIFF/WAVE header. */
export function speechWavFromPcm(pcm: Uint8Array): Uint8Array {
  const bytes = new Uint8Array(44 + pcm.byteLength);
  const view = new DataView(bytes.buffer);
  const ascii = (offset: number, value: string) => {
    for (let index = 0; index < value.length; index += 1) {
      bytes[offset + index] = value.charCodeAt(index);
    }
  };
  ascii(0, "RIFF");
  view.setUint32(4, 36 + pcm.byteLength, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, SPEECH_SAMPLE_RATE_HZ, true);
  view.setUint32(28, SPEECH_SAMPLE_RATE_HZ * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, "data");
  view.setUint32(40, pcm.byteLength, true);
  bytes.set(pcm, 44);
  return bytes;
}

/** Returns the PCM16 sample payload of a validated speech WAV. */
export function speechPcmFromWav(bytes: Uint8Array): Uint8Array {
  const wav = validateSpeechWav(bytes);
  return wav.bytes.subarray(
    wav.dataOffset,
    wav.dataOffset + wav.sampleCount * 2,
  );
}

/** Presents a completed speech WAV as a single-chunk PCM stream. */
export function bufferedSpeechStream(bytes: Uint8Array): SpeechAudioStream {
  const pcm = speechPcmFromWav(bytes);
  return Object.freeze({
    sampleRate: SPEECH_SAMPLE_RATE_HZ,
    pcm: new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(pcm);
        controller.close();
      },
    }),
  });
}

/** Rejects warm-server PCM that is not a whole number of completed frames. */
export function assertCompleteSpeechPcm(byteLength: number): number {
  if (byteLength === 0 || byteLength % (SPEECH_SAMPLES_PER_FRAME * 2) !== 0) {
    throw new SpeechOutputError(
      "Speech runtime returned audio that is not a whole number of frames.",
    );
  }
  const frames = byteLength / (SPEECH_SAMPLES_PER_FRAME * 2);
  if (frames >= SPEECH_MAX_FRAMES) {
    throw new SpeechOutputError(
      "Speech generation reached the native frame cap and may be truncated.",
    );
  }
  return frames;
}

export function speechNativeArguments(
  preparation: Extract<SpeechPreparation, { kind: "llama-tts" }>,
  promptPath: string,
  outputPath: string,
  voice: ReferenceSpeechVoice | "default" = "default",
): string[] {
  const speakerFile =
    voice === "default" ? undefined : preparation.speakerFiles[voice];
  return [
    preparation.binaryPath,
    "--offline",
    "-m",
    preparation.modelPath,
    "-mm",
    preparation.projectorPath,
    "-f",
    promptPath,
    "-c",
    String(SPEECH_CONTEXT_TOKENS),
    "-n",
    String(SPEECH_MAX_FRAMES),
    "-t",
    "4",
    "-tb",
    "4",
    "--tts-lang",
    "en",
    ...(speakerFile ? ["--tts-speaker-file", speakerFile] : []),
    "-o",
    outputPath,
  ];
}

export function kokoroNativeArguments(
  preparation: Extract<SpeechPreparation, { kind: "kokoro-onnx" }>,
  modelDirectory: string,
  promptPath: string,
  outputPath: string,
  voice: KokoroSpeechVoice,
): string[] {
  return [
    ...preparation.command,
    "generate",
    "--model",
    modelDirectory,
    "--prompt",
    promptPath,
    "--output",
    outputPath,
    "--voice",
    voice,
    "--inventory-sha256",
    preparation.inventorySha256,
  ];
}

async function createKokoroModelLayout(
  privateDirectory: string,
  files: Extract<SpeechPreparation, { kind: "kokoro-onnx" }>["modelFiles"],
): Promise<string> {
  const modelDirectory = join(privateDirectory, "model");
  const onnxDirectory = join(modelDirectory, "onnx");
  await mkdir(onnxDirectory, { recursive: true, mode: 0o700 });
  await Promise.all([
    symlink(files.config, join(modelDirectory, "config.json")),
    symlink(files.tokenizer, join(modelDirectory, "tokenizer.json")),
    symlink(
      files.tokenizerConfig,
      join(modelDirectory, "tokenizer_config.json"),
    ),
    symlink(files.model, join(onnxDirectory, "model_quantized.onnx")),
  ]);
  return modelDirectory;
}

/**
 * Kokoro runs with no inherited hooks, module paths, loader overrides or host
 * tools, matching the packaged runtime's invocation contract.
 */
export async function kokoroEnvironment(
  privateDirectory: string,
): Promise<Readonly<Record<string, string>>> {
  const emptyPath = join(privateDirectory, "empty-path");
  await mkdir(emptyPath, { mode: 0o700 });
  return Object.freeze({
    PATH: emptyPath,
    HOME: privateDirectory,
    TMPDIR: privateDirectory,
    LANG: "C",
    LC_ALL: "C",
  });
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

function generatedKokoroSampleCount(stderr: string): number {
  const matches = [
    ...stderr.matchAll(/Kokoro speech completed: (\d+) samples\./g),
  ];
  const value = matches.at(-1)?.[1];
  if (!value) {
    throw new SpeechOutputError("Kokoro did not report completed samples.");
  }
  const samples = Number(value);
  if (!Number.isSafeInteger(samples) || samples < 1) {
    throw new SpeechOutputError("Kokoro reported invalid samples.");
  }
  return samples;
}

function llamaVoice(
  voice: SpeechVoice | undefined,
): ReferenceSpeechVoice | "default" {
  if (voice === undefined || voice === "default") return "default";
  if (voice === "harbor" || voice === "willow") return voice;
  throw new SpeechOutputError("Voice is not supported by llama-tts.");
}

function abortableDelay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/** Reads at most `limit` bytes of an error body as a short printable message. */
async function boundedText(response: Response, limit: number): Promise<string> {
  const reader = response.body?.getReader();
  if (!reader) return "";
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (size < limit) {
      const { done, value } = await reader.read();
      if (done) break;
      const take = Math.min(value.byteLength, limit - size);
      chunks.push(value.subarray(0, take));
      size += take;
    }
  } catch {
    // A partial error body is still useful.
  } finally {
    await reader.cancel().catch(() => {});
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  const text = new TextDecoder().decode(bytes);
  let message = text;
  try {
    const parsed = JSON.parse(text) as { error?: unknown };
    if (typeof parsed.error === "string") message = parsed.error;
  } catch {
    // Not JSON.
  }
  return message
    .replace(/[^\x20-\x7e]+/g, " ")
    .trim()
    .slice(0, 512);
}

function kokoroVoice(voice: SpeechVoice | undefined): KokoroSpeechVoice {
  if (voice === "af_heart" || voice === "af_bella") return voice;
  throw new SpeechOutputError("Voice is not supported by kokoro-onnx.");
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
  private warm: WarmSpeechServer | undefined;
  private readonly active = new Set<ActiveGeneration>();

  constructor(private readonly options: SpeechSupervisorOptions) {}

  runtimeId(): string {
    return this.options.runtimeId;
  }

  state(): ModalityLifecycleState {
    // A warm server can crash and restart on its own after preparation.
    if (this.lifecycleState === "running" && this.warm) {
      return this.warm.state();
    }
    return this.lifecycleState;
  }

  /** Busy generation slots on the warm server; undefined for one-shot runtimes. */
  async processingSlots(): Promise<number | undefined> {
    const warm = this.warm;
    if (!warm || this.lifecycleState !== "running") return undefined;
    return await warm.processingSlots();
  }

  /**
   * True while a healthy warm server owns generation. The server stops a
   * generation when its client disconnects, so a cancelled request does not
   * need the runtime (and its resident weights) torn down.
   */
  retainsRuntimeOnCancel(): boolean {
    return (
      this.warm !== undefined &&
      this.lifecycleState === "running" &&
      this.warm.state() === "running"
    );
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
      if (this.warm) await this.warm.ensureRunning();
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
    const stale = () =>
      this.shuttingDown ||
      this.lifecycleState === "stopping" ||
      generation !== this.preparationGeneration;
    try {
      const preparation = await this.options.prepare();
      if (stale()) return;
      if (preparation.kind === "tts-server") {
        if (!this.options.createWarmServer) {
          throw new Error(
            "tts-server preparation needs a warm server factory.",
          );
        }
        const warm = this.options.createWarmServer(preparation);
        // kill() stops the published server, which also cancels its startup.
        this.warm = warm;
        try {
          await warm.ensureRunning();
        } catch (error) {
          if (this.warm === warm) this.warm = undefined;
          await warm.kill().catch(() => {});
          throw error;
        }
        if (stale()) {
          if (this.warm === warm) this.warm = undefined;
          await warm.kill();
          return;
        }
      }
      this.preparation = preparation;
      this.lifecycleState = "running";
      this.options.logger.event({
        severity: "info",
        eventName: "backend.ready",
        category: "runtime",
        component: "llama-tts",
        runtime: "tts",
        message:
          preparation.kind === "tts-server"
            ? "Warm speech server is ready."
            : "Speech runtime assets are ready.",
        attributes: {
          model_id: this.options.modelId,
          speech_runtime: preparation.kind,
        },
      });
    } catch (error) {
      if (generation === this.preparationGeneration) {
        // Admission refusal leaves nothing running; a later request can retry.
        this.lifecycleState =
          error instanceof RuntimeMemoryAdmissionError ? "idle" : "failed";
      }
      throw error;
    } finally {
      if (generation === this.preparationGeneration) {
        this.preparationPromise = undefined;
      }
    }
  }

  private assertSpeechInput(input: SpeechGenerationInput): void {
    const characterCount = Array.from(input.text).length;
    if (characterCount < 1 || characterCount > SPEECH_MAX_INPUT_CHARACTERS) {
      throw new SpeechOutputError(
        `Speech input must contain between 1 and ${SPEECH_MAX_INPUT_CHARACTERS} characters.`,
      );
    }
  }

  /**
   * Streams PCM16 audio as it is generated. One-shot runtimes have no partial
   * audio, so they emit the completed waveform as a single chunk.
   */
  async generateSpeechStream(
    input: SpeechGenerationInput,
  ): Promise<SpeechAudioStream> {
    this.assertSpeechInput(input);
    await this.ensureRunning();
    const preparation = this.preparation;
    const warm = this.warm;
    if (!preparation || this.lifecycleState !== "running") {
      throw new SpeechGenerationAbortedError();
    }
    if (preparation.kind === "tts-server" && warm) {
      return await this.streamWarmSpeech(warm, input);
    }
    return bufferedSpeechStream(await this.generateSpeech(input));
  }

  private async generateWarmSpeech(
    warm: WarmSpeechServer,
    input: SpeechGenerationInput,
  ): Promise<Uint8Array> {
    const { pcm } = await this.streamWarmSpeech(warm, input);
    const reader = pcm.getReader();
    const chunks: Uint8Array[] = [];
    let size = 0;
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
        size += value.byteLength;
      }
    } finally {
      reader.releaseLock();
    }
    const samples = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) {
      samples.set(chunk, offset);
      offset += chunk.byteLength;
    }
    return validateSpeechWav(speechWavFromPcm(samples)).bytes;
  }

  private async streamWarmSpeech(
    warm: WarmSpeechServer,
    input: SpeechGenerationInput,
  ): Promise<SpeechAudioStream> {
    const voice = llamaVoice(input.voice);
    const active = operation();
    this.active.add(active);
    const request = new AbortController();
    let timedOut = false;
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    let finished = false;
    const signals = [active.cancellation.signal, input.signal].filter(
      (signal): signal is AbortSignal => signal !== undefined,
    );
    const abort = () => request.abort();
    const timeout = setTimeout(() => {
      timedOut = true;
      request.abort();
    }, SPEECH_TIMEOUT_MS);
    const finish = () => {
      if (finished) return;
      finished = true;
      clearTimeout(timeout);
      for (const signal of signals) signal.removeEventListener("abort", abort);
      this.active.delete(active);
      active.finish();
    };
    // Aborting closes the upstream connection, which makes tts-server stop at
    // its next chunk, so ownership ends even if nobody reads the stream.
    request.signal.addEventListener(
      "abort",
      () => {
        void reader?.cancel().catch(() => {});
        finish();
      },
      { once: true },
    );
    for (const signal of signals) {
      signal.addEventListener("abort", abort, { once: true });
    }
    if (signals.some((signal) => signal.aborted)) abort();
    const failure = (error: unknown): unknown => {
      if (request.signal.aborted) {
        return timedOut
          ? new SpeechGenerationTimeoutError()
          : new SpeechGenerationAbortedError();
      }
      if (error instanceof SpeechOutputError) return error;
      return new SpeechOutputError(
        "Speech runtime closed the audio stream before completion.",
      );
    };

    let response: Response;
    try {
      response = await this.requestWarmSpeech(
        warm,
        input.text,
        voice,
        request.signal,
      );
      if (!response.body) {
        throw new SpeechOutputError("Speech runtime returned no audio.");
      }
      reader = response.body.getReader();
    } catch (error) {
      finish();
      throw failure(error);
    }
    const body = reader;
    let received = 0;
    let carry: number | undefined;
    const pcm = new ReadableStream<Uint8Array>({
      pull: async (controller) => {
        try {
          while (true) {
            if (request.signal.aborted) throw request.signal.reason;
            const { done, value } = await body.read();
            if (done) {
              if (carry !== undefined) {
                throw new SpeechOutputError(
                  "Speech runtime returned a partial PCM sample.",
                );
              }
              assertCompleteSpeechPcm(received);
              finish();
              controller.close();
              return;
            }
            received += value.byteLength;
            if (received > SPEECH_MAX_PCM_BYTES) {
              throw new SpeechOutputError(
                "Speech output exceeds the native frame limit.",
              );
            }
            let chunk = value;
            if (carry !== undefined) {
              chunk = new Uint8Array(value.byteLength + 1);
              chunk[0] = carry;
              chunk.set(value, 1);
              carry = undefined;
            }
            if (chunk.byteLength % 2 === 1) {
              carry = chunk[chunk.byteLength - 1];
              chunk = chunk.subarray(0, chunk.byteLength - 1);
            }
            if (chunk.byteLength > 0) {
              controller.enqueue(chunk);
              return;
            }
          }
        } catch (error) {
          const mapped = failure(error);
          void body.cancel().catch(() => {});
          finish();
          controller.error(mapped);
        }
      },
      cancel: async () => {
        request.abort();
        await body.cancel().catch(() => {});
        finish();
      },
    });
    return Object.freeze({ sampleRate: SPEECH_SAMPLE_RATE_HZ, pcm });
  }

  private async requestWarmSpeech(
    warm: WarmSpeechServer,
    text: string,
    voice: ReferenceSpeechVoice | "default",
    signal: AbortSignal,
  ): Promise<Response> {
    const body = JSON.stringify({ input: text, voice });
    while (true) {
      signal.throwIfAborted();
      let response: Response;
      try {
        response = await fetch(warm.generateUrl, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body,
          signal,
        });
      } catch (error) {
        if (signal.aborted) throw error;
        throw new SpeechOutputError("Speech runtime request failed.");
      }
      if (response.status === 503) {
        // A cancelled generation finishes its current window before the
        // server frees the slot.
        await response.body?.cancel().catch(() => {});
        await abortableDelay(
          this.options.busyRetryMs ?? DEFAULT_BUSY_RETRY_MS,
          signal,
        );
        continue;
      }
      if (!response.ok) {
        const detail = await boundedText(response, MAX_RUNTIME_ERROR_BYTES);
        throw new SpeechOutputError(
          `Speech runtime rejected the request (HTTP ${response.status})${detail ? `: ${detail}` : "."}`,
        );
      }
      const format = response.headers.get("x-audio-format");
      const sampleRate = Number(response.headers.get("x-sample-rate"));
      if (format !== "s16le" || sampleRate !== SPEECH_SAMPLE_RATE_HZ) {
        await response.body?.cancel().catch(() => {});
        throw new SpeechOutputError(
          "Speech runtime returned an unexpected audio format.",
        );
      }
      return response;
    }
  }

  async generateSpeech(input: SpeechGenerationInput): Promise<Uint8Array> {
    this.assertSpeechInput(input);
    await this.ensureRunning();
    const preparation = this.preparation;
    if (!preparation || this.lifecycleState !== "running") {
      throw new SpeechGenerationAbortedError();
    }
    if (preparation.kind === "tts-server") {
      if (!this.warm) throw new SpeechGenerationAbortedError();
      return await this.generateWarmSpeech(this.warm, input);
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
        demand: this.options.memoryDemand ?? SPEECH_MEMORY_DEMAND,
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
      if (preparation.kind === "llama-tts") {
        await writePrivate(outputPath, "");
      }
      const command =
        preparation.kind === "llama-tts"
          ? speechNativeArguments(
              preparation,
              promptPath,
              outputPath,
              llamaVoice(input.voice),
            )
          : kokoroNativeArguments(
              preparation,
              await createKokoroModelLayout(
                temporaryDirectory,
                preparation.modelFiles,
              ),
              promptPath,
              outputPath,
              kokoroVoice(input.voice),
            );
      if (aborted()) throw new SpeechGenerationAbortedError();
      const process = (this.options.spawn ?? defaultSpawn)(
        command,
        temporaryDirectory,
        preparation.kind === "kokoro-onnx"
          ? await kokoroEnvironment(temporaryDirectory)
          : undefined,
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

      const file = Bun.file(outputPath);
      const stat = await file.stat();
      if (stat.size > SPEECH_MAX_OUTPUT_BYTES) {
        throw new SpeechOutputError("Speech output exceeds the 1 MiB limit.");
      }
      const wav = validateSpeechWav(await file.bytes());
      if (preparation.kind === "llama-tts") {
        const frames = generatedFrameCount(diagnosticOutcome.value);
        if (wav.sampleCount !== frames * SPEECH_SAMPLES_PER_FRAME) {
          throw new SpeechOutputError(
            "Speech WAV sample count does not match the native frame count.",
          );
        }
      } else if (
        wav.sampleCount !== generatedKokoroSampleCount(diagnosticOutcome.value)
      ) {
        throw new SpeechOutputError(
          "Speech WAV sample count does not match Kokoro output.",
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
    const warm = this.warm;
    try {
      await Promise.all([
        // Stopping the warm server also ends a startup that preparation awaits.
        warm ? (this.shuttingDown ? warm.shutdown() : warm.kill()) : undefined,
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
    if (this.warm === warm) this.warm = undefined;
    if (!this.shuttingDown) this.lifecycleState = "idle";
  }

  async shutdown(): Promise<void> {
    this.shuttingDown = true;
    this.lifecycleState = "stopping";
    await this.kill();
  }
}

function defaultSpawn(
  command: string[],
  cwd: string,
  env?: Readonly<Record<string, string>>,
): Bun.Subprocess {
  return Bun.spawn(command, {
    cwd,
    ...(env ? { env: { ...env } } : {}),
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
