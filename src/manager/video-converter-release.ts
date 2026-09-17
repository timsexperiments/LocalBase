import type { ManagedInstallOptions, installManagedRuntime } from "./binaries";
import type { PlatformTarget } from "./managed-runtime-manifest";

// GitHub release asset digests, independently byte-verified against b6.1.1.
// This packaging tag includes different FFmpeg versions; retain upstream notices.
const baseUrl =
  "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.1.1";
const macArm64BaseUrl =
  "https://ffmpeg.martin-riedl.de/download/macos/arm64/1787073674_9.0.1";

// GPLv3-or-later, verified by the signed binary's -L and -version output.
// The ZIP contains only a root-level ffmpeg; notices are verified separately.
// FFmpeg source: https://ffmpeg.org/releases/ffmpeg-9.0.1.tar.xz
// Publisher build/dependency sources:
// https://git.martin-riedl.de/ffmpeg/build-script/src/commit/f63b8aab8f5ce1a067da86ba69e34a36a7e217e5
const macArm64 = {
  release: {
    name: "ffmpeg",
    tag: "martin-riedl-1787073674-9.0.1",
    assetName: "ffmpeg.zip",
    url: `${macArm64BaseUrl}/ffmpeg.zip`,
    expectedSizeBytes: 28447413,
    sha256: "8287a1b2229e05eb41859f073e18e6c52c60a778f2f5e6881070fe51b79407fe",
    format: "zip",
    stripComponents: 0,
  } satisfies Parameters<typeof installManagedRuntime>[1],
  supportFiles: [
    {
      filename: "LICENSE",
      url: "https://raw.githubusercontent.com/FFmpeg/FFmpeg/n9.0.1/COPYING.GPLv3",
      expectedSizeBytes: 35147,
      sha256:
        "8ceb4b9ee5adedde47b31e975c1d90c73ad27b6b165a1dcd80c7c545eb65b903",
    },
    {
      filename: "README",
      url: `${macArm64BaseUrl}/versions.txt`,
      expectedSizeBytes: 1950,
      sha256:
        "9508bf4dad7245f28ca364934c3d90bdbb7d7cad35f5211aaf56120a7333d62c",
    },
  ] satisfies ManagedInstallOptions["supportFiles"],
};
const targets = [
  {
    os: "darwin",
    cpu: "x64",
    size: 78862176,
    sha256: "ebdddc936f61e14049a2d4b549a412b8a40deeff6540e58a9f2a2da9e6b18894",
    licenseSize: 4346,
    licenseSha256:
      "2e1d16c72fd74e12063776371da757322f8b77589386532f4fd8634bde7de1af",
    readmeSize: 6227,
    readmeSha256:
      "e88a0325f8e5b75210355e37341824f074d3cd82def2125be54c914b62848a36",
  },
  {
    os: "linux",
    cpu: "x64",
    size: 79826272,
    sha256: "e7e7fb30477f717e6f55f9180a70386c62677ef8a4d4d1a5d948f4098aa3eb99",
    licenseSize: 35147,
    licenseSha256:
      "8ceb4b9ee5adedde47b31e975c1d90c73ad27b6b165a1dcd80c7c545eb65b903",
    readmeSize: 2235,
    readmeSha256:
      "72f4b1b06d419d22ace6e7cc75f06826f90737345aa0b1736158929f4aacc537",
  },
  {
    os: "linux",
    cpu: "arm64",
    size: 51134160,
    sha256: "6bb182d0d75d23028db82e9e4f723ca69b853d055698486e6984ddb2c06fb8ce",
    licenseSize: 35147,
    licenseSha256:
      "8ceb4b9ee5adedde47b31e975c1d90c73ad27b6b165a1dcd80c7c545eb65b903",
    readmeSize: 2217,
    readmeSha256:
      "d6777d2fd276b23f0ac6666fa619e88ffe4826521881c7ff83836e30cb4acec2",
  },
];

export function videoConverterRelease(target: PlatformTarget) {
  if (target.os === "darwin" && target.cpu === "arm64") return macArm64;
  const pin = targets.find(
    ({ os, cpu }) => os === target.os && cpu === target.cpu,
  );
  if (!pin) {
    throw new Error(
      `No packaged video converter for ${target.os}/${target.cpu}.`,
    );
  }
  const assetName = `ffmpeg-${pin.os}-${pin.cpu}`;
  const release = {
    name: "ffmpeg",
    tag: "b6.1.1",
    assetName,
    url: `${baseUrl}/${assetName}`,
    expectedSizeBytes: pin.size,
    sha256: pin.sha256,
    format: "binary",
    stripComponents: 0,
  } satisfies Parameters<typeof installManagedRuntime>[1];
  const supportFiles = [
    {
      filename: "LICENSE",
      url: `${baseUrl}/${pin.os}-${pin.cpu}.LICENSE`,
      expectedSizeBytes: pin.licenseSize,
      sha256: pin.licenseSha256,
    },
    {
      filename: "README",
      url: `${baseUrl}/${pin.os}-${pin.cpu}.README`,
      expectedSizeBytes: pin.readmeSize,
      sha256: pin.readmeSha256,
    },
  ] satisfies ManagedInstallOptions["supportFiles"];
  return { release, supportFiles };
}
