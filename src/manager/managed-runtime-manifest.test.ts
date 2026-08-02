import { expect, test } from "bun:test";
import rawManifest from "./managed-runtime-manifest.json";
import { parseManagedRuntimeManifest } from "./managed-runtime-manifest";

test("rejects a manifest that assigns LocalBase-managed runtimes to a CLI-only target", () => {
  const manifest = structuredClone(rawManifest) as {
    targets: Array<{
      platform: string;
      architecture: string;
      runtimes: Record<string, unknown>;
    }>;
  };
  const target = manifest.targets.find(
    (candidate) =>
      candidate.platform === "darwin" && candidate.architecture === "x64",
  );
  if (!target) throw new Error("Expected a macOS x64 manifest target.");
  target.runtimes["whisper-server"] = {
    tag: "test",
    assetName: "whisper-server-macos-x64",
    url: "https://example.test/whisper-server-macos-x64",
    expectedSizeBytes: 1,
    sha256: "0".repeat(64),
    format: "binary",
  };

  expect(() => parseManagedRuntimeManifest(manifest)).toThrow(
    "CLI-only targets cannot define LocalBase-managed runtimes",
  );
});
