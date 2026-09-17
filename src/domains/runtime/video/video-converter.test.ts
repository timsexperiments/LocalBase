import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdtemp, readdir, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createVideoArtifactPreparer } from "./video-converter";

let fixtureRoot: string;
let executable: string;
beforeAll(async () => {
  fixtureRoot = await mkdtemp(join(tmpdir(), "localbase-converter-fixture-"));
  executable = join(fixtureRoot, "converter");
  const entrypoint = join(fixtureRoot, "fixture.ts");
  await Bun.write(
    entrypoint,
    `
    import { runBackendGuardian, BACKEND_GUARDIAN_COMMAND } from ${JSON.stringify(join(import.meta.dirname, "../backend-guardian.ts"))};
    if (process.argv[2] === BACKEND_GUARDIAN_COMMAND) process.exit(await runBackendGuardian(process.argv.slice(3)));
    const args = process.argv.slice(2);
    const output = args.at(-1);
    const mode = process.env.CONVERTER_FIXTURE_MODE;
    if (mode === "hold") {
      process.on("SIGTERM", () => {});
      process.send("ready");
      await new Promise(() => setInterval(() => {}, 1000));
    }
    if (mode === "failure") { process.stderr.write("private generated content\\n"); process.exit(1); }
    if (mode === "stderr-flood") process.stderr.write("private".repeat(100000));
    const bytes = mode === "oversized" ? new Uint8Array(65537) : new Uint8Array(40);
    const view = new DataView(bytes.buffer);
    for (const [offset, type, size] of [[0,"ftyp",16],[16,"moov",8],[24,"mdat",16]]) {
      view.setUint32(offset,size);
      for(let i=0;i<4;i++) bytes[offset+4+i]=type.charCodeAt(i);
    }
    if (mode === "invalid") bytes.fill(65);
    await Bun.write(output,bytes);
    process.stdout.write("frame=" + (mode === "truncated" ? 12 : 33) + "\\nprogress=end\\n");
  `,
  );
  const build = await Bun.build({
    entrypoints: [entrypoint],
    target: "bun",
    compile: { outfile: executable },
  });
  if (!build.success) throw new Error("Converter fixture compilation failed.");
});
afterAll(async () => {
  if (fixtureRoot) await rm(fixtureRoot, { recursive: true, force: true });
});

function avi() {
  const bytes = new Uint8Array(16);
  bytes.set(new TextEncoder().encode("RIFF"));
  bytes.set(new TextEncoder().encode("AVI "), 8);
  return {
    bytes,
    mimeType: "video/x-msvideo",
    outputFormat: "avi",
    fps: 16,
    frameCount: 33,
  } satisfies Parameters<
    ReturnType<typeof createVideoArtifactPreparer>
  >[0]["media"];
}

function fixture(
  mode: string,
  onSpawn?: (directory: string, command: string[]) => void,
  scheduleDeadline?: Parameters<
    typeof createVideoArtifactPreparer
  >[0]["scheduleDeadline"],
) {
  const ready = Promise.withResolvers<void>();
  const children: Bun.Subprocess[] = [];
  const guardians: Bun.Subprocess[] = [];
  const containmentFailures: unknown[] = [];
  const prepare = createVideoArtifactPreparer({
    ensureConverter: async () => executable,
    scheduleDeadline,
    spawn(command, directory) {
      const child = Bun.spawn(command, {
        cwd: directory,
        stdin: "ignore",
        stdout: "pipe",
        stderr: "pipe",
        env: { ...process.env, CONVERTER_FIXTURE_MODE: mode },
        ipc(message) {
          if (message === "ready") ready.resolve();
        },
      });
      children.push(child);
      onSpawn?.(directory, command);
      return child;
    },
    spawnGuardian(command) {
      const guardian = Bun.spawn([executable, ...command.slice(-3)], {
        stdin: "ignore",
        stdout: "ignore",
        stderr: "ignore",
      });
      guardians.push(guardian);
      return guardian;
    },
  });
  return {
    prepare,
    ready: ready.promise,
    children,
    guardians,
    containmentFailures,
    onContainmentFailure: (error: unknown) => containmentFailures.push(error),
  };
}

test.each(["success", "stderr-flood"])(
  "%s uses private files, fixed H264/AAC arguments, and cleans all owned processes",
  async (mode) => {
    const directory = await mkdtemp(
      join(tmpdir(), "localbase-converter-test-"),
    );
    let command: string[] = [];
    let permissions: Promise<number[]> | undefined;
    const converter = fixture(mode, (temporary, args) => {
      command = args;
      permissions = Promise.all(
        [
          temporary,
          join(temporary, "input.avi"),
          join(temporary, "output.mp4"),
        ].map(async (path) => (await stat(path)).mode & 0o777),
      );
    });
    try {
      const result = await converter.prepare({
        media: avi(),
        directory,
        signal: new AbortController().signal,
        maxArtifactBytes: 65536,
        onContainmentFailure: converter.onContainmentFailure,
      });
      expect(result).toMatchObject({
        mimeType: "video/mp4",
        outputFormat: "mp4",
        fps: 16,
        frameCount: 33,
      });
      expect(result.bytes.length).toBe(40);
      expect(await permissions).toEqual([0o700, 0o600, 0o600]);
      expect(command[0]).toBe(executable);
      expect(
        command.filter(
          (_value, index) => command[index - 1] === "-protocol_whitelist",
        ),
      ).toEqual(["file", "file"]);
      expect(
        command.filter((_value, index) => command[index - 1] === "-map"),
      ).toEqual(["0:v:0", "0:a:0?"]);
      for (const [flag, value] of [
        ["-c:v", "libx264"],
        ["-c:a", "aac"],
        ["-pix_fmt", "yuv420p"],
        ["-movflags", "+faststart"],
        ["-fs", "65536"],
      ]) {
        expect(command[command.indexOf(flag) + 1]).toBe(value);
      }
      expect(await readdir(directory)).toEqual([]);
      expect(converter.containmentFailures).toEqual([]);
      for (const child of [...converter.children, ...converter.guardians])
        expect(child.exitCode !== null || child.signalCode !== null).toBe(true);
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  },
);

test.each(["failure", "oversized", "truncated", "invalid"])(
  "%s fails without leaking diagnostics or leaving intermediate files",
  async (mode) => {
    const directory = await mkdtemp(
      join(tmpdir(), "localbase-converter-error-"),
    );
    const converter = fixture(mode);
    try {
      const result = await converter
        .prepare({
          media: avi(),
          directory,
          signal: new AbortController().signal,
          maxArtifactBytes: 65536,
          onContainmentFailure: converter.onContainmentFailure,
        })
        .then(
          () => null,
          (error: unknown) => error,
        );
      expect(result).toBeInstanceOf(Error);
      expect(String(result)).not.toContain("private generated content");
      expect(await readdir(directory)).toEqual([]);
      for (const child of [...converter.children, ...converter.guardians])
        expect(child.exitCode !== null || child.signalCode !== null).toBe(true);
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  },
);

test.each(["abort", "timeout"])(
  "%s stops even a SIGTERM-resistant converter before cleaning files",
  async (reason) => {
    const directory = await mkdtemp(
      join(tmpdir(), "localbase-converter-stop-"),
    );
    const controller = new AbortController();
    const deadline = Promise.withResolvers<() => void>();
    let deadlineCancelled = false;
    const converter = fixture("hold", undefined, (expire) => {
      deadline.resolve(expire);
      return () => {
        deadlineCancelled = true;
      };
    });
    try {
      const conversion = converter
        .prepare({
          media: avi(),
          directory,
          signal: controller.signal,
          maxArtifactBytes: 65536,
          onContainmentFailure: converter.onContainmentFailure,
        })
        .then(
          () => null,
          (error: unknown) => error,
        );
      await converter.ready;
      if (reason === "abort") controller.abort();
      else (await deadline.promise)();
      const error = await conversion;
      if (reason === "abort") expect(error).toBe(controller.signal.reason);
      else expect(String(error)).toContain("timed out");
      expect(converter.children[0]?.signalCode).toBe("SIGKILL");
      expect(deadlineCancelled).toBe(true);
      expect(await readdir(directory)).toEqual([]);
      expect(converter.containmentFailures).toEqual([]);
    } finally {
      controller.abort();
      await rm(directory, { recursive: true, force: true });
    }
  },
);

test("rejects invalid input, oversized input, and PATH-based executables before spawning", async () => {
  const directory = await mkdtemp(join(tmpdir(), "localbase-converter-input-"));
  const converter = fixture("success");
  const base = {
    directory,
    signal: new AbortController().signal,
    onContainmentFailure: converter.onContainmentFailure,
  };
  try {
    await expect(
      converter.prepare({ ...base, media: avi(), maxArtifactBytes: 8 }),
    ).rejects.toThrow("bounded AVI");
    await expect(
      converter.prepare({
        ...base,
        media: { ...avi(), bytes: new Uint8Array(16) },
        maxArtifactBytes: 65536,
      }),
    ).rejects.toThrow("bounded AVI");
    const relative = createVideoArtifactPreparer({
      ensureConverter: async () => "ffmpeg",
    });
    await expect(
      relative({ ...base, media: avi(), maxArtifactBytes: 65536 }),
    ).rejects.toThrow("managed absolute path");
    expect(converter.children).toHaveLength(0);
    expect(await readdir(directory)).toEqual([]);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
