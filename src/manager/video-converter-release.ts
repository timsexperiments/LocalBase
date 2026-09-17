import type { ManagedInstallOptions, installManagedRuntime } from "./binaries";
import type { PlatformTarget } from "./managed-runtime-manifest";

// GitHub release asset digests, independently byte-verified against b6.1.1.
// This packaging tag includes different FFmpeg versions; retain upstream notices.
const baseUrl =
  "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.1.1";
const targets = [
  {
    os: "darwin",
    cpu: "arm64",
    size: 45568216,
    sha256: "a90e3db6a3fd35f6074b013f948b1aa45b31c6375489d39e572bea3f18336584",
    licenseSize: 4376,
    licenseSha256:
      "cb48bf09a11f5fb576cddb0431c8f5ed0a60157a9ec942adffc13907cbe083f2",
    readmeSize: 1810,
    readmeSha256:
      "05ba4b92c96605434b1aaae3eedf5a2c280c9607bf78ffca9a5b536d9af2dc6a",
  },
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
