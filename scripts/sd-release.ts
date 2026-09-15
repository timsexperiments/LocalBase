import { runMain } from "citty";
import { defineCommand } from "citty";
import { basename, join } from "node:path";
import { Readable } from "node:stream";
import { unzipSync } from "fflate";
import { extract as createTarExtractor, type Headers } from "tar-stream";
import { z } from "zod";

export const sdTargetSchema = z.enum(["linux-x64", "macos-arm64"]);
export type SdTarget = z.infer<typeof sdTargetSchema>;

const licenseFiles = [
  "LICENSE.darts-clone.txt",
  "LICENSE.ggml.txt",
  "LICENSE.libwebm.txt",
  "LICENSE.libwebp.txt",
  "LICENSE.nlohmann-json.txt",
  "LICENSE.oniguruma.txt",
  "LICENSE.stable-diffusion.cpp.txt",
  "LICENSE.utf8proc.txt",
] as const;

type ArchiveEntry = { name: string; type?: string; bytes: Uint8Array };

const sourceProvenanceSchema = z
  .object({
    version: z.literal(1),
    sources: z.tuple([
      z.object({
        name: z.literal("stable-diffusion.cpp"),
        revision: z.literal("07a85c74cb08cda3aa176f688c5d8f522615e2b9"),
        sha256: z.literal(
          "a1850648d5fd6e12b23d8f290023563c7dad74ec64302d9ce6862ab880f3d819",
        ),
        url: z.string().url(),
      }),
      z.object({
        name: z.literal("ggml"),
        revision: z.literal("e20c3a14aa70ee84ca58499814206dd08d8026bc"),
        sha256: z.literal(
          "3dde7c76c0dc2bce436ab16258baafbf3f1a1dbf933a3583ca32471aadb0e160",
        ),
        url: z.string().url(),
      }),
      z.object({
        name: z.literal("libwebp"),
        revision: z.literal("0c9546f7efc61eac7f79ae115c3f99c91c21c443"),
        sha256: z.literal(
          "6cb433070b4461179067b0901a682e4e14b220354fc35884c1b1315adedefc99",
        ),
        url: z.string().url(),
      }),
      z.object({
        name: z.literal("libwebm"),
        revision: z.literal("5bf12267eea773a32fcf4949de52b0add158a8d5"),
        sha256: z.literal(
          "294049a03d35e4480a94a5ced96c80ebfd4114554966709c8c97de4f19bd941a",
        ),
        url: z.string().url(),
      }),
    ]),
    patch: z.object({
      name: z.literal("inline-wav-audio.patch"),
      sha256: z.string().regex(/^[a-f0-9]{64}$/),
    }),
  })
  .strict();

export function validateSdArchiveEntries(
  target: SdTarget,
  entries: ArchiveEntry[],
): Uint8Array {
  const expected = [
    "sd-server",
    "SOURCE.sd-server.json",
    ...licenseFiles,
  ].sort();
  const names = entries.map((entry) => entry.name).sort();
  if (JSON.stringify(names) !== JSON.stringify(expected)) {
    throw new Error(
      `${target} archive must contain only sd-server, source provenance, and the eight required license files.`,
    );
  }
  for (const entry of entries) {
    if ((entry.type && entry.type !== "file") || entry.bytes.length === 0) {
      throw new Error(
        `${target} archive entry ${entry.name} is not a non-empty file.`,
      );
    }
  }
  const provenance = entries.find(
    (entry) => entry.name === "SOURCE.sd-server.json",
  )!;
  try {
    sourceProvenanceSchema.parse(
      JSON.parse(new TextDecoder().decode(provenance.bytes)),
    );
  } catch (error) {
    throw new Error(`${target} archive has invalid source provenance.`, {
      cause: error,
    });
  }
  return entries.find((entry) => entry.name === "sd-server")!.bytes;
}

function readU32(bytes: Uint8Array, offset: number): number {
  return (
    (bytes[offset]! |
      (bytes[offset + 1]! << 8) |
      (bytes[offset + 2]! << 16) |
      (bytes[offset + 3]! << 24)) >>>
    0
  );
}

export function validateSdBinaryArchitecture(
  target: SdTarget,
  bytes: Uint8Array,
): void {
  const valid =
    target === "linux-x64"
      ? bytes.length >= 20 &&
        bytes[0] === 0x7f &&
        bytes[1] === 0x45 &&
        bytes[2] === 0x4c &&
        bytes[3] === 0x46 &&
        bytes[4] === 2 &&
        bytes[5] === 1 &&
        bytes[18] === 0x3e &&
        bytes[19] === 0
      : bytes.length >= 8 &&
        readU32(bytes, 0) === 0xfeedfacf &&
        readU32(bytes, 4) === 0x0100000c;
  if (!valid)
    throw new Error(`sd-server does not match ${target} architecture.`);
}

async function readTarGz(path: string): Promise<ArchiveEntry[]> {
  const extractor = createTarExtractor();
  const entries: ArchiveEntry[] = [];
  let entryError: unknown;
  const completed = new Promise<void>((resolve, reject) => {
    extractor.once("finish", () =>
      entryError ? reject(entryError) : resolve(),
    );
    extractor.once("error", reject);
    extractor.on(
      "entry",
      (header: Headers, stream: Readable, next: (error?: unknown) => void) => {
        void (async () => {
          try {
            const chunks: Uint8Array[] = [];
            for await (const chunk of stream) chunks.push(chunk);
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
            entries.push({ name: header.name, type: header.type, bytes });
          } catch (error) {
            entryError ??= error;
          }
          next();
        })();
      },
    );
  });
  Readable.fromWeb(
    Bun.file(path)
      .stream()
      .pipeThrough(
        new DecompressionStream("gzip"),
      ) as unknown as import("node:stream/web").ReadableStream,
  ).pipe(extractor);
  await completed;
  return entries;
}

async function readZip(path: string): Promise<ArchiveEntry[]> {
  let files: Record<string, Uint8Array>;
  try {
    files = unzipSync(new Uint8Array(await Bun.file(path).arrayBuffer()));
  } catch (error) {
    throw new Error("Invalid macOS sd-server ZIP archive.", { cause: error });
  }
  return Object.entries(files).map(([name, bytes]) => ({ name, bytes }));
}

async function run(args: string[]) {
  const child = Bun.spawn(args, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) {
    throw new Error(
      `${basename(args[0]!)} failed with exit ${code}.\n${stdout}${stderr}`,
    );
  }
  return `${stdout}\n${stderr}`;
}

export async function qualifySdArchive(
  target: SdTarget,
  archive: string,
  extractionDirectory: string,
  teamId?: string,
) {
  const entries =
    target === "linux-x64" ? await readTarGz(archive) : await readZip(archive);
  const binary = validateSdArchiveEntries(target, entries);
  validateSdBinaryArchitecture(target, binary);

  if (target === "macos-arm64" && teamId) {
    if (!/^[A-Z0-9]{10}$/.test(teamId)) {
      throw new Error("Apple Team ID must contain 10 uppercase characters.");
    }
    await Bun.write(join(extractionDirectory, "sd-server"), binary);
    const path = join(extractionDirectory, "sd-server");
    await run([
      "/usr/bin/codesign",
      "--verify",
      "--strict",
      "--verbose=2",
      path,
    ]);
    const details = await run([
      "/usr/bin/codesign",
      "--display",
      "--verbose=4",
      path,
    ]);
    if (
      !details.includes(`TeamIdentifier=${teamId}`) ||
      !/^CodeDirectory.*\bruntime\b/m.test(details)
    ) {
      throw new Error(
        "sd-server signature does not match the expected team and hardened runtime.",
      );
    }
  }
}

const command = defineCommand({
  meta: {
    name: "sd-release",
    description: "Qualify native sd-server releases",
  },
  subCommands: {
    "qualify-archive": defineCommand({
      args: {
        target: {
          type: "enum",
          options: sdTargetSchema.options,
          required: true,
        },
        archive: { type: "string", required: true },
        "work-directory": { type: "string", required: true },
        "team-id": { type: "string" },
      },
      async run({ args }) {
        await qualifySdArchive(
          sdTargetSchema.parse(args.target),
          z.string().min(1).parse(args.archive),
          z.string().min(1).parse(args["work-directory"]),
          args["team-id"],
        );
      },
    }),
  },
});

if (import.meta.main) await runMain(command);
