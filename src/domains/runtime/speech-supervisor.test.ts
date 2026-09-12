import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  compileSpeechRuntimeFixture,
  type SpeechFixtureControl,
} from "../../test/speech-runtime-fixture";
import {
  SpeechChildStopError,
  SpeechGenerationAbortedError,
  SpeechOutputError,
  SpeechSupervisor,
  speechNativeArguments,
  validateSpeechWav,
} from "./speech-supervisor";

type FixtureEvent = Readonly<{
  event: "started" | "stopping" | "stopped";
  pid: number;
  args?: string[];
  promptLength?: number;
}>;

function startReportServer(
  onEvent: (event: FixtureEvent) => void,
): Bun.Server<undefined> {
  for (let attempt = 0; attempt < 20; attempt++) {
    const random = new Uint32Array(1);
    crypto.getRandomValues(random);
    try {
      return Bun.serve({
        hostname: "127.0.0.1",
        port: 20_000 + (random[0]! % 40_000),
        async fetch(request) {
          onEvent((await request.json()) as FixtureEvent);
          return new Response(null, { status: 204 });
        },
      });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EADDRINUSE") throw error;
    }
  }
  throw new Error("Could not reserve a speech fixture report port.");
}

describe("bounded speech supervisor", () => {
  let root: string;
  let binaryPath: string;
  let controlPath: string;
  let eventsPath: string;
  let reportServer: Bun.Server<undefined>;
  const reportedEvents: FixtureEvent[] = [];
  const eventObservers: Array<(event: FixtureEvent) => void> = [];

  beforeAll(async () => {
    root = await mkdtemp(join(tmpdir(), "localbase-speech-supervisor-"));
    binaryPath = join(root, "llama-tts");
    controlPath = join(root, "control.json");
    eventsPath = join(root, "events.jsonl");
    reportServer = startReportServer((event) => {
      const observer = eventObservers.shift();
      if (observer) observer(event);
      else reportedEvents.push(event);
    });
    await compileSpeechRuntimeFixture(
      binaryPath,
      controlPath,
      eventsPath,
      `http://127.0.0.1:${reportServer.port}/event`,
    );
    await chmod(binaryPath, 0o755);
  });

  afterAll(async () => {
    reportServer.stop(true);
    await rm(root, { recursive: true, force: true });
  });

  async function waitForEvent(
    name: FixtureEvent["event"],
  ): Promise<FixtureEvent> {
    while (true) {
      const index = reportedEvents.findIndex(({ event }) => event === name);
      if (index >= 0) return reportedEvents.splice(index, 1)[0]!;
      const event = await new Promise<FixtureEvent>((resolve) => {
        eventObservers.push(resolve);
      });
      if (event.event === name) return event;
      reportedEvents.push(event);
    }
  }

  async function writeControl(control: SpeechFixtureControl): Promise<void> {
    await Bun.write(controlPath, JSON.stringify(control));
  }

  function createSupervisor(
    overrides: {
      spawnGuardian?: (command: string[]) => Bun.Subprocess | undefined;
      spawn?: (command: string[], cwd: string) => Bun.Subprocess;
      writePrivateFile?: (path: string, contents: string) => Promise<void>;
      removeTemporaryDirectory?: (path: string) => Promise<void>;
      childStopGraceMs?: number;
    } = {},
  ) {
    const memoryEvents: string[] = [];
    const memoryRequests: unknown[] = [];
    const memorySafety = {
      async reserve(request: unknown) {
        memoryRequests.push(request);
        memoryEvents.push("reserved");
        return {
          runtimeId: "speech:test:reservation",
          materialize: () => memoryEvents.push("materialized"),
          release: () => memoryEvents.push("released"),
        };
      },
    };
    const supervisor = new SpeechSupervisor({
      runtimeId: "tts:fixture:1",
      root,
      modelId: "fixture",
      logger: { event() {} } as never,
      memorySafety: memorySafety as never,
      spawnGuardian: overrides.spawnGuardian ?? (() => undefined),
      ...(overrides.spawn ? { spawn: overrides.spawn } : {}),
      ...(overrides.writePrivateFile
        ? { writePrivateFile: overrides.writePrivateFile }
        : {}),
      ...(overrides.removeTemporaryDirectory
        ? { removeTemporaryDirectory: overrides.removeTemporaryDirectory }
        : {}),
      childStopGraceMs: overrides.childStopGraceMs ?? 100,
      prepare: async () => ({
        binaryPath,
        modelPath: join(root, "model.gguf"),
        projectorPath: join(root, "mmproj.gguf"),
      }),
    });
    return { supervisor, memoryEvents, memoryRequests };
  }

  test("uses only bounded private-file native arguments and returns validated WAV", async () => {
    await writeControl({ mode: "success" });
    const { supervisor, memoryEvents, memoryRequests } = createSupervisor();
    const prompt = "private fixture prompt";
    const wav = await supervisor.generateSpeech({ text: prompt });
    const started = await waitForEvent("started");

    expect(validateSpeechWav(wav).sampleCount).toBe(3_840);
    expect(started.promptLength).toBe(Array.from(prompt).length);
    expect(started.args).toEqual(
      expect.arrayContaining([
        "--offline",
        "-n",
        "256",
        "-t",
        "4",
        "-tb",
        "4",
        "--tts-lang",
        "en",
      ]),
    );
    expect(started.args).not.toContain(prompt);
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(memoryRequests).toEqual([
      {
        runtimeId: "tts:fixture:1:generation:1",
        demand: {
          unifiedBytes: 8 * 1024 ** 3,
          hostBytes: 8 * 1024 ** 3,
          acceleratorBytes: 8 * 1024 ** 3,
          confidence: "estimated",
        },
      },
    ]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
  });

  test("fails closed for frame-cap, oversized, malformed, and inconsistent WAV output", async () => {
    for (const [mode, message] of [
      ["truncated", "frame cap"],
      ["oversized", "1 MiB"],
      ["malformed", "RIFF/WAVE"],
      ["sample-mismatch", "sample count"],
    ] as const) {
      await writeControl({ mode });
      const { supervisor, memoryEvents } = createSupervisor();
      await expect(
        supervisor.generateSpeech({ text: "bounded output" }),
      ).rejects.toThrow(message);
      await waitForEvent("started");
      expect(memoryEvents).toEqual(["reserved", "released"]);
      expect(await readdir(join(root, "tmp"))).toEqual([]);
    }
  });

  test("holds the reservation and private files until a cancelled child exits", async () => {
    const releasePath = join(root, `release-${crypto.randomUUID()}`);
    const exitReleasePath = join(root, `exit-release-${crypto.randomUUID()}`);
    await writeControl({ mode: "hold", releasePath, exitReleasePath });
    const { supervisor, memoryEvents } = createSupervisor();
    const cancellation = new AbortController();
    const generation = supervisor.generateSpeech({
      text: "cancel this speech",
      signal: cancellation.signal,
    });
    const generationFailure = generation.catch((error: unknown) => error);
    let failure: unknown;
    try {
      const started = await waitForEvent("started");
      expect(memoryEvents).toEqual(["reserved"]);
      const temporaryDirectories = await readdir(join(root, "tmp"));
      expect(temporaryDirectories).toHaveLength(1);
      const temporaryDirectory = join(root, "tmp", temporaryDirectories[0]!);
      expect((await stat(temporaryDirectory)).mode & 0o777).toBe(0o700);
      const promptPath = started.args?.[started.args.indexOf("-f") + 1];
      const outputPath = started.args?.[started.args.indexOf("-o") + 1];
      if (!promptPath || !outputPath)
        throw new Error("Missing private file paths.");
      expect((await stat(promptPath)).mode & 0o777).toBe(0o600);
      expect((await stat(outputPath)).mode & 0o777).toBe(0o600);

      cancellation.abort();
      await waitForEvent("stopping");
      expect(memoryEvents).toEqual(["reserved"]);
    } finally {
      cancellation.abort();
      try {
        await Bun.write(exitReleasePath, "release");
      } finally {
        failure = await generationFailure;
      }
    }
    expect(failure).toBeInstanceOf(SpeechGenerationAbortedError);
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
  });

  test("shutdown waits for an active child and its cleanup", async () => {
    const releasePath = join(root, `release-${crypto.randomUUID()}`);
    const exitReleasePath = join(root, `exit-release-${crypto.randomUUID()}`);
    await writeControl({ mode: "hold", releasePath, exitReleasePath });
    const { supervisor, memoryEvents } = createSupervisor();
    const generation = supervisor.generateSpeech({ text: "shutdown speech" });
    const generationFailure = generation.catch((error: unknown) => error);
    let shutdown: Promise<void> | undefined;
    let failure: unknown;
    try {
      await waitForEvent("started");
      shutdown = supervisor.shutdown();
      await waitForEvent("stopping");
      expect(memoryEvents).toEqual(["reserved"]);
    } finally {
      shutdown ??= supervisor.shutdown();
      try {
        await Bun.write(exitReleasePath, "release");
      } finally {
        [failure] = await Promise.all([generationFailure, shutdown]);
      }
    }
    expect(failure).toBeInstanceOf(SpeechGenerationAbortedError);
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
  });

  test("stops the child when guardian creation fails", async () => {
    const releasePath = join(root, `never-release-${crypto.randomUUID()}`);
    await writeControl({ mode: "hold", releasePath });
    const guardianFailure = new Error("guardian creation failed");
    const { supervisor, memoryEvents } = createSupervisor({
      spawnGuardian() {
        throw guardianFailure;
      },
    });

    let failure: unknown;
    try {
      await supervisor.generateSpeech({ text: "guardian failure" });
    } catch (error) {
      failure = error;
    }

    expect(failure).toBe(guardianFailure);
    expect(await Bun.file(releasePath).exists()).toBe(false);
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
  });

  test("preserves generation failure and completes bookkeeping when cleanup fails", async () => {
    await writeControl({ mode: "malformed" });
    const cleanupFailure = new Error("cleanup failed");
    const { supervisor, memoryEvents } = createSupervisor({
      removeTemporaryDirectory: async () => {
        throw cleanupFailure;
      },
    });

    let failure: unknown;
    try {
      await supervisor.generateSpeech({ text: "cleanup failure" });
    } catch (error) {
      failure = error;
    }

    expect(failure).toBeInstanceOf(SpeechOutputError);
    expect((failure as Error).message).toContain("RIFF/WAVE");
    expect(memoryEvents).toEqual(["reserved", "released"]);
    await supervisor.kill();
    const leftovers = await readdir(join(root, "tmp"));
    expect(leftovers).toHaveLength(1);
    await rm(join(root, "tmp", leftovers[0]!), {
      recursive: true,
      force: true,
    });
  });

  test("does not spawn after cancellation crosses a private-file write", async () => {
    let releaseWrite: () => void = () => {
      throw new Error("Write-release barrier was not initialized.");
    };
    const writeReleased = new Promise<void>((resolve) => {
      releaseWrite = resolve;
    });
    let reachWrite: () => void;
    const writeReached = new Promise<void>((resolve) => {
      reachWrite = resolve;
    });
    let spawnCalls = 0;
    const { supervisor, memoryEvents } = createSupervisor({
      async writePrivateFile(path, contents) {
        await writeFile(path, contents, { mode: 0o600, flag: "wx" });
        if (path.endsWith("output.wav")) {
          reachWrite();
          await writeReleased;
        }
      },
      spawn() {
        spawnCalls += 1;
        throw new Error("child must not spawn");
      },
    });
    const cancellation = new AbortController();
    const generation = supervisor.generateSpeech({
      text: "cancel before spawn",
      signal: cancellation.signal,
    });

    await writeReached;
    cancellation.abort();
    releaseWrite();

    await expect(generation).rejects.toBeInstanceOf(
      SpeechGenerationAbortedError,
    );
    expect(spawnCalls).toBe(0);
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
  });

  test("retains ownership until a failed-to-signal child actually exits", async () => {
    let resolveExit: (exitCode: number) => void = () => {
      throw new Error("Exit barrier was not initialized.");
    };
    const exited = new Promise<number>((resolve) => {
      resolveExit = resolve;
    });
    let closeDiagnostics: () => void = () => {
      throw new Error("Diagnostics barrier was not initialized.");
    };
    const stderr = new ReadableStream<Uint8Array>({
      start(controller) {
        closeDiagnostics = () => controller.close();
      },
    });
    let reportSpawned: () => void;
    const spawned = new Promise<void>((resolve) => {
      reportSpawned = resolve;
    });
    let reportCleanup: () => void;
    const cleanupCompleted = new Promise<void>((resolve) => {
      reportCleanup = resolve;
    });
    const fakeProcess = {
      pid: 42_424,
      exited,
      stderr,
      kill() {
        throw new Error("signal failed");
      },
    } as unknown as Bun.Subprocess;
    const { supervisor, memoryEvents } = createSupervisor({
      childStopGraceMs: 1,
      spawn() {
        reportSpawned();
        return fakeProcess;
      },
      async removeTemporaryDirectory(path) {
        await rm(path, { recursive: true, force: true });
        reportCleanup();
      },
    });
    const cancellation = new AbortController();
    const generation = supervisor.generateSpeech({
      text: "failed signal",
      signal: cancellation.signal,
    });

    await spawned;
    cancellation.abort();
    await expect(generation).rejects.toThrow("could not be stopped");
    expect(supervisor.state()).toBe("failed");
    expect(memoryEvents).toEqual(["reserved"]);
    expect(await readdir(join(root, "tmp"))).toHaveLength(1);
    await expect(supervisor.kill()).rejects.toBeInstanceOf(
      SpeechChildStopError,
    );

    closeDiagnostics();
    resolveExit(0);
    await cleanupCompleted;
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
    await supervisor.kill();
  });

  test("retains ownership after guardian stop failure until the guardian exits", async () => {
    await writeControl({ mode: "success" });
    let resolveGuardianExit: (exitCode: number) => void = () => {
      throw new Error("Guardian exit barrier was not initialized.");
    };
    const guardianExited = new Promise<number>((resolve) => {
      resolveGuardianExit = resolve;
    });
    const guardian = {
      pid: 42_425,
      exitCode: null,
      signalCode: null,
      exited: guardianExited,
      kill() {
        throw new Error("guardian signal failed");
      },
    } as unknown as Bun.Subprocess;
    const { supervisor, memoryEvents } = createSupervisor({
      childStopGraceMs: 1,
      spawnGuardian: () => guardian,
    });

    await expect(
      supervisor.generateSpeech({ text: "guardian stop failure" }),
    ).rejects.toThrow("Speech generation cleanup failed");
    expect(supervisor.state()).toBe("failed");
    expect(memoryEvents).toEqual(["reserved", "released"]);
    expect(await readdir(join(root, "tmp"))).toEqual([]);
    await expect(supervisor.kill()).rejects.toBeInstanceOf(
      SpeechChildStopError,
    );

    resolveGuardianExit(0);
    await guardianExited;
    await supervisor.kill();
    expect(supervisor.state()).toBe("idle");
  });
});

test("rejects malformed WAV boundaries", () => {
  const preparation = {
    binaryPath: "/runtime/llama-tts",
    modelPath: "/models/model.gguf",
    projectorPath: "/models/mmproj.gguf",
  };
  const args = speechNativeArguments(
    preparation,
    "/private/prompt.txt",
    "/private/output.wav",
  );
  expect(args.join(" ")).not.toContain("secret prompt");
  expect(() => validateSpeechWav(new TextEncoder().encode("not wav"))).toThrow(
    SpeechOutputError,
  );
});
