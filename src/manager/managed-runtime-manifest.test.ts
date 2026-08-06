import { expect, test } from "bun:test";
import rawManifest from "./managed-runtime-manifest.json";
import { parseManagedRuntimeManifest } from "./managed-runtime-manifest";

test("requires CLI-only targets to have no LocalBase-managed runtimes", () => {
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
  target.runtimes["llama-server"] = {
    tag: "test",
    assetName: "llama-server-macos-x64",
    url: "https://example.test/llama-server-macos-x64",
    expectedSizeBytes: 1,
    sha256: "0".repeat(64),
    format: "binary",
    stripComponents: 0,
  };

  expect(() => parseManagedRuntimeManifest(manifest)).toThrow(
    "CLI-only targets cannot define LocalBase-managed runtimes",
  );
});

test("requires explicit extraction metadata for every managed runtime", () => {
  const manifest = structuredClone(rawManifest) as {
    targets: Array<{
      platform: string;
      architecture: string;
      runtimes: Record<string, { stripComponents?: number }>;
    }>;
  };
  const target = manifest.targets.find(
    (candidate) =>
      candidate.platform === "darwin" && candidate.architecture === "arm64",
  );
  const runtime = target?.runtimes["whisper-server"];
  if (!runtime) throw new Error("Expected a managed Whisper runtime.");
  delete runtime.stripComponents;

  expect(() => parseManagedRuntimeManifest(manifest)).toThrow(
    "stripComponents",
  );
});
