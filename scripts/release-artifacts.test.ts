import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import {
  qualifyArtifactDirectory,
  releaseArtifactFilenames,
  releasePackageFilename,
  releaseTargetSchema,
  stageReleaseArtifacts,
  verifyArtifactDirectory,
  verifyReleasePackage,
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

function binaryHeader(target: ReleaseTarget) {
  if (target.startsWith("linux-")) {
    const header = new Uint8Array(32);
    header.set([0x7f, 0x45, 0x4c, 0x46]);
    header[18] = target.endsWith("x64") ? 0x3e : 0xb7;
    return header;
  }
  const header = new Uint8Array(32);
  header.set([0xcf, 0xfa, 0xed, 0xfe]);
  header[4] = target.endsWith("x64") ? 0x07 : 0x0c;
  header[7] = 0x01;
  return header;
}

async function artifacts(target: ReleaseTarget, parent = temp()) {
  const directory = join(parent, target);
  const extracted = join(parent, `${target}-extracted`);
  mkdirSync(directory, { recursive: true });
  mkdirSync(extracted, { recursive: true });
  const [cli] = releaseArtifactFilenames(target);
  const binary = binaryHeader(target);
  await Bun.write(join(directory, cli!), binary);
  await Bun.write(join(extracted, cli!), binary);
  await Bun.write(join(directory, releasePackageFilename(target)), "package");
  await writeArtifactManifest(target, directory);
  return { directory, extracted };
}

test("rejects a changed canonical package", async () => {
  const { directory } = await artifacts("linux-x64");
  await Bun.write(
    join(directory, releasePackageFilename("linux-x64")),
    "tampered",
  );
  await expect(verifyArtifactDirectory("linux-x64", directory)).rejects.toThrow(
    "package local-base-linux-x64.tar.gz digest mismatch",
  );
});

test("rejects a package whose extracted CLI differs from the manifest", async () => {
  const { directory, extracted } = await artifacts("macos-arm64");
  await Bun.write(join(extracted, "local-base-macos-arm64"), "tampered");
  await expect(
    verifyReleasePackage("macos-arm64", directory, extracted),
  ).rejects.toThrow("Packaged CLI local-base-macos-arm64 digest mismatch");
});

test("stages unchanged qualified canonical packages", async () => {
  const parent = temp();
  const inputs = await Promise.all(
    releaseTargetSchema.options.map((target) => artifacts(target, parent)),
  );
  await Promise.all(
    inputs.map(({ directory, extracted }, index) =>
      qualifyArtifactDirectory(
        releaseTargetSchema.options[index]!,
        directory,
        extracted,
      ),
    ),
  );
  const output = join(parent, "stage");
  await stageReleaseArtifacts(
    inputs.map(({ directory }) => directory),
    output,
  );
  const checksums = await Bun.file(join(output, "checksums.txt")).text();
  for (const target of releaseTargetSchema.options) {
    const filename = releasePackageFilename(target);
    expect(await Bun.file(join(output, filename)).text()).toBe("package");
    expect(checksums).toContain(`  ${filename}`);
  }
});
