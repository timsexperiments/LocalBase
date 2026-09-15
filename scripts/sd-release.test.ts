import { expect, test } from "bun:test";
import rawManifest from "../src/manager/managed-runtime-manifest.json";
import {
  validateSdArchiveEntries,
  validateSdBinaryArchitecture,
  type SdTarget,
} from "./sd-release";
import {
  sdReleaseReceiptSchema,
  validateSdReleaseTag,
  type Fetcher,
} from "./sd-release/contracts";
import { sdManifestMatches, updateSdManifest } from "./sd-release/manifest";
import { verifyPublishedSdRelease } from "./sd-release/published-release";

const licenses = [
  "LICENSE.darts-clone.txt",
  "LICENSE.ggml.txt",
  "LICENSE.libwebm.txt",
  "LICENSE.libwebp.txt",
  "LICENSE.nlohmann-json.txt",
  "LICENSE.oniguruma.txt",
  "LICENSE.stable-diffusion.cpp.txt",
  "LICENSE.utf8proc.txt",
];

const provenance = JSON.stringify({
  version: 1,
  sources: [
    {
      name: "stable-diffusion.cpp",
      revision: "07a85c74cb08cda3aa176f688c5d8f522615e2b9",
      sha256:
        "a1850648d5fd6e12b23d8f290023563c7dad74ec64302d9ce6862ab880f3d819",
      url: "https://example.test/stable-diffusion.cpp",
    },
    {
      name: "ggml",
      revision: "e20c3a14aa70ee84ca58499814206dd08d8026bc",
      sha256:
        "3dde7c76c0dc2bce436ab16258baafbf3f1a1dbf933a3583ca32471aadb0e160",
      url: "https://example.test/ggml",
    },
    {
      name: "libwebp",
      revision: "0c9546f7efc61eac7f79ae115c3f99c91c21c443",
      sha256:
        "6cb433070b4461179067b0901a682e4e14b220354fc35884c1b1315adedefc99",
      url: "https://example.test/libwebp",
    },
    {
      name: "libwebm",
      revision: "5bf12267eea773a32fcf4949de52b0add158a8d5",
      sha256:
        "294049a03d35e4480a94a5ced96c80ebfd4114554966709c8c97de4f19bd941a",
      url: "https://example.test/libwebm",
    },
  ],
  patch: { name: "inline-wav-audio.patch", sha256: "a".repeat(64) },
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

function header(target: SdTarget) {
  const bytes = new Uint8Array(32);
  if (target === "linux-x64") {
    bytes.set([0x7f, 0x45, 0x4c, 0x46, 2, 1]);
    bytes[18] = 0x3e;
  } else {
    bytes.set([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1]);
  }
  return bytes;
}

test("requires the binary and complete license set", () => {
  const entries = [
    { name: "sd-server", type: "file", bytes: header("linux-x64") },
    {
      name: "SOURCE.sd-server.json",
      type: "file",
      bytes: new TextEncoder().encode(provenance),
    },
    ...licenses.map((name) => ({
      name,
      type: "file",
      bytes: new TextEncoder().encode("license"),
    })),
  ];
  expect(validateSdArchiveEntries("linux-x64", entries)).toEqual(
    header("linux-x64"),
  );
  expect(() =>
    validateSdArchiveEntries("linux-x64", entries.slice(0, -1)),
  ).toThrow("eight required license files");
});

test("validates canonical runtime architectures", () => {
  expect(() =>
    validateSdBinaryArchitecture("linux-x64", header("linux-x64")),
  ).not.toThrow();
  expect(() =>
    validateSdBinaryArchitecture("macos-arm64", header("macos-arm64")),
  ).not.toThrow();
  expect(() =>
    validateSdBinaryArchitecture("macos-arm64", header("linux-x64")),
  ).toThrow("architecture");
});

test("accepts only canonical immutable sd-server tags", () => {
  expect(validateSdReleaseTag("sd-server-v0.0.1")).toBe("sd-server-v0.0.1");
  for (const invalid of ["sd-v0.0.1", "sd-server-v00.0.1", "sd-server-v0.1"]) {
    expect(() => validateSdReleaseTag(invalid)).toThrow();
  }
});

function sha256(bytes: Uint8Array): string {
  return new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
}

test("verifies publication identity and updates only sd-server entries", async () => {
  const repository = "timsexperiments/LocalBase";
  const tag = "sd-server-v0.0.1";
  const linux = new TextEncoder().encode("linux archive");
  const macos = new TextEncoder().encode("macOS archive");
  const source = new TextEncoder().encode(provenance);
  const checksums = new TextEncoder().encode(
    `${sha256(macos)}  sd-server-macos-arm64.zip\n${sha256(linux)}  sd-server-linux-x64.tar.gz\n`,
  );
  const values = {
    "sd-server-linux-x64.tar.gz": linux,
    "sd-server-macos-arm64.zip": macos,
    "SOURCE.sd-server.json": source,
    "checksums.txt": checksums,
  };
  const assets = Object.entries(values).map(([name, bytes]) => ({
    name,
    size: bytes.byteLength,
    digest: `sha256:${sha256(bytes)}`,
    browser_download_url: `https://github.com/${repository}/releases/download/${tag}/${name}`,
  }));
  const fetcher: Fetcher = async (input) => {
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
  const output = "/tmp/localbase-sd-release-receipt-test.json";
  const receipt = await verifyPublishedSdRelease(
    repository,
    tag,
    output,
    fetcher,
  );
  expect(sdReleaseReceiptSchema.parse(await Bun.file(output).json())).toEqual(
    receipt,
  );
  const before = structuredClone(rawManifest);
  const updated = updateSdManifest(before, receipt);
  expect(sdManifestMatches(updated, receipt)).toBeTrue();
  for (const target of updated.targets) {
    const original = before.targets.find(
      (candidate) =>
        candidate.platform === target.platform &&
        candidate.architecture === target.architecture,
    )!;
    expect(target.runtimes["llama-server"]).toEqual(
      original.runtimes["llama-server"],
    );
    expect(target.runtimes["whisper-server"]).toEqual(
      original.runtimes["whisper-server"],
    );
  }
});
