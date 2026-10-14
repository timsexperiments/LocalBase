import { appendFileSync, watch } from "node:fs";
import { dirname, join } from "node:path";

const fixtureEntrypoint = join(
  import.meta.dirname,
  "speech-runtime-fixture.ts",
);

declare const __LOCALBASE_TEST_SPEECH_CONTROL_PATH__: string;
declare const __LOCALBASE_TEST_SPEECH_EVENTS_PATH__: string;
declare const __LOCALBASE_TEST_SPEECH_REPORT_URL__: string | undefined;

export type SpeechFixtureMode =
  | "success"
  | "hold"
  | "truncated"
  | "malformed"
  | "oversized"
  | "sample-mismatch"
  | "failure";

export type SpeechFixtureControl = Readonly<{
  mode: SpeechFixtureMode;
  releasePath?: string;
  exitReleasePath?: string;
}>;

export async function compileSpeechRuntimeFixture(
  outputPath: string,
  controlPath: string,
  eventsPath: string,
  reportUrl?: string,
): Promise<void> {
  const result = await Bun.build({
    entrypoints: [fixtureEntrypoint],
    target: "bun",
    compile: { outfile: outputPath },
    define: {
      __LOCALBASE_TEST_SPEECH_CONTROL_PATH__: JSON.stringify(controlPath),
      __LOCALBASE_TEST_SPEECH_EVENTS_PATH__: JSON.stringify(eventsPath),
      __LOCALBASE_TEST_SPEECH_REPORT_URL__: JSON.stringify(reportUrl),
    },
  });
  if (!result.success) {
    throw new Error(
      `Could not compile speech runtime fixture: ${result.logs.map((log) => log.message).join("\n")}`,
    );
  }
}

function argumentValue(args: string[], name: string): string {
  const index = args.indexOf(name);
  const value = index < 0 ? undefined : args[index + 1];
  if (!value) throw new Error(`Speech fixture requires ${name}.`);
  return value;
}

async function waitForPath(path: string): Promise<void> {
  if (await Bun.file(path).exists()) return;
  await new Promise<void>((resolve, reject) => {
    let settled = false;
    const inspect = async () => {
      if (settled) return;
      if (!(await Bun.file(path).exists())) return;
      settled = true;
      watcher.close();
      resolve();
    };
    const watcher = watch(dirname(path), (_event, filename) => {
      if (filename?.toString() === path.slice(dirname(path).length + 1)) {
        void inspect();
      }
    });
    watcher.on("error", (error) => {
      settled = true;
      watcher.close();
      reject(error);
    });
    void inspect();
  });
}

function wav(sampleCount: number): Uint8Array {
  const dataLength = sampleCount * 2;
  const bytes = new Uint8Array(44 + dataLength);
  const view = new DataView(bytes.buffer);
  const writeText = (offset: number, value: string) => {
    for (let index = 0; index < value.length; index++) {
      bytes[offset + index] = value.charCodeAt(index);
    }
  };
  writeText(0, "RIFF");
  view.setUint32(4, bytes.byteLength - 8, true);
  writeText(8, "WAVE");
  writeText(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, 24_000, true);
  view.setUint32(28, 48_000, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  writeText(36, "data");
  view.setUint32(40, dataLength, true);
  return bytes;
}

async function record(event: Record<string, unknown>): Promise<void> {
  appendFileSync(
    __LOCALBASE_TEST_SPEECH_EVENTS_PATH__,
    `${JSON.stringify(event)}\n`,
  );
  if (typeof __LOCALBASE_TEST_SPEECH_REPORT_URL__ === "string") {
    const response = await fetch(__LOCALBASE_TEST_SPEECH_REPORT_URL__, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(event),
    });
    if (!response.ok) throw new Error("Could not report speech fixture event.");
  }
}

async function runSpeechFixture(): Promise<void> {
  const args = Bun.argv.slice(2);
  const promptPath = argumentValue(args, "-f");
  const outputPath = argumentValue(args, "-o");
  const control = JSON.parse(
    await Bun.file(__LOCALBASE_TEST_SPEECH_CONTROL_PATH__).text(),
  ) as SpeechFixtureControl;
  const promptLength = Array.from(await Bun.file(promptPath).text()).length;
  let stopping = false;
  process.on("SIGTERM", () => {
    if (stopping) return;
    stopping = true;
    void (async () => {
      await record({ event: "stopping", pid: process.pid });
      if (control.exitReleasePath) await waitForPath(control.exitReleasePath);
      await record({ event: "stopped", pid: process.pid });
      process.exit(0);
    })();
  });
  await record({ event: "started", pid: process.pid, args, promptLength });

  if (control.mode === "hold") {
    if (!control.releasePath)
      throw new Error("Hold mode requires releasePath.");
    await waitForPath(control.releasePath);
  }
  if (control.mode === "failure") process.exit(1);
  if (control.mode === "malformed") {
    await Bun.write(outputPath, "not a wav");
    console.error("generated 2 frames, 9 bytes of WAV audio");
    return;
  }
  if (control.mode === "oversized") {
    await Bun.write(outputPath, new Uint8Array(1024 * 1024 + 1));
    console.error("generated 2 frames, 1048577 bytes of WAV audio");
    return;
  }

  const frames = control.mode === "truncated" ? 256 : 2;
  const samples = control.mode === "sample-mismatch" ? 1_920 : frames * 1_920;
  const bytes = wav(samples);
  await Bun.write(outputPath, bytes);
  console.error(
    `generated ${frames} frames, ${bytes.byteLength} bytes of WAV audio`,
  );
}

const SAMPLES_PER_FRAME = 1_920;

function pcm(sampleCount: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(sampleCount * 2);
}

function pcmResponse(bytes: Uint8Array<ArrayBuffer>): Response {
  return new Response(bytes, {
    headers: {
      "content-type": "application/octet-stream",
      "x-audio-format": "s16le",
      "x-sample-rate": "24000",
    },
  });
}

/** Fakes the warm tts-server: /health, /slots and streaming /v1/generate. */
async function runSpeechServerFixture(args: string[]): Promise<void> {
  const host = argumentValue(args, "--host");
  const port = Number(argumentValue(args, "--port"));
  let processing = 0;
  let stopping = false;
  let control: SpeechFixtureControl | undefined;
  process.on("SIGTERM", () => {
    if (stopping) return;
    stopping = true;
    void (async () => {
      await record({ event: "stopping", pid: process.pid });
      if (control?.exitReleasePath) await waitForPath(control.exitReleasePath);
      await record({ event: "stopped", pid: process.pid });
      process.exit(0);
    })();
  });
  Bun.serve({
    hostname: host,
    port,
    async fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/health") return Response.json({ status: "ok" });
      if (pathname === "/slots") {
        return Response.json([{ id: 0, is_processing: processing > 0 }]);
      }
      if (pathname !== "/v1/generate" || request.method !== "POST") {
        return new Response("not found", { status: 404 });
      }
      if (processing > 0) return new Response("busy", { status: 503 });
      processing++;
      try {
        const body = (await request.json()) as {
          input: string;
          voice: string;
        };
        control = JSON.parse(
          await Bun.file(__LOCALBASE_TEST_SPEECH_CONTROL_PATH__).text(),
        ) as SpeechFixtureControl;
        await record({
          event: "started",
          pid: process.pid,
          args,
          promptLength: Array.from(body.input).length,
          voice: body.voice,
        });
        if (control.mode === "hold") {
          if (!control.releasePath)
            throw new Error("Hold mode requires releasePath.");
          await waitForPath(control.releasePath);
        }
        if (control.mode === "failure") {
          return new Response("generation failed", { status: 500 });
        }
        if (control.mode === "malformed")
          return pcmResponse(pcm(1).subarray(1));
        if (control.mode === "oversized") {
          return pcmResponse(pcm(256 * SAMPLES_PER_FRAME + 1));
        }
        if (control.mode === "truncated") {
          return pcmResponse(pcm(256 * SAMPLES_PER_FRAME));
        }
        if (control.mode === "sample-mismatch") {
          return pcmResponse(pcm(SAMPLES_PER_FRAME + 1));
        }
        return pcmResponse(pcm(2 * SAMPLES_PER_FRAME));
      } finally {
        processing--;
      }
    },
  });
}

if (import.meta.main) {
  const args = Bun.argv.slice(2);
  if (args.includes("--port")) await runSpeechServerFixture(args);
  else await runSpeechFixture();
}
