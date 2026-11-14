import { runMain } from "citty";
import { defineCommand } from "citty";
import { basename, join } from "node:path";
import { z } from "zod";
import {
  readSdArchiveEntries,
  type ArchiveEntry,
} from "./sd-release/archive-reader";
import {
  filePathSchema,
  repositorySchema,
  ttsReleaseTagSchema,
  ttsSourceProvenanceSchema,
  validateTtsReleaseTag,
} from "./tts-release/contracts";
import {
  updateTtsManifestFile,
  verifyTtsManifestFile,
} from "./tts-release/manifest";
import {
  assertTtsReleaseAvailable,
  verifyPublishedTtsRelease,
} from "./tts-release/published-release";

export const ttsTargetSchema = z.enum(["linux-x64", "macos-arm64"]);
export type TtsTarget = z.infer<typeof ttsTargetSchema>;

const licenseFiles = [
  "LICENSE.cpp-httplib.txt",
  "LICENSE.llama.cpp.txt",
  "LICENSE.nlohmann-json.txt",
] as const;

export function validateTtsArchiveEntries(
  target: TtsTarget,
  entries: ArchiveEntry[],
): Uint8Array {
  const expected = [
    "tts-server",
    "SOURCE.tts-server.json",
    ...licenseFiles,
  ].sort();
  const names = entries.map((entry) => entry.name).sort();
  if (JSON.stringify(names) !== JSON.stringify(expected)) {
    throw new Error(
      `${target} archive must contain only tts-server, source provenance, and the three required license files.`,
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
    (entry) => entry.name === "SOURCE.tts-server.json",
  )!;
  try {
    ttsSourceProvenanceSchema.parse(
      JSON.parse(new TextDecoder().decode(provenance.bytes)),
    );
  } catch (error) {
    throw new Error(`${target} archive has invalid source provenance.`, {
      cause: error,
    });
  }
  return entries.find((entry) => entry.name === "tts-server")!.bytes;
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

export function validateTtsBinaryArchitecture(
  target: TtsTarget,
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
    throw new Error(`tts-server does not match ${target} architecture.`);
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

export async function qualifyTtsArchive(
  target: TtsTarget,
  archive: string,
  extractionDirectory: string,
  teamId?: string,
) {
  const entries = await readSdArchiveEntries(
    target,
    new Uint8Array(await Bun.file(archive).arrayBuffer()),
  );
  const binary = validateTtsArchiveEntries(target, entries);
  validateTtsBinaryArchitecture(target, binary);

  if (target === "macos-arm64" && teamId) {
    if (!/^[A-Z0-9]{10}$/.test(teamId)) {
      throw new Error("Apple Team ID must contain 10 uppercase characters.");
    }
    const path = join(extractionDirectory, "tts-server");
    await Bun.write(path, binary);
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
        "tts-server signature does not match the expected team and hardened runtime.",
      );
    }
  }
}

const command = defineCommand({
  meta: {
    name: "tts-release",
    description: "Qualify native tts-server releases",
  },
  subCommands: {
    "validate-tag": defineCommand({
      args: {
        tag: { type: "string", required: true },
        "github-output": { type: "string" },
      },
      async run({ args }) {
        const tag = validateTtsReleaseTag(args.tag);
        if (args["github-output"]) {
          const output = filePathSchema.parse(args["github-output"]);
          await Bun.write(output, `tag=${tag}\nbranch=tim/${tag}-manifest\n`);
        }
      },
    }),
    "assert-release-available": defineCommand({
      args: {
        repository: { type: "string", required: true },
        tag: { type: "string", required: true },
      },
      async run({ args }) {
        await assertTtsReleaseAvailable(
          repositorySchema.parse(args.repository),
          ttsReleaseTagSchema.parse(args.tag),
        );
      },
    }),
    "qualify-archive": defineCommand({
      args: {
        target: {
          type: "enum",
          options: ttsTargetSchema.options,
          required: true,
        },
        archive: { type: "string", required: true },
        "work-directory": { type: "string", required: true },
        "team-id": { type: "string" },
      },
      async run({ args }) {
        await qualifyTtsArchive(
          ttsTargetSchema.parse(args.target),
          z.string().min(1).parse(args.archive),
          z.string().min(1).parse(args["work-directory"]),
          args["team-id"],
        );
      },
    }),
    "verify-published-release": defineCommand({
      args: {
        repository: { type: "string", required: true },
        tag: { type: "string", required: true },
        output: { type: "string", required: true },
      },
      async run({ args }) {
        await verifyPublishedTtsRelease(args.repository, args.tag, args.output);
      },
    }),
    "update-manifest": defineCommand({
      args: {
        manifest: { type: "string", required: true },
        receipt: { type: "string", required: true },
      },
      async run({ args }) {
        await updateTtsManifestFile(args.manifest, args.receipt);
      },
    }),
    "verify-manifest": defineCommand({
      args: {
        manifest: { type: "string", required: true },
        receipt: { type: "string", required: true },
      },
      async run({ args }) {
        await verifyTtsManifestFile(args.manifest, args.receipt);
      },
    }),
  },
});

if (import.meta.main) await runMain(command);
