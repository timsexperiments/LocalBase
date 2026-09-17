import { afterEach, expect, test } from "bun:test";
import {
  chmod,
  mkdir,
  mkdtemp,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { join } from "node:path";
import { createHash } from "node:crypto";
import { gzipSync } from "fflate";
import { pack } from "tar-stream";
import { verifyRegistryPackage } from "./kokoro-release";
import { validateSpeechWav } from "../src/domains/runtime/speech-supervisor";
import { parseArguments } from "../runtimes/kokoro/cli";
import {
  digestFile,
  packageFiles,
  parseInventory,
  verifyInventory,
} from "../runtimes/kokoro/inventory";
import { MAX_SAMPLES, pcm16Wav } from "../runtimes/kokoro/wav";
import {
  archiveName,
  kokoroArtifactSchema,
  kokoroInvocation,
  kokoroReleaseManifestSchema,
} from "./kokoro-package-contract";

const directories: string[] = [];
afterEach(async () => {
  for (const directory of directories.splice(0))
    await rm(directory, { recursive: true, force: true });
});
async function temp() {
  const directory = await mkdtemp("/tmp/localbase-kokoro-contract-");
  directories.push(directory);
  return directory;
}

test("Kokoro converts finite bounded float audio into the existing strict PCM16 contract", () => {
  const wav = pcm16Wav(new Float32Array([-1, -0.5, 0, 0.5, 1]));
  expect(validateSpeechWav(wav).sampleCount).toBe(5);
  const view = new DataView(wav.buffer);
  expect(
    Array.from({ length: 5 }, (_, index) =>
      view.getInt16(44 + index * 2, true),
    ),
  ).toEqual([-32768, -16384, 0, 16384, 32767]);
  expect(
    validateSpeechWav(pcm16Wav(new Float32Array(MAX_SAMPLES))).sampleCount,
  ).toBe(MAX_SAMPLES);
  for (const samples of [
    new Float32Array(),
    new Float32Array(MAX_SAMPLES + 1),
    new Float32Array([NaN]),
    new Float32Array([Infinity]),
    new Float32Array([1.01]),
  ]) {
    expect(() => pcm16Wav(samples)).toThrow();
  }
});

test("package launch is explicit, clean-environment, and rooted in a trusted release inventory", async () => {
  const root = await temp();
  await writeFile(join(root, "cli.js"), "sidecar");
  await writeFile(
    join(root, "inventory.json"),
    JSON.stringify({
      version: 1,
      target: "linux-x64",
      files: await packageFiles(root),
    }),
  );
  const digest = await digestFile(join(root, "inventory.json"));
  const invocation = await kokoroInvocation({
    packageDirectory: root,
    privateDirectory: "/tmp/private",
    inventorySha256: digest,
    arguments: [
      "generate",
      "--model",
      "/tmp/model",
      "--prompt",
      "/tmp/private/prompt",
      "--output",
      "/tmp/private/output",
    ],
  });
  expect(invocation.command.slice(0, 7)).toEqual([
    join(root, "kokoro-tts"),
    "--no-install",
    "--no-env-file",
    "--config",
    join(root, "bunfig.toml"),
    join(root, "cli.js"),
    "generate",
  ]);
  expect(invocation.env).toEqual({
    PATH: "/tmp/private/empty-path",
    HOME: "/tmp/private",
    TMPDIR: "/tmp/private",
    LANG: "C",
    LC_ALL: "C",
  });
  expect(parseArguments(invocation.command.slice(6))).toMatchObject({
    kind: "generate",
    voice: "af_heart",
    inventorySha256: digest,
  });
  expect(parseArguments(["smoke", "--inventory-sha256", digest])).toEqual({
    kind: "smoke",
    inventorySha256: digest,
  });
  await writeFile(join(root, "cli.js"), "tampered sidecar");
  await expect(
    kokoroInvocation({
      packageDirectory: root,
      privateDirectory: "/tmp/private",
      inventorySha256: digest,
      arguments: ["smoke"],
    }),
  ).rejects.toThrow("integrity");
  for (const args of [
    ["smoke"],
    [...invocation.command.slice(6), "--voice", "harbor"],
    [...invocation.command.slice(6), "--inventory-sha256", digest],
    [
      "generate",
      "--model",
      "relative",
      "--prompt",
      "/prompt",
      "--output",
      "/output",
      "--inventory-sha256",
      digest,
    ],
  ]) {
    expect(() => parseArguments(args)).toThrow();
  }
});

test("trusted inventory rejects rewritten manifests, missing or added files, links, and executable-mode changes", async () => {
  const root = await temp();
  await mkdir(join(root, "node_modules"));
  await writeFile(join(root, "cli.js"), "sidecar", { mode: 0o644 });
  await writeFile(join(root, "node_modules/native.node"), "prebuilt", {
    mode: 0o644,
  });
  const inventory = {
    version: 1,
    target: "linux-x64",
    files: await packageFiles(root),
  };
  const inventoryPath = join(root, "inventory.json");
  const contents = `${JSON.stringify(inventory)}\n`;
  await writeFile(inventoryPath, contents);
  const trusted = await digestFile(inventoryPath);
  expect((await verifyInventory(root, trusted)).files.length).toBe(2);
  await writeFile(inventoryPath, `${contents} `);
  await expect(verifyInventory(root, trusted)).rejects.toThrow(
    "pinned release",
  );
  await writeFile(inventoryPath, contents);
  await chmod(join(root, "cli.js"), 0o755);
  await expect(verifyInventory(root, trusted)).rejects.toThrow("integrity");
  await chmod(join(root, "cli.js"), 0o644);
  await writeFile(join(root, "node_modules/external.js"), "unexpected");
  await expect(verifyInventory(root, trusted)).rejects.toThrow("file set");
  await rm(join(root, "node_modules/external.js"));
  await rm(join(root, "node_modules/native.node"));
  await expect(verifyInventory(root, trusted)).rejects.toThrow("file set");
  await symlink("/etc/passwd", join(root, "node_modules/native.node"));
  await expect(verifyInventory(root, trusted)).rejects.toThrow("links");
  expect(() =>
    parseInventory({
      ...inventory,
      files: [{ ...inventory.files[0], path: "../escape" }],
    }),
  ).toThrow("Unsafe");
});

test("actual installed prebuilts must match SRI-authenticated registry content and executable modes", async () => {
  const root = await temp();
  const content = new Uint8Array([0, 1, 2, 3]);
  await writeFile(join(root, "native.node"), content, { mode: 0o644 });
  const tar = pack();
  const chunks: Uint8Array[] = [];
  const complete = new Promise<Uint8Array>((resolve, reject) => {
    tar.on("data", (chunk: Uint8Array) => chunks.push(chunk));
    tar.once("error", reject);
    tar.once("end", () => resolve(gzipSync(Buffer.concat(chunks))));
  });
  tar.entry(
    { name: "prebuilt/native.node", mode: 0o644 },
    Buffer.from(content),
  );
  tar.finalize();
  const archive = await complete;
  const pkg = {
    root,
    name: "native-fixture",
    version: "1.0.0",
    license: "MIT",
    integrity: `sha512-${createHash("sha512").update(archive).digest("base64")}`,
  };
  await verifyRegistryPackage(pkg, archive);
  await expect(
    verifyRegistryPackage(
      { ...pkg, integrity: `sha512-${"a".repeat(88)}` },
      archive,
    ),
  ).rejects.toThrow("integrity");
  await chmod(join(root, "native.node"), 0o755);
  await expect(verifyRegistryPackage(pkg, archive)).rejects.toThrow(
    "differs from registry",
  );
  await chmod(join(root, "native.node"), 0o644);
  await writeFile(join(root, "native.node"), new Uint8Array([4, 3, 2, 1]));
  await expect(verifyRegistryPackage(pkg, archive)).rejects.toThrow(
    "differs from registry",
  );
  await writeFile(join(root, "native.node"), content);
  await writeFile(join(root, "unexpected.js"), "external");
  await expect(verifyRegistryPackage(pkg, archive)).rejects.toThrow(
    "Unexpected installed",
  );
});

test("immutable release manifest binds each canonical archive to its target and inventory digest", () => {
  const artifact = (target: "macos-arm64" | "linux-x64") => ({
    target,
    assetName: archiveName(target),
    expectedSizeBytes: 123,
    sha256: "a".repeat(64),
    inventorySha256: "b".repeat(64),
    format: "tar.gz",
    stripComponents: 0,
  });
  const manifest = {
    version: 1,
    tag: "kokoro-v0.0.1",
    runtimes: {
      "macos-arm64": artifact("macos-arm64"),
      "linux-x64": artifact("linux-x64"),
    },
  };
  expect(kokoroReleaseManifestSchema.safeParse(manifest).success).toBe(true);
  expect(
    kokoroArtifactSchema.safeParse({
      ...artifact("linux-x64"),
      assetName: "other.tar.gz",
    }).success,
  ).toBe(false);
  expect(
    kokoroReleaseManifestSchema.safeParse({
      ...manifest,
      runtimes: { ...manifest.runtimes, "linux-x64": artifact("macos-arm64") },
    }).success,
  ).toBe(false);
});
