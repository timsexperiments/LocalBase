import { expect, test } from "bun:test";
import { gzipSync, zipSync } from "fflate";
import { pack as createTarPack } from "tar-stream";
import rawManifest from "../src/manager/managed-runtime-manifest.json";
import { registerTtsServerTool } from "./build-tts-server";
import {
  validateTtsArchiveEntries,
  validateTtsBinaryArchitecture,
  type TtsTarget,
} from "./tts-release";
import {
  ttsReleaseReceiptSchema,
  validateTtsReleaseTag,
  type Fetcher,
} from "./tts-release/contracts";
import { ttsManifestMatches, updateTtsManifest } from "./tts-release/manifest";
import { verifyPublishedTtsRelease } from "./tts-release/published-release";

const licenses = [
  "LICENSE.cpp-httplib.txt",
  "LICENSE.llama.cpp.txt",
  "LICENSE.nlohmann-json.txt",
];

const provenance = JSON.stringify({
  version: 1,
  sources: [
    {
      name: "llama.cpp",
      tag: "b10419",
      revision: "aee56b3abfeb134218fe5c496c329a1faac1ef28",
      sha256:
        "43ed9540769b26a2f180e60d0f14ffd2eaef7f844ab12b40f5b7b6fa37a2c387",
      url: "https://example.test/llama.cpp",
    },
  ],
  localSources: [
    { name: "CMakeLists.txt", sha256: "a".repeat(64) },
    { name: "tts-server.cpp", sha256: "b".repeat(64) },
  ],
  runtimeRequirements: {
    "linux-x64": {
      buildBaseline: "Ubuntu 24.04 x86_64",
      gpu: "Vulkan loader and a compatible Vulkan GPU driver",
      systemLibraries:
        "glibc and libstdc++ compatible with the Ubuntu 24.04 build baseline",
    },
    "macos-arm64": {
      buildBaseline: "macOS 14 arm64",
      gpu: "Apple Metal",
      systemLibraries: "macOS system frameworks",
    },
  },
});

function header(target: TtsTarget) {
  const bytes = new Uint8Array(32);
  if (target === "linux-x64") {
    bytes.set([0x7f, 0x45, 0x4c, 0x46, 2, 1]);
    bytes[18] = 0x3e;
  } else {
    bytes.set([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1]);
  }
  return bytes;
}

test("registers tts-server beside llama-tts exactly once", () => {
  const upstream =
    "    add_subdirectory(run)\n    add_subdirectory(tts)\n    add_subdirectory(mtmd)\n";
  expect(registerTtsServerTool(upstream)).toBe(
    "    add_subdirectory(run)\n    add_subdirectory(tts)\n    add_subdirectory(tts-server)\n    add_subdirectory(mtmd)\n",
  );
  expect(() => registerTtsServerTool("    add_subdirectory(run)\n")).toThrow(
    "exactly once",
  );
  expect(() => registerTtsServerTool(registerTtsServerTool(upstream))).toThrow(
    "already registers",
  );
});

test("requires the binary, provenance, and complete license set", () => {
  const entries = [
    { name: "tts-server", type: "file", bytes: header("linux-x64") },
    {
      name: "SOURCE.tts-server.json",
      type: "file",
      bytes: new TextEncoder().encode(provenance),
    },
    ...licenses.map((name) => ({
      name,
      type: "file",
      bytes: new TextEncoder().encode("license"),
    })),
  ];
  expect(validateTtsArchiveEntries("linux-x64", entries)).toEqual(
    header("linux-x64"),
  );
  expect(() =>
    validateTtsArchiveEntries("linux-x64", entries.slice(0, -1)),
  ).toThrow("three required license files");
  expect(() =>
    validateTtsArchiveEntries("linux-x64", [
      ...entries.slice(0, 1),
      {
        name: "SOURCE.tts-server.json",
        type: "file",
        bytes: new TextEncoder().encode(provenance.replace("b10419", "b1")),
      },
      ...entries.slice(2),
    ]),
  ).toThrow("invalid source provenance");
});

test("validates canonical runtime architectures", () => {
  expect(() =>
    validateTtsBinaryArchitecture("linux-x64", header("linux-x64")),
  ).not.toThrow();
  expect(() =>
    validateTtsBinaryArchitecture("macos-arm64", header("macos-arm64")),
  ).not.toThrow();
  expect(() =>
    validateTtsBinaryArchitecture("macos-arm64", header("linux-x64")),
  ).toThrow("architecture");
});

test("accepts only canonical immutable tts-server tags", () => {
  expect(validateTtsReleaseTag("tts-server-v0.0.1")).toBe("tts-server-v0.0.1");
  for (const invalid of [
    "tts-v0.0.1",
    "tts-server-v00.0.1",
    "tts-server-v0.1",
    "sd-server-v0.0.1",
  ]) {
    expect(() => validateTtsReleaseTag(invalid)).toThrow();
  }
});

function sha256(bytes: Uint8Array): string {
  return new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
}

async function tarGz(entries: Record<string, Uint8Array>): Promise<Uint8Array> {
  const archive = createTarPack();
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
  const size = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
  const tar = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    tar.set(chunk, offset);
    offset += chunk.length;
  }
  return gzipSync(tar);
}

function releaseFetcher(
  repository: string,
  tag: string,
  values: Record<string, Uint8Array>,
): Fetcher {
  const assets = Object.entries(values).map(([name, bytes]) => ({
    name,
    size: bytes.byteLength,
    digest: `sha256:${sha256(bytes)}`,
    browser_download_url: `https://github.com/${repository}/releases/download/${tag}/${name}`,
  }));
  return async (input) => {
    const url = String(input);
    if (url.endsWith(`/releases/tags/${tag}`)) {
      return Response.json({
        id: 42,
        tag_name: tag,
        draft: false,
        prerelease: false,
        assets,
      });
    }
    const entry = Object.entries(values).find(([name]) =>
      url.endsWith(`/${name}`),
    );
    return entry ? new Response(entry[1]) : new Response(null, { status: 404 });
  };
}

test("verifies publication identity and publishes only tts-server entries", async () => {
  const repository = "timsexperiments/LocalBase";
  const tag = "tts-server-v0.0.1";
  const source = new TextEncoder().encode(provenance);
  const linux = await tarGz({ "SOURCE.tts-server.json": source });
  const macos = zipSync({ "SOURCE.tts-server.json": source });
  const values = {
    "tts-server-linux-x64.tar.gz": linux,
    "tts-server-macos-arm64.zip": macos,
    "SOURCE.tts-server.json": source,
    "checksums.txt": new TextEncoder().encode(
      `${sha256(macos)}  tts-server-macos-arm64.zip\n${sha256(linux)}  tts-server-linux-x64.tar.gz\n`,
    ),
  };
  const output = "/tmp/localbase-tts-release-receipt-test.json";
  const receipt = await verifyPublishedTtsRelease(
    repository,
    tag,
    output,
    releaseFetcher(repository, tag, values),
  );
  expect(ttsReleaseReceiptSchema.parse(await Bun.file(output).json())).toEqual(
    receipt,
  );

  const before = structuredClone(rawManifest);
  expect(ttsManifestMatches(before, receipt)).toBeFalse();
  const updated = updateTtsManifest(before, receipt);
  expect(ttsManifestMatches(updated, receipt)).toBeTrue();
  for (const target of updated.targets) {
    const original = before.targets.find(
      (candidate) =>
        candidate.platform === target.platform &&
        candidate.architecture === target.architecture,
    )!;
    for (const [name, runtime] of Object.entries(original.runtimes)) {
      if (name === "tts-server") continue;
      expect((target.runtimes as Record<string, unknown>)[name]).toEqual(
        runtime,
      );
    }
    if (target.tier !== "managed") continue;
    const published = target.runtimes["tts-server"]!;
    expect("pendingPublication" in published).toBeFalse();
    expect(published.tag).toBe(tag);
    expect(published.sha256).toBe(
      sha256(target.platform === "linux" ? linux : macos),
    );
  }

  const mismatchedMacos = zipSync({
    "SOURCE.tts-server.json": new TextEncoder().encode(
      provenance.replace("a".repeat(64), "c".repeat(64)),
    ),
  });
  const mismatched = {
    ...values,
    "tts-server-macos-arm64.zip": mismatchedMacos,
    "checksums.txt": new TextEncoder().encode(
      `${sha256(mismatchedMacos)}  tts-server-macos-arm64.zip\n${sha256(linux)}  tts-server-linux-x64.tar.gz\n`,
    ),
  };
  await expect(
    verifyPublishedTtsRelease(
      repository,
      tag,
      output,
      releaseFetcher(repository, tag, mismatched),
    ),
  ).rejects.toThrow("source identity does not match");
  await expect(
    verifyPublishedTtsRelease(
      repository,
      tag,
      output,
      releaseFetcher(repository, tag, {
        ...values,
        "extra.txt": new TextEncoder().encode("x"),
      }),
    ),
  ).rejects.toThrow("exactly the canonical tts-server assets");
});
