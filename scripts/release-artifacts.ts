import { mkdir } from "node:fs/promises";
import { join, resolve } from "node:path";
import { z } from "zod";
import {
  computeSha256,
  safeFilenameSchema,
  sha256Schema,
} from "../src/utils/checksum";

export const releaseTargetSchema = z.enum([
  "macos-arm64",
  "macos-x64",
  "linux-x64",
  "linux-arm64",
]);
export type ReleaseTarget = z.infer<typeof releaseTargetSchema>;

type ReleaseTargetSpec = { bunTarget: string; architecture: "arm64" | "x64" };
const releaseTargets: Record<ReleaseTarget, ReleaseTargetSpec> = {
  "macos-arm64": { bunTarget: "bun-darwin-arm64", architecture: "arm64" },
  "macos-x64": { bunTarget: "bun-darwin-x64", architecture: "x64" },
  "linux-x64": { bunTarget: "bun-linux-x64", architecture: "x64" },
  "linux-arm64": { bunTarget: "bun-linux-arm64", architecture: "arm64" },
};

const artifactSchema = z
  .object({
    filename: safeFilenameSchema,
    size: z.number().int().positive(),
    sha256: sha256Schema,
  })
  .strict();

const packageArtifactSchema = artifactSchema
  .extend({
    format: z.enum(["zip", "tar.gz"]),
  })
  .strict();

const cliArtifactSchema = artifactSchema
  .extend({
    architecture: z.enum(["arm64", "x64"]),
  })
  .strict();

const cliFilename = (target: ReleaseTarget) => `local-base-${target}`;
export const releasePackageFilename = (target: ReleaseTarget) =>
  target.startsWith("macos-")
    ? `local-base-${target}.zip`
    : `local-base-${target}.tar.gz`;
const packageFormat = (target: ReleaseTarget) =>
  target.startsWith("macos-") ? "zip" : "tar.gz";

export const releaseArtifactManifestSchema = z
  .object({
    version: z.literal(2),
    target: releaseTargetSchema,
    package: packageArtifactSchema,
    cli: cliArtifactSchema,
  })
  .strict()
  .superRefine((manifest, ctx) => {
    const expectedTarget = releaseTargets[manifest.target];
    if (
      manifest.package.filename !== releasePackageFilename(manifest.target) ||
      manifest.package.format !== packageFormat(manifest.target)
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["package"],
        message: "must describe the target canonical package",
      });
    }
    if (
      manifest.cli.filename !== cliFilename(manifest.target) ||
      manifest.cli.architecture !== expectedTarget.architecture
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["cli"],
        message: "must describe the target CLI architecture",
      });
    }
  });
export type ReleaseArtifactManifest = z.infer<
  typeof releaseArtifactManifestSchema
>;

const qualificationReceiptSchema = z
  .object({
    version: z.literal(2),
    target: releaseTargetSchema,
    manifestSha256: sha256Schema,
  })
  .strict();

export const ARTIFACT_MANIFEST_FILENAME = "release-artifact-manifest.json";
export const QUALIFICATION_RECEIPT_FILENAME =
  "release-artifact-qualification.json";

export function releaseArtifactFilenames(target: ReleaseTarget): string[] {
  return [cliFilename(target)];
}

function artifactPath(directory: string, filename: string): string {
  return join(resolve(directory), safeFilenameSchema.parse(filename));
}

async function artifactEntry(directory: string, filename: string) {
  const path = artifactPath(directory, filename);
  const file = Bun.file(path);
  if (!(await file.exists()))
    throw new Error(`Missing release artifact ${filename} in ${directory}.`);
  const stat = await file.stat();
  if (!stat.isFile() || stat.size <= 0)
    throw new Error(`Release artifact ${path} is not a non-empty file.`);
  return artifactSchema.parse({
    filename,
    size: stat.size,
    sha256: await computeSha256(path),
  });
}

function matchesArtifact(
  actual: z.infer<typeof artifactSchema>,
  expected: z.infer<typeof artifactSchema>,
): boolean {
  return actual.size === expected.size && actual.sha256 === expected.sha256;
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

function targetManifest(
  manifest: ReleaseArtifactManifest,
  target: ReleaseTarget,
): ReleaseArtifactManifest {
  if (manifest.target !== target) {
    throw new Error(
      `Release artifact manifest is for ${manifest.target}, expected ${target}.`,
    );
  }
  return manifest;
}

export async function writeArtifactManifest(
  target: ReleaseTarget,
  directory: string,
) {
  const packageEntry = await artifactEntry(
    directory,
    releasePackageFilename(target),
  );
  const cliEntry = await artifactEntry(directory, cliFilename(target));
  const manifest = releaseArtifactManifestSchema.parse({
    version: 2,
    target,
    package: { ...packageEntry, format: packageFormat(target) },
    cli: { ...cliEntry, architecture: releaseTargets[target].architecture },
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
  const manifest = targetManifest(await readManifest(directory), target);
  const actual = await artifactEntry(directory, manifest.package.filename);
  if (!matchesArtifact(actual, manifest.package)) {
    throw new Error(
      `Release package ${manifest.package.filename} digest mismatch: expected ${manifest.package.sha256} (${manifest.package.size} bytes), received ${actual.sha256} (${actual.size} bytes).`,
    );
  }
  return manifest;
}

function readUInt32LE(bytes: Uint8Array, offset: number): number {
  return (
    (bytes[offset]! |
      (bytes[offset + 1]! << 8) |
      (bytes[offset + 2]! << 16) |
      (bytes[offset + 3]! << 24)) >>>
    0
  );
}

function isExpectedArchitecture(
  target: ReleaseTarget,
  bytes: Uint8Array,
): boolean {
  const architecture = releaseTargets[target].architecture;
  if (target.startsWith("linux-")) {
    return (
      bytes.length >= 20 &&
      bytes[0] === 0x7f &&
      bytes[1] === 0x45 &&
      bytes[2] === 0x4c &&
      bytes[3] === 0x46 &&
      bytes[18] === (architecture === "x64" ? 0x3e : 0xb7) &&
      bytes[19] === 0
    );
  }
  return (
    bytes.length >= 8 &&
    readUInt32LE(bytes, 0) === 0xfeedfacf &&
    readUInt32LE(bytes, 4) ===
      (architecture === "x64" ? 0x01000007 : 0x0100000c)
  );
}

export async function verifyReleasePackage(
  target: ReleaseTarget,
  directory: string,
  extractedDirectory: string,
) {
  const manifest = await verifyArtifactDirectory(target, directory);
  const actual = await artifactEntry(extractedDirectory, manifest.cli.filename);
  if (!matchesArtifact(actual, manifest.cli)) {
    throw new Error(
      `Packaged CLI ${manifest.cli.filename} digest mismatch: expected ${manifest.cli.sha256} (${manifest.cli.size} bytes), received ${actual.sha256} (${actual.size} bytes).`,
    );
  }
  const binary = Bun.file(
    artifactPath(extractedDirectory, manifest.cli.filename),
  );
  const header = new Uint8Array(await binary.slice(0, 32).arrayBuffer());
  if (!isExpectedArchitecture(target, header)) {
    throw new Error(
      `Packaged CLI ${manifest.cli.filename} does not match ${target} architecture.`,
    );
  }
  return manifest;
}

export async function buildReleaseArtifacts(
  target: ReleaseTarget,
  directory: string,
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
      { stdout: "inherit", stderr: "inherit" },
    );
    if ((await child.exited) !== 0)
      throw new Error(`bun build failed for ${filename}.`);
  };
  await build("src/cli.ts", cliFilename(target));
}

export async function qualifyArtifactDirectory(
  target: ReleaseTarget,
  directory: string,
  extractedDirectory: string,
): Promise<void> {
  await verifyReleasePackage(target, directory, extractedDirectory);
  const receipt = qualificationReceiptSchema.parse({
    version: 2,
    target,
    manifestSha256: await computeSha256(
      artifactPath(directory, ARTIFACT_MANIFEST_FILENAME),
    ),
  });
  await Bun.write(
    artifactPath(directory, QUALIFICATION_RECEIPT_FILENAME),
    `${JSON.stringify(receipt, null, 2)}\n`,
  );
}

async function verifyQualified(directory: string, target: ReleaseTarget) {
  const manifest = await verifyArtifactDirectory(target, directory);
  const receipt = qualificationReceiptSchema.parse(
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
  if (inputDirectories.length !== releaseTargetSchema.options.length) {
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
    targets.size !== releaseTargetSchema.options.length ||
    releaseTargetSchema.options.some((target) => !targets.has(target))
  ) {
    throw new Error(
      "Release staging requires exactly one qualified package for every supported target.",
    );
  }
  await mkdir(outputDirectory, { recursive: true });
  const checksums: string[] = [];
  for (const { directory, manifest } of qualified.sort((a, b) =>
    a.manifest.target.localeCompare(b.manifest.target),
  )) {
    const source = artifactPath(directory, manifest.package.filename);
    const destination = artifactPath(
      outputDirectory,
      manifest.package.filename,
    );
    await Bun.write(destination, Bun.file(source));
    const staged = await artifactEntry(
      outputDirectory,
      manifest.package.filename,
    );
    if (!matchesArtifact(staged, manifest.package)) {
      throw new Error(
        `Staged release package ${manifest.package.filename} does not match its qualified digest.`,
      );
    }
    checksums.push(`${staged.sha256}  ${manifest.package.filename}`);
  }
  await Bun.write(
    artifactPath(outputDirectory, "checksums.txt"),
    `${checksums.join("\n")}\n`,
  );
}

const commandSchema = z.enum([
  "build",
  "manifest",
  "verify",
  "verify-package",
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
  const command = commandSchema.parse(Bun.argv[2]);
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
  const target = releaseTargetSchema.parse(raw.target);
  const output = z.string().min(1).parse(raw.output);
  if (command === "build") await buildReleaseArtifacts(target, output);
  else if (command === "manifest") await writeArtifactManifest(target, output);
  else if (command === "verify") await verifyArtifactDirectory(target, output);
  else {
    const extracted = z.string().min(1).parse(raw.extracted);
    if (command === "verify-package")
      await verifyReleasePackage(target, output, extracted);
    else await qualifyArtifactDirectory(target, output, extracted);
  }
}

if (import.meta.main)
  main().catch((error) => {
    console.error(error);
    process.exit(1);
  });
