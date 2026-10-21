// Golden video converter pins from TS (manager/video-converter-release.ts):
// the ffmpeg release and its support files for every target, in the Go
// runtime manifest's shape ("copy" support files are used as published).
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/ffmpeg.ts \
//     > go/internal/runtime/testdata/ffmpeg-pins.json
import { videoConverterRelease } from "../../../src/manager/video-converter-release";

const targets = [
  { os: "darwin", cpu: "arm64" },
  { os: "darwin", cpu: "x64" },
  { os: "linux", cpu: "x64" },
  { os: "linux", cpu: "arm64" },
] as const;

const out = targets.map(({ os, cpu }) => {
  const { release, supportFiles } = videoConverterRelease({ os, cpu } as never);
  return {
    platform: os,
    architecture: cpu,
    ffmpeg: {
      tag: release.tag,
      assetName: release.assetName,
      url: release.url,
      expectedSizeBytes: release.expectedSizeBytes,
      sha256: release.sha256,
      format: release.format,
      stripComponents: release.stripComponents,
      supportFiles: supportFiles.map((file) => ({
        filename: file.filename,
        sha256: file.sha256,
        derivation: "copy",
        sources: [{ url: file.url, expectedSizeBytes: file.expectedSizeBytes, sha256: file.sha256 }],
      })),
    },
  };
});
console.log(JSON.stringify(out, null, 2));
