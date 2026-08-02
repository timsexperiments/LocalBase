import { mkdir } from "node:fs/promises";
import { join, resolve } from "node:path";
import { z } from "zod";
import {
  computeSha256,
  SafeFilenameSchema,
  Sha256Schema,
} from "../src/utils/checksum";

export const ReleaseTargetSchema = z.enum([
  "macos-arm64",
  "macos-x64",
  "linux-x64",
  "linux-arm64",
]);
export type ReleaseTarget = z.infer<typeof ReleaseTargetSchema>;

type ReleaseTargetSpec = { bunTarget: string };
const releaseTargets: Record<ReleaseTarget, ReleaseTargetSpec> = {
  "macos-arm64": { bunTarget: "bun-darwin-arm64" },
  "macos-x64": { bunTarget: "bun-darwin-x64" },
  "linux-x64": { bunTarget: "bun-linux-x64" },
  "linux-arm64": { bunTarget: "bun-linux-arm64" },
};

const ArtifactSchema = z
  .object({
    filename: SafeFilenameSchema,
    size: z.number().int().positive(),
    sha256: Sha256Schema,
  })
  .strict();

export const releaseArtifactManifestSchema = z
  .object({
    version: z.literal(1),
    target: ReleaseTargetSchema,
    artifacts: z.array(ArtifactSchema).min(1).max(2),
  })
  .strict()
  .superRefine((manifest, ctx) => {
    const expected = new Set(releaseArtifactFilenames(manifest.target));
    const actual = new Set(
      manifest.artifacts.map((artifact) => artifact.filename),
    );
    if (
      actual.size !== manifest.artifacts.length ||
      [...actual].some((filename) => !expected.has(filename)) ||
      !actual.has(cliFilename(manifest.target))
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["artifacts"],
        message: "must describe the target CLI and smoke runner",
      });
    }
  });
export type ReleaseArtifactManifest = z.infer<
  typeof releaseArtifactManifestSchema
>;

const QualificationReceiptSchema = z
  .object({
    version: z.literal(1),
    target: ReleaseTargetSchema,
    manifestSha256: Sha256Schema,
  })
  .strict();

export const ARTIFACT_MANIFEST_FILENAME = "release-artifact-manifest.json";
export const QUALIFICATION_RECEIPT_FILENAME =
  "release-artifact-qualification.json";

const cliFilename = (target: ReleaseTarget) => `local-base-${target}`;
const runnerFilename = (target: ReleaseTarget) =>
  `localbase-runtime-smoke-${target}`;

export function releaseArtifactFilenames(
  target: ReleaseTarget,
  includeSmokeRunner = true,
): string[] {
  return includeSmokeRunner
    ? [cliFilename(target), runnerFilename(target)]
    : [cliFilename(target)];
}

function artifactPath(directory: string, filename: string): string {
  return join(resolve(directory), SafeFilenameSchema.parse(filename));
}

async function artifactEntry(directory: string, filename: string) {
  const path = artifactPath(directory, filename);
  const file = Bun.file(path);
  if (!(await file.exists()))
    throw new Error(`Missing release artifact ${filename} in ${directory}.`);
  const stat = await file.stat();
  if (!stat.isFile() || stat.size <= 0)
    throw new Error(`Release artifact ${path} is not a non-empty file.`);
  return ArtifactSchema.parse({
    filename,
    size: stat.size,
    sha256: await computeSha256(path),
  });
}

async function readJson(path: string, description: string): Promise<unknown> {
  try {
    return JSON.parse(await Bun.file(path).text());
  } catch (error) {
    throw new Error(`Invalid ${description} at ${path}.`, { cause: error });
  }
}

async function readManifest(
  directory: string,
): Promise<ReleaseArtifactManifest> {
  return releaseArtifactManifestSchema.parse(
    await readJson(
      artifactPath(directory, ARTIFACT_MANIFEST_FILENAME),
      "release artifact manifest",
    ),
  );
}

export async function writeArtifactManifest(
  target: ReleaseTarget,
  directory: string,
) {
  const files = [];
  for (const filename of releaseArtifactFilenames(target)) {
    if (await Bun.file(artifactPath(directory, filename)).exists())
      files.push(filename);
  }
  const manifest = releaseArtifactManifestSchema.parse({
    version: 1,
    target,
    artifacts: await Promise.all(
      files.map((filename) => artifactEntry(directory, filename)),
    ),
  });
  await Bun.write(
    artifactPath(directory, ARTIFACT_MANIFEST_FILENAME),
    `${JSON.stringify(manifest, null, 2)}\n`,
  );
  return manifest;
}

export async function verifyArtifactDirectory(
  target: ReleaseTarget,
  directory: string,
) {
  const manifest = await readManifest(directory);
  if (manifest.target !== target) {
    throw new Error(
      `Release artifact manifest at ${directory} is for ${manifest.target}, expected ${target}.`,
    );
  }
  for (const expected of manifest.artifacts) {
    const actual = await artifactEntry(directory, expected.filename);
    if (actual.size !== expected.size || actual.sha256 !== expected.sha256) {
      throw new Error(
        `Release artifact ${expected.filename} digest mismatch: expected ${expected.sha256} (${expected.size} bytes), received ${actual.sha256} (${actual.size} bytes).`,
      );
    }
  }
  return manifest;
}

export async function buildReleaseArtifacts(
  target: ReleaseTarget,
  directory: string,
  options: { includeSmokeRunner?: boolean } = {},
): Promise<void> {
  await mkdir(directory, { recursive: true });
  const targetFlag = `--target=${releaseTargets[target].bunTarget}`;
  const common = [
    "--compile",
    "--no-compile-autoload-dotenv",
    "--no-compile-autoload-bunfig",
    targetFlag,
  ];
  const build = async (entrypoint: string, filename: string) => {
    const child = Bun.spawn(
      [
        process.execPath,
        "build",
        entrypoint,
        ...common,
        "--asset-naming=[dir]/[name].[ext]",
        `--outfile=${artifactPath(directory, filename)}`,
      ],
      {
        stdout: "inherit",
        stderr: "inherit",
      },
    );
    if ((await child.exited) !== 0)
      throw new Error(`bun build failed for ${filename}.`);
  };
  await build("src/cli.ts", cliFilename(target));
  if (options.includeSmokeRunner ?? true)
    await build("scripts/runtime-smoke.ts", runnerFilename(target));
}

export async function qualifyArtifactDirectory(
  target: ReleaseTarget,
  directory: string,
): Promise<void> {
  const manifest = await verifyArtifactDirectory(target, directory);
  const receipt = QualificationReceiptSchema.parse({
    version: 1,
    target,
    manifestSha256: await computeSha256(
      artifactPath(directory, ARTIFACT_MANIFEST_FILENAME),
    ),
  });
  await Bun.write(
    artifactPath(directory, QUALIFICATION_RECEIPT_FILENAME),
    `${JSON.stringify(receipt, null, 2)}\n`,
  );
  if (manifest.target !== target)
    throw new Error(`Qualification target mismatch for ${target}.`);
}

async function verifyQualified(directory: string, target: ReleaseTarget) {
  const manifest = await verifyArtifactDirectory(target, directory);
  const receipt = QualificationReceiptSchema.parse(
    await readJson(
      artifactPath(directory, QUALIFICATION_RECEIPT_FILENAME),
      "release artifact qualification receipt",
    ),
  );
  const digest = await computeSha256(
    artifactPath(directory, ARTIFACT_MANIFEST_FILENAME),
  );
  if (receipt.target !== target || receipt.manifestSha256 !== digest) {
    throw new Error(
      `Release artifact manifest for ${target} changed after qualification.`,
    );
  }
  return manifest;
}

export async function stageReleaseArtifacts(
  inputDirectories: string[],
  outputDirectory: string,
): Promise<void> {
  if (inputDirectories.length !== ReleaseTargetSchema.options.length) {
    throw new Error(
      "Release staging requires exactly one artifact directory for every supported target.",
    );
  }
  const qualified = await Promise.all(
    inputDirectories.map(async (directory) => {
      const manifest = await readManifest(directory);
      return {
        directory,
        manifest: await verifyQualified(directory, manifest.target),
      };
    }),
  );
  const targets = new Set(qualified.map(({ manifest }) => manifest.target));
  if (
    targets.size !== ReleaseTargetSchema.options.length ||
    ReleaseTargetSchema.options.some((target) => !targets.has(target))
  ) {
    throw new Error(
      "Release staging requires exactly one qualified artifact for every supported target.",
    );
  }
  await mkdir(outputDirectory, { recursive: true });
  const checksums: string[] = [];
  for (const { directory, manifest } of qualified.sort((a, b) =>
    a.manifest.target.localeCompare(b.manifest.target),
  )) {
    const filename = cliFilename(manifest.target);
    const source = artifactPath(directory, filename);
    const destination = artifactPath(outputDirectory, filename);
    await Bun.write(destination, Bun.file(source));
    const staged = await artifactEntry(outputDirectory, filename);
    const expected = manifest.artifacts.find(
      (artifact) => artifact.filename === filename,
    );
    if (
      !expected ||
      staged.size !== expected.size ||
      staged.sha256 !== expected.sha256
    ) {
      throw new Error(
        `Staged release artifact ${filename} does not match its qualified digest.`,
      );
    }
    checksums.push(`${staged.sha256}  ${filename}`);
  }
  await Bun.write(
    artifactPath(outputDirectory, "checksums.txt"),
    `${checksums.join("\n")}\n`,
  );
}

const CommandSchema = z.enum([
  "build",
  "manifest",
  "verify",
  "qualify",
  "stage",
]);
function options(args: string[]) {
  const result: Record<string, string | string[]> = {};
  for (let i = 0; i < args.length; i += 2) {
    const key = args[i];
    const value = args[i + 1];
    if (!key?.startsWith("--") || !value || value.startsWith("--"))
      throw new Error(`Expected a value after ${key ?? "the command"}.`);
    result[key.slice(2)] = result[key.slice(2)]
      ? [result[key.slice(2)]].flat().concat(value)
      : value;
  }
  return result;
}

async function main() {
  const command = CommandSchema.parse(Bun.argv[2]);
  const raw = options(Bun.argv.slice(3));
  if (command === "stage") {
    const input = Array.isArray(raw.input)
      ? raw.input
      : raw.input
        ? [raw.input]
        : [];
    await stageReleaseArtifacts(input, z.string().min(1).parse(raw.output));
    return;
  }
  const target = ReleaseTargetSchema.parse(raw.target);
  const output = z.string().min(1).parse(raw.output);
  if (command === "build") await buildReleaseArtifacts(target, output);
  else if (command === "manifest") await writeArtifactManifest(target, output);
  else if (command === "verify") await verifyArtifactDirectory(target, output);
  else await qualifyArtifactDirectory(target, output);
}

if (import.meta.main)
  main().catch((error) => {
    console.error(error);
    process.exit(1);
  });
