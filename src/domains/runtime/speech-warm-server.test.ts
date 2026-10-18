import { afterEach, describe, expect, test } from "bun:test";
import { reservePort } from "../../test/ports";
import type { ModalityLifecycleState } from "./health";
import { RuntimeMemoryAdmissionError } from "./memory-controller";
import {
  SPEECH_MAX_FRAMES,
  SPEECH_SAMPLES_PER_FRAME,
  SpeechGenerationAbortedError,
  SpeechOutputError,
  SpeechSupervisor,
  assertCompleteSpeechPcm,
  speechPcmFromWav,
  speechWavFromPcm,
  validateSpeechWav,
  type WarmSpeechServer,
} from "./speech-supervisor";

const FRAME_BYTES = SPEECH_SAMPLES_PER_FRAME * 2;

function framePcm(frames: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(frames * FRAME_BYTES);
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = index % 251;
  }
  return bytes;
}

type GenerateHandler = (
  request: Request,
  attempt: number,
) => Response | Promise<Response>;

const servers: Bun.Server<undefined>[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

function pcmResponse(
  body: BodyInit,
  headers: Record<string, string> = {},
): Response {
  return new Response(body, {
    headers: {
      "content-type": "application/octet-stream",
      "x-audio-format": "s16le",
      "x-sample-rate": "24000",
      ...headers,
    },
  });
}

function startFakeTtsServer(handler: GenerateHandler) {
  const requests: unknown[] = [];
  let attempt = 0;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: reservePort(),
    async fetch(request) {
      if (new URL(request.url).pathname !== "/v1/generate") {
        return new Response("not found", { status: 404 });
      }
      requests.push(await request.clone().json());
      attempt += 1;
      return await handler(request, attempt);
    },
  });
  servers.push(server);
  return {
    url: `http://127.0.0.1:${server.port}/v1/generate`,
    requests,
  };
}

function createWarmHarness(
  generateUrl: string,
  options: { ensureRunningError?: unknown } = {},
) {
  const calls = { create: 0, ensureRunning: 0, kill: 0, shutdown: 0 };
  let state: ModalityLifecycleState = "idle";
  const warm: WarmSpeechServer = {
    generateUrl,
    async ensureRunning() {
      calls.ensureRunning += 1;
      if (options.ensureRunningError) throw options.ensureRunningError;
      state = "running";
    },
    state: () => state,
    processingSlots: async () => 0,
    async kill() {
      calls.kill += 1;
      state = "idle";
    },
    async shutdown() {
      calls.shutdown += 1;
      state = "idle";
    },
  };
  const supervisor = new SpeechSupervisor({
    runtimeId: "tts:warm:1",
    root: "/nonexistent",
    modelId: "fixture",
    logger: { event() {} } as never,
    memorySafety: {} as never,
    busyRetryMs: 5,
    prepare: async () => ({
      kind: "tts-server",
      binaryPath: "/nonexistent/tts-server",
      modelPath: "/nonexistent/model.gguf",
      projectorPath: "/nonexistent/codec.gguf",
      speakerFiles: {
        harbor: "/models/harbor.wav",
        willow: "/models/willow.wav",
      },
    }),
    createWarmServer: () => {
      calls.create += 1;
      return warm;
    },
  });
  return {
    supervisor,
    calls,
    setState: (next: ModalityLifecycleState) => {
      state = next;
    },
  };
}

async function readAll(stream: ReadableStream<Uint8Array>) {
  const chunks: Uint8Array[] = [];
  const reader = stream.getReader();
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
  }
  return chunks;
}

function concat(chunks: readonly Uint8Array[]): Uint8Array {
  const out = new Uint8Array(chunks.reduce((sum, c) => sum + c.byteLength, 0));
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

describe("speech PCM helpers", () => {
  test("wraps and unwraps PCM in a canonical speech WAV", () => {
    const pcm = framePcm(2);
    const wav = speechWavFromPcm(pcm);
    expect(wav.byteLength).toBe(44 + pcm.byteLength);
    expect(validateSpeechWav(wav).sampleCount).toBe(
      2 * SPEECH_SAMPLES_PER_FRAME,
    );
    expect(Buffer.from(speechPcmFromWav(wav)).equals(Buffer.from(pcm))).toBe(
      true,
    );
  });

  test("accepts only whole frames below the native cap", () => {
    expect(assertCompleteSpeechPcm(3 * FRAME_BYTES)).toBe(3);
    expect(() => assertCompleteSpeechPcm(0)).toThrow(SpeechOutputError);
    expect(() => assertCompleteSpeechPcm(FRAME_BYTES + 2)).toThrow(
      "whole number of frames",
    );
    expect(() =>
      assertCompleteSpeechPcm(SPEECH_MAX_FRAMES * FRAME_BYTES),
    ).toThrow("frame cap");
  });
});

describe("warm tts-server speech supervisor", () => {
  test("creates the warm server once and reuses it across requests", async () => {
    const pcm = framePcm(2);
    const fake = startFakeTtsServer(() => pcmResponse(pcm));
    const { supervisor, calls } = createWarmHarness(fake.url);

    const first = await supervisor.generateSpeech({ text: "hello" });
    const second = await supervisor.generateSpeech({
      text: "again",
      voice: "harbor",
    });

    expect(calls.create).toBe(1);
    expect(supervisor.state()).toBe("running");
    expect(validateSpeechWav(first).sampleCount).toBe(
      2 * SPEECH_SAMPLES_PER_FRAME,
    );
    expect(Buffer.from(speechPcmFromWav(second)).equals(Buffer.from(pcm))).toBe(
      true,
    );
    expect(fake.requests).toHaveLength(2);
    expect(fake.requests[0]).toEqual({ input: "hello", voice: "default" });
    expect(fake.requests[1]).toEqual({ input: "again", voice: "harbor" });
    expect(await supervisor.processingSlots()).toBe(0);
    expect(supervisor.retainsRuntimeOnCancel()).toBe(true);

    await supervisor.shutdown();
    expect(calls.shutdown).toBe(1);
    expect(supervisor.retainsRuntimeOnCancel()).toBe(false);
  });

  test("streams even-length chunks and carries odd trailing bytes", async () => {
    const pcm = framePcm(1);
    const splits = [1, 2, 3, 1000, pcm.byteLength - 1006];
    const fake = startFakeTtsServer(() =>
      pcmResponse(
        new ReadableStream<Uint8Array>({
          async start(controller) {
            let offset = 0;
            for (const size of splits) {
              controller.enqueue(pcm.slice(offset, offset + size));
              offset += size;
              await Bun.sleep(1);
            }
            controller.close();
          },
        }),
      ),
    );
    const { supervisor } = createWarmHarness(fake.url);

    const stream = await supervisor.generateSpeechStream({ text: "stream" });
    expect(stream.sampleRate).toBe(24_000);
    const chunks = await readAll(stream.pcm);
    expect(chunks.length).toBeGreaterThan(1);
    for (const chunk of chunks) expect(chunk.byteLength % 2).toBe(0);
    expect(Buffer.from(concat(chunks)).equals(Buffer.from(pcm))).toBe(true);
    await supervisor.shutdown();
  });

  test("retries while the warm server reports it is busy", async () => {
    const pcm = framePcm(1);
    const fake = startFakeTtsServer((_request, attempt) =>
      attempt < 3 ? new Response("busy", { status: 503 }) : pcmResponse(pcm),
    );
    const { supervisor } = createWarmHarness(fake.url);

    const wav = await supervisor.generateSpeech({ text: "busy" });
    expect(fake.requests).toHaveLength(3);
    expect(validateSpeechWav(wav).sampleCount).toBe(SPEECH_SAMPLES_PER_FRAME);
    await supervisor.shutdown();
  });

  test("rejects a partial sample, partial frame, and unexpected format", async () => {
    const pcm = framePcm(1);
    let mode: "odd" | "frame" | "format" | "error" = "odd";
    const fake = startFakeTtsServer(() => {
      if (mode === "odd") return pcmResponse(pcm.slice(0, FRAME_BYTES - 1));
      if (mode === "frame") return pcmResponse(pcm.slice(0, FRAME_BYTES - 2));
      if (mode === "format")
        return pcmResponse(pcm, { "x-sample-rate": "16000" });
      return new Response("model exploded", { status: 500 });
    });
    const { supervisor } = createWarmHarness(fake.url);

    await expect(supervisor.generateSpeech({ text: "x" })).rejects.toThrow(
      "partial PCM sample",
    );
    mode = "frame";
    await expect(supervisor.generateSpeech({ text: "x" })).rejects.toThrow(
      "whole number of frames",
    );
    mode = "format";
    await expect(supervisor.generateSpeech({ text: "x" })).rejects.toThrow(
      "unexpected audio format",
    );
    mode = "error";
    await expect(supervisor.generateSpeech({ text: "x" })).rejects.toThrow(
      "HTTP 500): model exploded",
    );
    // Output failures do not tear down the warm server.
    expect(supervisor.state()).toBe("running");
    await supervisor.shutdown();
  });

  test("cancelling the stream aborts the upstream generation", async () => {
    const pcm = framePcm(1);
    let upstreamCancelled: () => void = () => {};
    const cancelled = new Promise<void>((resolve) => {
      upstreamCancelled = resolve;
    });
    const fake = startFakeTtsServer((request) => {
      request.signal.addEventListener("abort", () => upstreamCancelled());
      return pcmResponse(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(pcm.slice(0, 1024));
          },
          cancel() {
            upstreamCancelled();
          },
        }),
      );
    });
    const { supervisor, calls } = createWarmHarness(fake.url);

    const stream = await supervisor.generateSpeechStream({ text: "cancel" });
    const reader = stream.pcm.getReader();
    const first = await reader.read();
    expect(first.value?.byteLength).toBe(1024);
    await reader.cancel();
    await cancelled;

    expect(supervisor.retainsRuntimeOnCancel()).toBe(true);
    expect(calls.kill).toBe(0);
    await supervisor.shutdown();
  });

  test("an aborted request signal ends the stream as cancelled", async () => {
    const fake = startFakeTtsServer(() =>
      pcmResponse(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new Uint8Array(4));
          },
        }),
      ),
    );
    const { supervisor } = createWarmHarness(fake.url);
    const abort = new AbortController();

    const stream = await supervisor.generateSpeechStream({
      text: "abort",
      signal: abort.signal,
    });
    const reader = stream.pcm.getReader();
    await reader.read();
    abort.abort();
    await expect(reader.read()).rejects.toBeInstanceOf(
      SpeechGenerationAbortedError,
    );
    await supervisor.shutdown();
  });

  test("does not retain the runtime once the warm server has failed", async () => {
    const fake = startFakeTtsServer(() => pcmResponse(framePcm(1)));
    const { supervisor, setState } = createWarmHarness(fake.url);
    await supervisor.ensureRunning();
    expect(supervisor.retainsRuntimeOnCancel()).toBe(true);
    setState("failed");
    expect(supervisor.state()).toBe("failed");
    expect(supervisor.retainsRuntimeOnCancel()).toBe(false);
    await supervisor.shutdown();
  });

  test("admission refusal leaves the runtime idle so a later request can retry", async () => {
    const admission = Object.create(
      RuntimeMemoryAdmissionError.prototype,
    ) as RuntimeMemoryAdmissionError;
    const fake = startFakeTtsServer(() => pcmResponse(framePcm(1)));
    const { supervisor, calls } = createWarmHarness(fake.url, {
      ensureRunningError: admission,
    });

    await expect(supervisor.ensureRunning()).rejects.toBe(admission);
    expect(supervisor.state()).toBe("idle");
    expect(calls.kill).toBe(1);
    expect(supervisor.retainsRuntimeOnCancel()).toBe(false);
  });

  test("other startup failures mark the runtime failed", async () => {
    const fake = startFakeTtsServer(() => pcmResponse(framePcm(1)));
    const { supervisor } = createWarmHarness(fake.url, {
      ensureRunningError: new Error("tts-server exited during startup"),
    });

    await expect(supervisor.ensureRunning()).rejects.toThrow(
      "exited during startup",
    );
    expect(supervisor.state()).toBe("failed");
  });
});
