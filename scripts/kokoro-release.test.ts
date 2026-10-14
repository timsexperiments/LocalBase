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
import { verifyPublished, verifyRegistryPackage } from "./kokoro-release";
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
  requiredKokoroLicenses,
  verifyDependencyLicenses,
  verifyKokoroLicenses,
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

test("release packaging requires every declared runtime license text", async () => {
  const root = await temp();
  for (const license of requiredKokoroLicenses)
    await writeFile(join(root, license), "license text");
  await expect(verifyKokoroLicenses(root)).resolves.toBeUndefined();
  await rm(join(root, requiredKokoroLicenses[1]!));
  await expect(verifyKokoroLicenses(root)).rejects.toThrow(
    requiredKokoroLicenses[1],
  );
  await writeFile(join(root, requiredKokoroLicenses[1]!), "");
  await expect(verifyKokoroLicenses(root)).rejects.toThrow(
    requiredKokoroLicenses[1],
  );
});

test("dependency license inventory rejects absent declarations and texts", async () => {
  const root = await temp();
  await writeFile(join(root, "LICENSE.MIT.txt"), "MIT terms");
  await expect(
    verifyDependencyLicenses(root, [
      { name: "known", license: "(MIT OR CC0-1.0)" },
    ]),
  ).rejects.toThrow("CC0-1.0");
  await expect(
    verifyDependencyLicenses(root, [{ name: "unknown", license: undefined }]),
  ).rejects.toThrow("no declared license");
});

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

function publishedReleaseFixture() {
  const repository = "timsexperiments/LocalBase";
  const tag = "kokoro-v0.0.1";
  const sha = (bytes: Uint8Array) =>
    createHash("sha256").update(bytes).digest("hex");
  const archives = {
    "macos-arm64": new TextEncoder().encode("mac archive"),
    "linux-x64": new TextEncoder().encode("linux archive"),
  } as const;
  const runtimes = Object.fromEntries(
    (["macos-arm64", "linux-x64"] as const).map((target) => [
      target,
      {
        target,
        assetName: archiveName(target),
        expectedSizeBytes: archives[target].length,
        sha256: sha(archives[target]),
        inventorySha256: "b".repeat(64),
        format: "tar.gz",
        stripComponents: 0,
      },
    ]),
  );
  const files = new Map<string, Uint8Array>([
    [archiveName("macos-arm64"), archives["macos-arm64"]],
    [archiveName("linux-x64"), archives["linux-x64"]],
    [
      "kokoro-runtime-manifest.json",
      new TextEncoder().encode(JSON.stringify({ version: 1, tag, runtimes })),
    ],
    [
      "checksums.txt",
      new TextEncoder().encode(
        (["macos-arm64", "linux-x64"] as const)
          .map((target) => `${sha(archives[target])}  ${archiveName(target)}`)
          .join("\n") + "\n",
      ),
    ],
  ]);
  const url = (name: string) =>
    `https://github.com/${repository}/releases/download/${tag}/${name}`;
  const release = {
    id: 42,
    tag_name: tag,
    draft: false,
    prerelease: false,
    assets: [...files].map(([name, bytes]) => ({
      name,
      size: bytes.length,
      digest: `sha256:${sha(bytes)}`,
      browser_download_url: url(name),
    })),
  };
  const fetcher = async (input: string) => {
    if (input.endsWith(`/releases/tags/${tag}`)) return Response.json(release);
    const name = input.split("/").at(-1)!;
    const bytes = files.get(name);
    return bytes
      ? new Response(new Uint8Array(bytes))
      : new Response("", { status: 404 });
  };
  return { repository, tag, files, release, fetcher };
}

test("published release verification pins both canonical archives into a receipt", async () => {
  const fixture = publishedReleaseFixture();
  const receipt = await verifyPublished(
    fixture.repository,
    fixture.tag,
    fixture.fetcher,
  );
  expect(receipt.releaseId).toBe(42);
  expect(receipt.runtimes["linux-x64"].url).toBe(
    `https://github.com/${fixture.repository}/releases/download/kokoro-v0.0.1/kokoro-tts-linux-x64.tar.gz`,
  );
  expect(receipt.runtimes["macos-arm64"].artifact.expectedSizeBytes).toBe(
    fixture.files.get(archiveName("macos-arm64"))!.length,
  );
});

test("published release verification rejects tampered assets, extra assets, and noncanonical URLs", async () => {
  const tampered = publishedReleaseFixture();
  tampered.files.set(
    archiveName("linux-x64"),
    new TextEncoder().encode("evil archive!"),
  );
  await expect(
    verifyPublished(tampered.repository, tampered.tag, tampered.fetcher),
  ).rejects.toThrow("integrity");

  const extra = publishedReleaseFixture();
  extra.release.assets.push({
    ...extra.release.assets[0]!,
    name: "extra.bin",
  });
  await expect(
    verifyPublished(extra.repository, extra.tag, extra.fetcher),
  ).rejects.toThrow("asset set");

  const moved = publishedReleaseFixture();
  moved.release.assets[0]!.browser_download_url =
    "https://example.com/kokoro-tts-macos-arm64.tar.gz";
  await expect(
    verifyPublished(moved.repository, moved.tag, moved.fetcher),
  ).rejects.toThrow("Noncanonical");

  const draft = publishedReleaseFixture();
  draft.release.draft = true;
  await expect(
    verifyPublished(draft.repository, draft.tag, draft.fetcher),
  ).rejects.toThrow();
});
