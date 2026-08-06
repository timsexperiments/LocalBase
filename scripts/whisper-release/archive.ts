import { mkdir } from "node:fs/promises";
import { basename, join, resolve } from "node:path";
import { Readable } from "node:stream";
import { unzipSync } from "fflate";
import { extract as createTarExtractor, type Headers } from "tar-stream";
import { z } from "zod";
import {
  archiveSpecification,
  filePathSchema,
  teamIdSchema,
  whisperTargetSchema,
  type WhisperTarget,
} from "./contracts";

type CommandOutput = { stdout: string; stderr: string };
export type CommandRunner = (args: string[]) => Promise<CommandOutput>;

function readUInt32LE(bytes: Uint8Array, offset: number): number {
  return (
    (bytes[offset]! |
      (bytes[offset + 1]! << 8) |
      (bytes[offset + 2]! << 16) |
      (bytes[offset + 3]! << 24)) >>>
    0
  );
}

function validateBinaryArchitecture(
  target: WhisperTarget,
  bytes: Uint8Array,
): void {
  if (target === "linux-x64") {
    const valid =
      bytes.length >= 20 &&
      bytes[0] === 0x7f &&
      bytes[1] === 0x45 &&
      bytes[2] === 0x4c &&
      bytes[3] === 0x46 &&
      bytes[4] === 2 &&
      bytes[5] === 1 &&
      bytes[18] === 0x3e &&
      bytes[19] === 0;
    if (valid) return;
  } else {
    const valid =
      bytes.length >= 8 &&
      readUInt32LE(bytes, 0) === 0xfeedfacf &&
      readUInt32LE(bytes, 4) === 0x0100000c;
    if (valid) return;
  }
  throw new Error(`whisper-server does not match ${target} architecture.`);
}

async function readTarGzRuntime(archivePath: string): Promise<Uint8Array> {
  const extractor = createTarExtractor();
  const entries: Array<{ header: Headers; bytes: Uint8Array }> = [];
  let entryError: unknown;
  const completed = new Promise<void>((resolveExtraction, rejectExtraction) => {
    extractor.once("finish", () => {
      if (entryError) rejectExtraction(entryError);
      else resolveExtraction();
    });
    extractor.once("error", rejectExtraction);
    extractor.on(
      "entry",
      (header: Headers, entry: Readable, next: (error?: unknown) => void) => {
        void (async () => {
          try {
            const chunks: Uint8Array[] = [];
            for await (const chunk of entry) chunks.push(chunk);
            const length = chunks.reduce(
              (total, chunk) => total + chunk.length,
              0,
            );
            const bytes = new Uint8Array(length);
            let offset = 0;
            for (const chunk of chunks) {
              bytes.set(chunk, offset);
              offset += chunk.length;
            }
            entries.push({ header, bytes });
          } catch (error) {
            entryError ??= error;
          }
          next();
        })();
      },
    );
  });

  Readable.fromWeb(
    Bun.file(archivePath)
      .stream()
      .pipeThrough(
        new DecompressionStream("gzip"),
      ) as unknown as import("node:stream/web").ReadableStream,
  ).pipe(extractor);
  await completed;

  if (
    entries.length !== 1 ||
    entries[0]!.header.name !== "whisper-server" ||
    (entries[0]!.header.type && entries[0]!.header.type !== "file") ||
    entries[0]!.bytes.length === 0
  ) {
    throw new Error(
      "Linux archive must contain exactly one non-empty root-level whisper-server file.",
    );
  }
  return entries[0]!.bytes;
}

async function readZipRuntime(archivePath: string): Promise<Uint8Array> {
  let files: Record<string, Uint8Array>;
  try {
    files = unzipSync(
      new Uint8Array(await Bun.file(archivePath).arrayBuffer()),
    );
  } catch (error) {
    throw new Error("Invalid macOS ZIP archive.", { cause: error });
  }
  const entries = Object.entries(files);
  if (
    entries.length !== 1 ||
    entries[0]![0] !== "whisper-server" ||
    entries[0]![1].length === 0
  ) {
    throw new Error(
      "macOS archive must contain exactly one non-empty root-level whisper-server file.",
    );
  }
  return entries[0]![1];
}

async function runProcess(args: string[]): Promise<CommandOutput> {
  const process = Bun.spawn(args, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
    process.exited,
  ]);
  if (exitCode !== 0) {
    throw new Error(
      `${basename(args[0]!)} failed with exit ${exitCode}.\nstdout:\n${stdout}\nstderr:\n${stderr}`,
    );
  }
  return { stdout, stderr };
}

const codeSignDetailsSchema = z
  .object({
    authorities: z.array(z.string().min(1)).min(1),
    teamIdentifier: z.string().min(1),
    codeDirectory: z.string().min(1),
  })
  .strict();

function parseCodeSignDetails(output: string) {
  const authorities = Array.from(
    output.matchAll(/^Authority=(.+)$/gm),
    (match) => match[1]!.trim(),
  );
  const teamIdentifier = /^TeamIdentifier=(.+)$/m.exec(output)?.[1]?.trim();
  const codeDirectory = /^CodeDirectory=(.+)$/m.exec(output)?.[1]?.trim();
  return codeSignDetailsSchema.parse({
    authorities,
    teamIdentifier,
    codeDirectory,
  });
}

async function qualifyMacosSignature(
  runtimePath: string,
  teamId: string,
  commandRunner: CommandRunner,
): Promise<void> {
  await commandRunner([
    "/usr/bin/codesign",
    "--verify",
    "--strict",
    "--verbose=2",
    runtimePath,
  ]);
  const details = await commandRunner([
    "/usr/bin/codesign",
    "--display",
    "--verbose=4",
    runtimePath,
  ]);
  const parsed = parseCodeSignDetails(`${details.stdout}\n${details.stderr}`);
  const expectedAuthority = new RegExp(
    `^Developer ID Application: .+ \\(${teamId}\\)$`,
  );
  if (
    !parsed.authorities.some((authority) => expectedAuthority.test(authority))
  ) {
    throw new Error(
      `whisper-server is not signed by a Developer ID Application certificate for team ${teamId}.`,
    );
  }
  if (parsed.teamIdentifier !== teamId) {
    throw new Error(
      `whisper-server signature team is ${parsed.teamIdentifier}, expected ${teamId}.`,
    );
  }
  if (!/\bruntime\b/.test(parsed.codeDirectory)) {
    throw new Error(
      "whisper-server signature does not enable the hardened runtime.",
    );
  }
}

export async function qualifyWhisperArchive(
  targetInput: unknown,
  archiveInput: unknown,
  workDirectoryInput: unknown,
  teamIdInput: unknown,
  commandRunner: CommandRunner = runProcess,
): Promise<void> {
  const target = whisperTargetSchema.parse(targetInput);
  const archivePath = filePathSchema.parse(archiveInput);
  const workDirectory = filePathSchema.parse(workDirectoryInput);
  const teamId =
    teamIdInput === undefined ? undefined : teamIdSchema.parse(teamIdInput);
  const specification = archiveSpecification[target];
  if (basename(archivePath) !== specification.assetName) {
    throw new Error(`Archive must be named ${specification.assetName}.`);
  }
  const archive = Bun.file(archivePath);
  if (!(await archive.exists())) {
    throw new Error(`Archive does not exist: ${archivePath}.`);
  }
  const binary =
    target === "linux-x64"
      ? await readTarGzRuntime(archivePath)
      : await readZipRuntime(archivePath);
  validateBinaryArchitecture(target, binary);
  await mkdir(workDirectory, { recursive: true });
  const runtimePath = join(resolve(workDirectory), "whisper-server");
  await Bun.write(runtimePath, binary);
  if (target === "macos-arm64") {
    if (!teamId) {
      throw new Error("macOS archive qualification requires --team-id.");
    }
    await qualifyMacosSignature(runtimePath, teamId, commandRunner);
  } else if (teamId) {
    throw new Error("Linux archive qualification does not accept --team-id.");
  }
}
