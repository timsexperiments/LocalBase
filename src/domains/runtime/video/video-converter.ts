import { chmod, mkdtemp, open, rm } from "node:fs/promises";
import { isAbsolute, join } from "node:path";
import { guardianProcessCommand } from "../backend-guardian";
import { stopNativeProcess } from "../native-process";
import type { VideoJobManagerOptions } from "./video-job-manager";

type PrepareArtifact = NonNullable<VideoJobManagerOptions["prepareArtifact"]>;
const CONVERSION_TIMEOUT_MS = 120_000;
const DIAGNOSTIC_BYTES = 4_096;

function spawnConverter(command: string[], directory: string) {
  return Bun.spawn(command, {
    cwd: directory,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
}

async function writePrivate(path: string, bytes: Uint8Array): Promise<void> {
  const file = await open(path, "wx", 0o600);
  try {
    await file.writeFile(bytes);
  } finally {
    await file.close();
  }
}

async function drainDiagnostics(
  stream: ReadableStream<Uint8Array>,
): Promise<void> {
  // Drain without retaining or logging native diagnostics, which can contain media data.
  for await (const _chunk of stream) {
  }
}

async function readProgress(
  stream: ReadableStream<Uint8Array>,
): Promise<number> {
  const decoder = new TextDecoder();
  let buffer = "";
  let frames = -1;
  let ended = false;
  for await (const chunk of stream) {
    buffer += decoder.decode(chunk, { stream: true });
    let newline: number;
    while ((newline = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, newline).trim();
      buffer = buffer.slice(newline + 1);
      if (/^frame=\d+$/.test(line)) frames = Number(line.slice(6));
      if (line === "progress=end") ended = true;
    }
    if (buffer.length > DIAGNOSTIC_BYTES)
      throw new Error("Invalid video converter progress.");
  }
  if (!ended) throw new Error("Video converter did not finish.");
  return frames;
}

/** Converts only private AVI artifacts; no client-controlled command or path is accepted. */
export function createVideoArtifactPreparer(options: {
  ensureConverter: (signal: AbortSignal) => Promise<string>;
  timeoutMs?: number;
  spawn?: typeof spawnConverter;
  spawnGuardian?: (command: string[]) => Bun.Subprocess | undefined;
}): PrepareArtifact {
  return async ({
    media,
    directory,
    signal,
    maxArtifactBytes,
    onContainmentFailure,
  }) => {
    signal.throwIfAborted();
    if (
      media.outputFormat !== "avi" ||
      media.bytes.length < 12 ||
      media.bytes.length > maxArtifactBytes ||
      new TextDecoder().decode(media.bytes.subarray(0, 4)) !== "RIFF" ||
      new TextDecoder().decode(media.bytes.subarray(8, 12)) !== "AVI "
    ) {
      throw new Error("Video conversion requires a bounded AVI artifact.");
    }
    const deadline = new AbortController();
    const timer = setTimeout(
      () => deadline.abort(new Error("Video conversion timed out.")),
      options.timeoutMs ?? CONVERSION_TIMEOUT_MS,
    );
    const conversionSignal = AbortSignal.any([signal, deadline.signal]);
    let temporary: string | undefined;
    let child: ReturnType<typeof spawnConverter> | undefined;
    let guardian: Bun.Subprocess | undefined;
    let output: ReturnType<typeof readProgress> | undefined;
    let diagnostics: ReturnType<typeof drainDiagnostics> | undefined;
    let abortListener = () => {};
    const stopped = async (process: Bun.Subprocess) => {
      try {
        await stopNativeProcess(process, 500);
      } catch (error) {
        onContainmentFailure(error);
        // A failed stop must not release the job lease or remove files under a live child.
        await process.exited.catch(() => new Promise<never>(() => {}));
      }
    };
    try {
      const executable = await options.ensureConverter(conversionSignal);
      conversionSignal.throwIfAborted();
      if (!isAbsolute(executable))
        throw new Error("Video converter must use a managed absolute path.");
      temporary = await mkdtemp(join(directory, "convert-"));
      await chmod(temporary, 0o700);
      const inputPath = join(temporary, "input.avi");
      const outputPath = join(temporary, "output.mp4");
      await writePrivate(inputPath, media.bytes);
      await writePrivate(outputPath, new Uint8Array());
      conversionSignal.throwIfAborted();
      child = (options.spawn ?? spawnConverter)(
        [
          executable,
          "-nostdin",
          "-hide_banner",
          "-loglevel",
          "error",
          "-nostats",
          "-y",
          "-progress",
          "pipe:1",
          "-filter_threads",
          "2",
          "-filter_complex_threads",
          "2",
          "-protocol_whitelist",
          "file",
          "-threads",
          "2",
          "-f",
          "avi",
          "-i",
          inputPath,
          "-map",
          "0:v:0",
          "-map",
          "0:a:0?",
          "-map_metadata",
          "-1",
          "-map_chapters",
          "-1",
          "-c:v",
          "libx264",
          "-threads:v",
          "2",
          "-pix_fmt",
          "yuv420p",
          "-preset",
          "veryfast",
          "-crf",
          "23",
          "-c:a",
          "aac",
          "-threads:a",
          "2",
          "-b:a",
          "128k",
          "-movflags",
          "+faststart",
          "-fs",
          String(maxArtifactBytes),
          "-protocol_whitelist",
          "file",
          "-f",
          "mp4",
          outputPath,
        ],
        temporary,
      );
      output = readProgress(child.stdout);
      diagnostics = drainDiagnostics(child.stderr);
      const completion = Promise.all([child.exited, output, diagnostics]);
      void completion.catch(() => {});
      guardian = options.spawnGuardian
        ? options.spawnGuardian(guardianProcessCommand(process.pid, child.pid))
        : Bun.spawn(guardianProcessCommand(process.pid, child.pid), {
            stdin: "ignore",
            stdout: "ignore",
            stderr: "ignore",
            detached: true,
          });
      const aborted = new Promise<never>((_resolve, reject) => {
        abortListener = () => reject(conversionSignal.reason);
        conversionSignal.addEventListener("abort", abortListener, {
          once: true,
        });
        if (conversionSignal.aborted) abortListener();
      });
      const [exitCode, frames] = await Promise.race([completion, aborted]);
      conversionSignal.throwIfAborted();
      if (exitCode !== 0 || frames !== media.frameCount)
        throw new Error(
          "Video conversion failed or produced incomplete frames.",
        );
      const file = Bun.file(outputPath);
      const stat = await file.stat();
      if (!stat.isFile() || stat.size < 24 || stat.size >= maxArtifactBytes)
        throw new Error("Converted video exceeds its size limit or is empty.");
      const bytes = await file.bytes();
      if (bytes.length !== stat.size)
        throw new Error("Converted video changed while being read.");
      if (new TextDecoder().decode(bytes.subarray(4, 8)) !== "ftyp") {
        throw new Error("Video converter returned an unexpected format.");
      }
      conversionSignal.throwIfAborted();
      return {
        bytes,
        mimeType: "video/mp4",
        outputFormat: "mp4",
        fps: media.fps,
        frameCount: media.frameCount,
      };
    } finally {
      clearTimeout(timer);
      conversionSignal.removeEventListener("abort", abortListener);
      if (child) await stopped(child);
      if (guardian) await stopped(guardian);
      await Promise.allSettled([output, diagnostics]);
      if (temporary) await rm(temporary, { recursive: true, force: true });
    }
  };
}
