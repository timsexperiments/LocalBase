import { expect, test } from "bun:test";
import {
  validateSdArchiveEntries,
  validateSdBinaryArchitecture,
  type SdTarget,
} from "./sd-release";

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
