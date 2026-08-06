import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { zipSync } from "fflate";
import { pack } from "tar-stream";
import rawManifest from "../src/manager/managed-runtime-manifest.json";
import {
  qualifyWhisperArchive,
  runWhisperReleaseCli,
  updateWhisperManifest,
  validateWhisperReleaseTag,
  verifyPublishedWhisperRelease,
  verifyWhisperManifest,
  whisperManifestMatches,
  type Fetcher,
} from "./whisper-release";

const directories: string[] = [];
afterEach(() =>
  directories
    .splice(0)
    .forEach((directory) =>
      rmSync(directory, { recursive: true, force: true }),
    ),
);

function temp(): string {
  const directory = mkdtempSync("/tmp/localbase-whisper-release-");
  directories.push(directory);
  return directory;
}

function linuxBinary(): Uint8Array {
  const bytes = new Uint8Array(32);
  bytes.set([0x7f, 0x45, 0x4c, 0x46, 2, 1]);
  bytes[18] = 0x3e;
  return bytes;
}

function macosBinary(): Uint8Array {
  const bytes = new Uint8Array(32);
  bytes.set([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1]);
  return bytes;
}

async function tarGz(entries: Record<string, Uint8Array>): Promise<Uint8Array> {
  const archive = pack();
  const chunks: Uint8Array[] = [];
  const complete = new Promise<void>((resolve, reject) => {
    archive.on("data", (chunk: Uint8Array) => chunks.push(chunk));
    archive.once("end", resolve);
    archive.once("error", reject);
  });
  for (const [name, bytes] of Object.entries(entries)) {
    await new Promise<void>((resolve, reject) =>
      archive.entry({ name }, bytes, (error) =>
        error ? reject(error) : resolve(),
      ),
    );
  }
  archive.finalize();
  await complete;
  const length = chunks.reduce((total, chunk) => total + chunk.length, 0);
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.length;
  }
  return new Uint8Array(
    await new Response(
      new Blob([bytes]).stream().pipeThrough(new CompressionStream("gzip")),
    ).arrayBuffer(),
  );
}

function sha256(bytes: Uint8Array): string {
  return new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
}

test("validates canonical immutable Whisper tags", async () => {
  expect(validateWhisperReleaseTag("whisper-v0.0.1")).toBe("whisper-v0.0.1");
  expect(() => validateWhisperReleaseTag("whisper-v01.0.1")).toThrow(
    "Invalid string",
  );
  expect(() => validateWhisperReleaseTag("v0.0.1")).toThrow("Invalid string");
  const output = join(temp(), "github-output");
  await runWhisperReleaseCli([
    "validate-tag",
    "--tag",
    "whisper-v0.0.1",
    "--github-output",
    output,
  ]);
  expect(await Bun.file(output).text()).toBe(
    "tag=whisper-v0.0.1\nbranch=tim/whisper-v0.0.1-manifest\n",
  );
});

test("qualifies canonical Linux and macOS archives", async () => {
  const directory = temp();
  const linuxArchive = join(directory, "whisper-server-linux-x64.tar.gz");
  const macosArchive = join(directory, "whisper-server-macos-arm64.zip");
  await Bun.write(
    linuxArchive,
    await tarGz({ "whisper-server": linuxBinary() }),
  );
  await Bun.write(macosArchive, zipSync({ "whisper-server": macosBinary() }));

  await qualifyWhisperArchive(
    "linux-x64",
    linuxArchive,
    join(directory, "linux"),
    undefined,
  );
  const commands: string[][] = [];
  await qualifyWhisperArchive(
    "macos-arm64",
    macosArchive,
    join(directory, "macos"),
    "TEAM123456",
    async (args) => {
      commands.push(args);
      return {
        stdout: "",
        stderr:
          "Authority=Developer ID Application: LocalBase (TEAM123456)\n" +
          "Authority=Developer ID Certification Authority\n" +
          "TeamIdentifier=TEAM123456\n" +
          "CodeDirectory=v=20500 size=1 flags=0x10000(runtime)\n",
      };
    },
  );
  expect(commands).toEqual([
    expect.arrayContaining(["--verify", "--strict"]),
    expect.arrayContaining(["--display", "--verbose=4"]),
  ]);
});

test("rejects noncanonical archive contents and signatures", async () => {
  const directory = temp();
  const nestedArchive = join(directory, "whisper-server-linux-x64.tar.gz");
  await Bun.write(
    nestedArchive,
    await tarGz({ "nested/whisper-server": linuxBinary() }),
  );
  await expect(
    qualifyWhisperArchive(
      "linux-x64",
      nestedArchive,
      join(directory, "nested"),
      undefined,
    ),
  ).rejects.toThrow("root-level whisper-server");

  const macosArchive = join(directory, "whisper-server-macos-arm64.zip");
  await Bun.write(macosArchive, zipSync({ "whisper-server": macosBinary() }));
  await expect(
    qualifyWhisperArchive(
      "macos-arm64",
      macosArchive,
      join(directory, "macos"),
      "TEAM123456",
      async () => ({
        stdout: "",
        stderr:
          "Authority=Developer ID Application: LocalBase (OTHER12345)\n" +
          "TeamIdentifier=OTHER12345\n" +
          "CodeDirectory=v=20500 size=1 flags=0x10000(runtime)\n",
      }),
    ),
  ).rejects.toThrow("TEAM123456");
});

function releaseFixture(options?: { checksums?: string; assets?: unknown[] }): {
  fetcher: Fetcher;
  expected: Record<string, { bytes: Uint8Array; digest: string }>;
} {
  const repository = "timsexperiments/LocalBase";
  const tag = "whisper-v0.0.1";
  const linux = new TextEncoder().encode("linux archive");
  const macos = new TextEncoder().encode("macos archive");
  const checksums =
    options?.checksums ??
    `${sha256(macos)}  whisper-server-macos-arm64.zip\n${sha256(linux)}  whisper-server-linux-x64.tar.gz\n`;
  const checksumBytes = new TextEncoder().encode(checksums);
  const expected = {
    "whisper-server-linux-x64.tar.gz": { bytes: linux, digest: sha256(linux) },
    "whisper-server-macos-arm64.zip": { bytes: macos, digest: sha256(macos) },
    "checksums.txt": { bytes: checksumBytes, digest: sha256(checksumBytes) },
  };
  const assets =
    options?.assets ??
    Object.entries(expected).map(([name, value]) => ({
      name,
      size: value.bytes.byteLength,
      digest: `sha256:${value.digest}`,
      browser_download_url: `https://github.com/${repository}/releases/download/${tag}/${name}`,
    }));
  const fetcher: Fetcher = async (input) => {
    const url = String(input);
    if (url.includes(`/releases/tags/${tag}`)) {
      return Response.json({
        id: 42,
        tag_name: tag,
        draft: false,
        prerelease: false,
        assets,
      });
    }
    const asset = Object.entries(expected).find(([name]) =>
      url.endsWith(`/${name}`),
    );
    return asset
      ? new Response(asset[1].bytes)
      : new Response("missing", { status: 404, statusText: "Not Found" });
  };
  return { fetcher, expected };
}

test("writes deterministic receipts only from verified published metadata", async () => {
  const directory = temp();
  const output = join(directory, "receipt.json");
  const { fetcher } = releaseFixture();
  const receipt = await verifyPublishedWhisperRelease(
    "timsexperiments/LocalBase",
    "whisper-v0.0.1",
    output,
    fetcher,
  );
  expect(await Bun.file(output).json()).toEqual(receipt);
  expect(receipt.runtimes["linux-x64"]).toMatchObject({
    assetName: "whisper-server-linux-x64.tar.gz",
    format: "tar.gz",
    stripComponents: 0,
  });
});

test("rejects malformed published release assets and checksums", async () => {
  const directory = temp();
  const extraAsset = releaseFixture().expected;
  const malformedAssets = Object.entries(extraAsset).map(([name, value]) => ({
    name,
    size: value.bytes.byteLength,
    digest: `sha256:${value.digest}`,
    browser_download_url: `https://github.com/timsexperiments/LocalBase/releases/download/whisper-v0.0.1/${name}`,
  }));
  malformedAssets.push({
    name: "unexpected.txt",
    size: 1,
    digest: `sha256:${"0".repeat(64)}`,
    browser_download_url:
      "https://github.com/timsexperiments/LocalBase/releases/download/whisper-v0.0.1/unexpected.txt",
  });
  await expect(
    verifyPublishedWhisperRelease(
      "timsexperiments/LocalBase",
      "whisper-v0.0.1",
      join(directory, "assets.json"),
      releaseFixture({ assets: malformedAssets }).fetcher,
    ),
  ).rejects.toThrow("must contain exactly");
  await expect(
    verifyPublishedWhisperRelease(
      "timsexperiments/LocalBase",
      "whisper-v0.0.1",
      join(directory, "checksums.json"),
      releaseFixture({
        checksums: `${"0".repeat(64)}  whisper-server-linux-x64.tar.gz\n`,
      }).fetcher,
    ),
  ).rejects.toThrow("checksums.txt must contain exactly");
  const invalidDigestAssets = Object.entries(releaseFixture().expected).map(
    ([name, value]) => ({
      name,
      size: value.bytes.byteLength,
      digest:
        name === "checksums.txt"
          ? "sha256:not-a-digest"
          : `sha256:${value.digest}`,
      browser_download_url: `https://github.com/timsexperiments/LocalBase/releases/download/whisper-v0.0.1/${name}`,
    }),
  );
  await expect(
    verifyPublishedWhisperRelease(
      "timsexperiments/LocalBase",
      "whisper-v0.0.1",
      join(directory, "digest.json"),
      releaseFixture({ assets: invalidDigestAssets }).fetcher,
    ),
  ).rejects.toThrow("digest");
});

test("updates and verifies only managed Whisper manifest entries", async () => {
  const directory = temp();
  const receipt = await verifyPublishedWhisperRelease(
    "timsexperiments/LocalBase",
    "whisper-v0.0.1",
    join(directory, "receipt.json"),
    releaseFixture().fetcher,
  );
  const original = structuredClone(rawManifest);
  const updated = updateWhisperManifest(original, receipt);
  expect(whisperManifestMatches(original, receipt)).toBeFalse();
  expect(whisperManifestMatches(updated, receipt)).toBeTrue();
  const manifestPath = join(directory, "managed-runtime-manifest.json");
  const statusPath = join(directory, "github-output");
  await Bun.write(manifestPath, JSON.stringify(updated));
  await runWhisperReleaseCli([
    "manifest-status",
    "--manifest",
    manifestPath,
    "--receipt",
    join(directory, "receipt.json"),
    "--github-output",
    statusPath,
  ]);
  expect(await Bun.file(statusPath).text()).toBe("synchronized=true\n");
  verifyWhisperManifest(updated, receipt);
  for (const target of updated.targets) {
    const before = original.targets.find(
      (candidate) =>
        candidate.platform === target.platform &&
        candidate.architecture === target.architecture,
    )!;
    for (const runtime of ["llama-server", "sd-server"] as const) {
      expect(target.runtimes[runtime]).toEqual(before.runtimes[runtime]);
    }
  }
  expect(() => verifyWhisperManifest(original, receipt)).toThrow(
    "do not match the release receipt",
  );
});
