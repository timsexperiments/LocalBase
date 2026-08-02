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

const directories: string[] = [];
afterEach(() =>
  directories
    .splice(0)
    .forEach((directory) =>
      rmSync(directory, { recursive: true, force: true }),
    ),
);

function temp() {
  const directory = mkdtempSync("/tmp/localbase-release-artifacts-");
  directories.push(directory);
  return directory;
}

async function artifacts(target: ReleaseTarget, parent = temp()) {
  const directory = join(parent, target);
  mkdirSync(directory, { recursive: true });
  const [cli, runner] = releaseArtifactFilenames(target);
  await Bun.write(join(directory, cli!), `cli:${target}`);
  await Bun.write(join(directory, runner!), `runner:${target}`);
  await writeArtifactManifest(target, directory);
  return directory;
}

test("rejects a changed artifact", async () => {
  const directory = await artifacts("linux-x64");
  const [cli] = releaseArtifactFilenames("linux-x64");
  await Bun.write(join(directory, cli!), "tampered");
  await expect(verifyArtifactDirectory("linux-x64", directory)).rejects.toThrow(
    "digest mismatch",
  );
});

test("invalidates a qualification receipt when the manifest changes", async () => {
  const parent = temp();
  const inputs = await Promise.all(
    ReleaseTargetSchema.options.map((target) => artifacts(target, parent)),
  );
  await Promise.all(
    inputs.map((directory, index) =>
      qualifyArtifactDirectory(ReleaseTargetSchema.options[index]!, directory),
    ),
  );
  const manifest = join(inputs[0]!, ARTIFACT_MANIFEST_FILENAME);
  await Bun.write(manifest, `${await Bun.file(manifest).text()}\n`);
  await expect(
    stageReleaseArtifacts(inputs, join(temp(), "stage")),
  ).rejects.toThrow("changed after qualification");
});

test("stages one unchanged CLI for every target and writes checksums", async () => {
  const parent = temp();
  const inputs = await Promise.all(
    ReleaseTargetSchema.options.map((target) => artifacts(target, parent)),
  );
  await Promise.all(
    inputs.map((directory, index) =>
      qualifyArtifactDirectory(ReleaseTargetSchema.options[index]!, directory),
    ),
  );
  const output = join(parent, "stage");
  await stageReleaseArtifacts(inputs, output);
  const checksums = await Bun.file(join(output, "checksums.txt")).text();
  for (const target of ReleaseTargetSchema.options) {
    const [cli] = releaseArtifactFilenames(target);
    expect(await Bun.file(join(output, cli!)).text()).toBe(`cli:${target}`);
    expect(checksums).toContain(`  ${cli}`);
  }
});
