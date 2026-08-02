import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import {
  ARTIFACT_MANIFEST_FILENAME,
  qualifyArtifactDirectory,
  releaseArtifactFilenames,
  ReleaseTargetSchema,
  stageReleaseArtifacts,
  verifyArtifactDirectory,
  writeArtifactManifest,
  type ReleaseTarget,
} from "./release-artifacts";

const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

function temporaryDirectory(): string {
  const directory = mkdtempSync("/tmp/localbase-release-artifacts-");
  temporaryDirectories.push(directory);
  return directory;
}

async function createArtifacts(
  target: ReleaseTarget,
  parent = temporaryDirectory(),
): Promise<string> {
  const directory = join(parent, target);
  mkdirSync(directory, { recursive: true });
  const [cli, runner] = releaseArtifactFilenames(target);
  await Bun.write(join(directory, cli!), `cli:${target}`);
  await Bun.write(join(directory, runner!), `runner:${target}`);
  await writeArtifactManifest(target, directory);
  return directory;
}

test("rejects release artifacts changed after manifest generation", async () => {
  const target = "linux-x64";
  const directory = await createArtifacts(target);
  const [cli] = releaseArtifactFilenames(target);

  await Bun.write(join(directory, cli!), "tampered");

  await expect(verifyArtifactDirectory(target, directory)).rejects.toThrow(
    "digest mismatch",
  );
});

test("rejects missing artifacts and manifests for another target", async () => {
  const directory = await createArtifacts("linux-x64");
  const [cli] = releaseArtifactFilenames("linux-x64");
  rmSync(join(directory, cli!), { force: true });

  await expect(verifyArtifactDirectory("linux-x64", directory)).rejects.toThrow(
    "Missing release artifact",
  );

  const otherDirectory = await createArtifacts("linux-arm64");
  await expect(
    verifyArtifactDirectory("macos-arm64", otherDirectory),
  ).rejects.toThrow("is for linux-arm64, expected macos-arm64");
});

test("stages only a complete set of unchanged qualified artifacts", async () => {
  const parent = temporaryDirectory();
  const directories = await Promise.all(
    ReleaseTargetSchema.options.map((target) =>
      createArtifacts(target, parent),
    ),
  );
  const smoke = async () => {};
  await expect(
    verifyArtifactDirectory("linux-x64", directories[2]!),
  ).resolves.toMatchObject({ target: "linux-x64" });

  await Promise.all(
    ReleaseTargetSchema.options
      .slice(0, -1)
      .map((target, index) =>
        qualifyArtifactDirectory(target, directories[index]!, "native", smoke),
      ),
  );

  await expect(
    stageReleaseArtifacts(
      [...directories, directories[0]!],
      join(parent, "duplicate"),
    ),
  ).rejects.toThrow("exactly one artifact directory");

  await expect(
    stageReleaseArtifacts(directories, join(parent, "unqualified")),
  ).rejects.toThrow("qualification receipt");

  const last = ReleaseTargetSchema.options.at(-1)!;
  await qualifyArtifactDirectory(last, directories.at(-1)!, "native", smoke);
  const staged = join(parent, "staged");
  await stageReleaseArtifacts(directories, staged);

  const checksums = await Bun.file(join(staged, "checksums.txt")).text();
  for (const target of ReleaseTargetSchema.options) {
    const [cli] = releaseArtifactFilenames(target);
    expect(await Bun.file(join(staged, cli!)).text()).toBe(`cli:${target}`);
    expect(checksums).toContain(`  ${cli}`);
  }

  const manifest = join(directories[0]!, ARTIFACT_MANIFEST_FILENAME);
  await Bun.write(manifest, `${await Bun.file(manifest).text()}\n`);
  await expect(
    stageReleaseArtifacts(directories, join(parent, "changed")),
  ).rejects.toThrow("changed after qualification");
});
