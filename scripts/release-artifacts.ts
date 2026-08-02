import { chmodSync, mkdirSync } from "node:fs";
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

const QualificationExecutionSchema = z.enum(["native", "rosetta", "qemu"]);
type QualificationExecution = z.infer<typeof QualificationExecutionSchema>;

type ReleaseTargetSpec = {
  platform: "darwin" | "linux";
  architecture: "arm64" | "x64";
  bunTarget: string;
};

const releaseTargets: Record<ReleaseTarget, ReleaseTargetSpec> = {
  "macos-arm64": {
    platform: "darwin",
    architecture: "arm64",
    bunTarget: "bun-darwin-arm64",
  },
  "macos-x64": {
    platform: "darwin",
    architecture: "x64",
    bunTarget: "bun-darwin-x64",
  },
  "linux-x64": {
    platform: "linux",
    architecture: "x64",
    bunTarget: "bun-linux-x64",
  },
  "linux-arm64": {
    platform: "linux",
    architecture: "arm64",
    bunTarget: "bun-linux-arm64",
  },
};

const ArtifactEntrySchema = z
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
    artifacts: z.array(ArtifactEntrySchema).min(1).max(2),
  })
  .strict()
  .superRefine((manifest, ctx) => {
    const expected = new Set(releaseArtifactFilenames(manifest.target));
    const actual = new Set(manifest.artifacts.map(({ filename }) => filename));
    if (
      actual.size !== manifest.artifacts.length ||
      [...actual].some((filename) => !expected.has(filename)) ||
      !actual.has(cliFilename(manifest.target))
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["artifacts"],
        message: "must describe the target CLI and qualification runner",
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

const projectRoot = resolve(import.meta.dir, "..");
const COMMAND_TIMEOUT_MS = 180_000;

function cliFilename(target: ReleaseTarget): string {
  return `local-base-${target}`;
}

function runnerFilename(target: ReleaseTarget): string {
  return `localbase-runtime-smoke-${target}`;
}

export function releaseArtifactFilenames(
  target: ReleaseTarget,
  includeSmokeRunner = true,
): string[] {
  return includeSmokeRunner
    ? [cliFilename(target), runnerFilename(target)]
    : [cliFilename(target)];
}

function pathFor(directory: string, filename: string): string {
  return join(resolve(directory), SafeFilenameSchema.parse(filename));
}

async function artifactEntry(
  directory: string,
  filename: string,
): Promise<z.infer<typeof ArtifactEntrySchema>> {
  const path = pathFor(directory, filename);
  const file = Bun.file(path);
  if (!(await file.exists())) {
    throw new Error(`Missing release artifact ${filename} in ${directory}.`);
  }
  const stat = await file.stat();
  if (!stat.isFile() || stat.size <= 0) {
    throw new Error(`Release artifact ${path} is not a non-empty file.`);
  }
  return ArtifactEntrySchema.parse({
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
      pathFor(directory, ARTIFACT_MANIFEST_FILENAME),
      "release artifact manifest",
    ),
  );
}

export async function writeArtifactManifest(
  target: ReleaseTarget,
  directory: string,
): Promise<ReleaseArtifactManifest> {
  const filenames = [
    cliFilename(target),
    ...((await Bun.file(pathFor(directory, runnerFilename(target))).exists())
      ? [runnerFilename(target)]
      : []),
  ];
  const manifest = releaseArtifactManifestSchema.parse({
    version: 1,
    target,
    artifacts: await Promise.all(
      filenames.map((filename) => artifactEntry(directory, filename)),
    ),
  });
  await Bun.write(
    pathFor(directory, ARTIFACT_MANIFEST_FILENAME),
    `${JSON.stringify(manifest, null, 2)}\n`,
  );
  return manifest;
}

export async function verifyArtifactDirectory(
  target: ReleaseTarget,
  directory: string,
): Promise<ReleaseArtifactManifest> {
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

async function runProcess(
  args: string[],
  options: { cwd?: string; env?: Record<string, string | undefined> } = {},
): Promise<void> {
  const child = Bun.spawn(args, {
    cwd: options.cwd ?? projectRoot,
    env: options.env ?? process.env,
    stdin: "ignore",
    stdout: "inherit",
    stderr: "inherit",
  });
  const result = await Promise.race([
    child.exited.then((exitCode) => ({ exitCode, timedOut: false })),
    Bun.sleep(COMMAND_TIMEOUT_MS).then(() => ({
      exitCode: -1,
      timedOut: true,
    })),
  ]);
  if (result.timedOut) {
    if (child.exitCode === null) child.kill(15);
    await Promise.race([child.exited, Bun.sleep(1_000)]);
    if (child.exitCode === null) child.kill(9);
    throw new Error(`${args[0]} exceeded ${COMMAND_TIMEOUT_MS}ms.`);
  }
  const exitCode = result.exitCode;
  if (exitCode !== 0) {
    throw new Error(`${args[0]} exited with code ${exitCode}.`);
  }
}

export async function buildReleaseArtifacts(
  target: ReleaseTarget,
  directory: string,
  options: { includeSmokeRunner?: boolean } = {},
): Promise<void> {
  mkdirSync(directory, { recursive: true });
  const spec = releaseTargets[target];
  const common = [
    "--compile",
    "--no-compile-autoload-dotenv",
    "--no-compile-autoload-bunfig",
    `--target=${spec.bunTarget}`,
  ];
  await runProcess([
    process.execPath,
    "build",
    "src/cli.ts",
    ...common,
    "--asset-naming=[dir]/[name].[ext]",
    `--outfile=${pathFor(directory, cliFilename(target))}`,
  ]);
  if (options.includeSmokeRunner ?? true) {
    await runProcess([
      process.execPath,
      "build",
      "scripts/runtime-smoke.ts",
      ...common,
      `--outfile=${pathFor(directory, runnerFilename(target))}`,
    ]);
  }
}

function qualificationEnvironment(
  target: ReleaseTarget,
  directory: string,
): Record<string, string | undefined> {
  return {
    ...process.env,
    LOCALBASE_SMOKE_CLI: pathFor(directory, cliFilename(target)),
    LOCALBASE_SMOKE_TARGET: target,
  };
}

async function executeQualification(
  target: ReleaseTarget,
  directory: string,
  execution: QualificationExecution,
): Promise<void> {
  const runner = pathFor(directory, runnerFilename(target));
  const env = qualificationEnvironment(target, directory);
  chmodSync(pathFor(directory, cliFilename(target)), 0o755);
  chmodSync(pathFor(directory, runnerFilename(target)), 0o755);
  if (execution === "native") {
    const expected = releaseTargets[target];
    if (
      process.platform !== expected.platform ||
      process.arch !== expected.architecture
    ) {
      throw new Error(
        `Native qualification for ${target} requires ${expected.platform}/${expected.architecture}, received ${process.platform}/${process.arch}.`,
      );
    }
    await runProcess([runner], {
      cwd: resolve(directory),
      env: { ...env, LOCALBASE_SMOKE_EXECUTION: execution },
    });
    return;
  }

  if (execution === "rosetta") {
    if (target !== "macos-x64" || process.platform !== "darwin") {
      throw new Error("Rosetta qualification is only valid for macos-x64.");
    }
    await runProcess(["/usr/bin/arch", "-x86_64", runner], {
      cwd: resolve(directory),
      env: { ...env, LOCALBASE_SMOKE_EXECUTION: execution },
    });
    return;
  }

  if (target !== "linux-arm64") {
    throw new Error("QEMU qualification is only valid for linux-arm64.");
  }
  const image = `localbase-runtime-smoke:${crypto.randomUUID()}`;
  try {
    await runProcess([
      "docker",
      "build",
      "--platform",
      "linux/arm64",
      "--file",
      join(projectRoot, "Dockerfile.runtime-smoke"),
      "--build-arg",
      `RELEASE_TARGET=${target}`,
      "--build-arg",
      "ARTIFACT_DIR=.",
      "--tag",
      image,
      resolve(directory),
    ]);
    await runProcess([
      "docker",
      "run",
      "--rm",
      "--platform",
      "linux/arm64",
      "--env",
      `LOCALBASE_SMOKE_TARGET=${target}`,
      "--env",
      "LOCALBASE_SMOKE_EXECUTION=qemu",
      image,
    ]);
  } finally {
    await runProcess(["docker", "image", "rm", "--force", image]).catch(
      () => undefined,
    );
  }
}

export type ArtifactSmokeExecutor = (
  target: ReleaseTarget,
  directory: string,
) => Promise<void>;

export async function qualifyArtifactDirectory(
  target: ReleaseTarget,
  directory: string,
  execution: QualificationExecution,
  smokeExecutor?: ArtifactSmokeExecutor,
): Promise<void> {
  await verifyArtifactDirectory(target, directory);
  if (smokeExecutor) await smokeExecutor(target, directory);
  else await executeQualification(target, directory, execution);
  await verifyArtifactDirectory(target, directory);

  const manifestSha256 = await computeSha256(
    pathFor(directory, ARTIFACT_MANIFEST_FILENAME),
  );
  const receipt = QualificationReceiptSchema.parse({
    version: 1,
    target,
    manifestSha256,
  });
  await Bun.write(
    pathFor(directory, QUALIFICATION_RECEIPT_FILENAME),
    `${JSON.stringify(receipt, null, 2)}\n`,
  );
}

async function verifyQualifiedArtifactDirectory(
  target: ReleaseTarget,
  directory: string,
): Promise<ReleaseArtifactManifest> {
  const manifest = await verifyArtifactDirectory(target, directory);
  const receipt = QualificationReceiptSchema.parse(
    await readJson(
      pathFor(directory, QUALIFICATION_RECEIPT_FILENAME),
      "release artifact qualification receipt",
    ),
  );
  if (receipt.target !== target) {
    throw new Error(
      `Release artifact qualification at ${directory} is for ${receipt.target}, expected ${target}.`,
    );
  }
  const manifestSha256 = await computeSha256(
    pathFor(directory, ARTIFACT_MANIFEST_FILENAME),
  );
  if (receipt.manifestSha256 !== manifestSha256) {
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
  const described = await Promise.all(
    inputDirectories.map(async (directory) => ({
      directory,
      manifest: await readManifest(directory),
    })),
  );
  if (described.length !== ReleaseTargetSchema.options.length) {
    throw new Error(
      "Release staging requires exactly one artifact directory for every supported target.",
    );
  }
  const targets = new Set(described.map(({ manifest }) => manifest.target));
  if (
    targets.size !== ReleaseTargetSchema.options.length ||
    ReleaseTargetSchema.options.some((target) => !targets.has(target))
  ) {
    throw new Error(
      "Release staging requires exactly one qualified artifact for every supported target.",
    );
  }

  const qualified = await Promise.all(
    described.map(async ({ directory, manifest }) => ({
      directory,
      manifest: await verifyQualifiedArtifactDirectory(
        manifest.target,
        directory,
      ),
    })),
  );

  mkdirSync(outputDirectory, { recursive: true });
  const checksums: string[] = [];
  for (const { directory, manifest } of qualified.sort((left, right) =>
    left.manifest.target.localeCompare(right.manifest.target),
  )) {
    const filename = cliFilename(manifest.target);
    const source = pathFor(directory, filename);
    const destination = pathFor(outputDirectory, filename);
    await Bun.write(destination, Bun.file(source));
    chmodSync(destination, 0o755);

    const qualifiedEntry = manifest.artifacts.find(
      (artifact) => artifact.filename === filename,
    );
    if (!qualifiedEntry) {
      throw new Error(`Release artifact manifest is missing ${filename}.`);
    }
    const stagedEntry = await artifactEntry(outputDirectory, filename);
    if (
      stagedEntry.size !== qualifiedEntry.size ||
      stagedEntry.sha256 !== qualifiedEntry.sha256
    ) {
      throw new Error(
        `Staged release artifact ${filename} does not match its qualified digest.`,
      );
    }
    checksums.push(`${stagedEntry.sha256}  ${filename}`);
  }
  await Bun.write(
    pathFor(outputDirectory, "checksums.txt"),
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
const TargetCommandOptionsSchema = z
  .object({ target: ReleaseTargetSchema, output: z.string().min(1) })
  .strict();
const QualifyCommandOptionsSchema = TargetCommandOptionsSchema.extend({
  execution: QualificationExecutionSchema,
}).strict();
const StageCommandOptionsSchema = z
  .object({
    output: z.string().min(1),
    input: z.array(z.string().min(1)).min(1),
  })
  .strict();

function parseOptions(args: string[]): Record<string, string | string[]> {
  const options: Record<string, string | string[]> = {};
  for (let index = 0; index < args.length; index += 1) {
    const flag = args[index];
    const value = args[index + 1];
    if (
      !flag?.startsWith("--") ||
      value === undefined ||
      value.startsWith("--")
    ) {
      throw new Error(`Expected a value after ${flag ?? "the command"}.`);
    }
    const name = flag.slice(2);
    const previous = options[name];
    options[name] =
      previous === undefined
        ? value
        : Array.isArray(previous)
          ? [...previous, value]
          : [previous, value];
    index += 1;
  }
  return options;
}

async function main(): Promise<void> {
  const [rawCommand, ...rawOptions] = Bun.argv.slice(2);
  const command = CommandSchema.parse(rawCommand);
  const options = parseOptions(rawOptions);
  if (command === "stage") {
    const input = options.input;
    const parsed = StageCommandOptionsSchema.parse({
      output: options.output,
      input: Array.isArray(input) ? input : input ? [input] : [],
    });
    await stageReleaseArtifacts(parsed.input, parsed.output);
    return;
  }
  if (command === "qualify") {
    const parsed = QualifyCommandOptionsSchema.parse(options);
    await qualifyArtifactDirectory(
      parsed.target,
      parsed.output,
      parsed.execution,
    );
    return;
  }
  const parsed = TargetCommandOptionsSchema.parse(options);
  if (command === "build") {
    await buildReleaseArtifacts(parsed.target, parsed.output);
  } else if (command === "manifest") {
    await writeArtifactManifest(parsed.target, parsed.output);
  } else {
    await verifyArtifactDirectory(parsed.target, parsed.output);
  }
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(error);
    process.exit(1);
  });
}
