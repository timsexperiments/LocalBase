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

type MutableManifest = {
  targets: Array<{
    platform: string;
    architecture: string;
    runtimes: Record<string, Record<string, unknown>>;
  }>;
};

function macRuntimes(): [
  MutableManifest,
  MutableManifest["targets"][number]["runtimes"],
] {
  const manifest = structuredClone(rawManifest) as MutableManifest;
  const target = manifest.targets.find(
    (candidate) =>
      candidate.platform === "darwin" && candidate.architecture === "arm64",
  );
  if (!target) throw new Error("Expected a macOS arm64 manifest target.");
  return [manifest, target.runtimes];
}

test("placeholder digests and pendingPublication must be declared together", () => {
  const [unmarked, unmarkedRuntimes] = macRuntimes();
  unmarkedRuntimes["kokoro-tts"]!.sha256 = "0".repeat(64);
  unmarkedRuntimes["kokoro-tts"]!.inventorySha256 = "0".repeat(64);
  delete unmarkedRuntimes["kokoro-tts"]!.pendingPublication;
  expect(() => parseManagedRuntimeManifest(unmarked)).toThrow(
    "pendingPublication must be declared together",
  );

  const [marked, markedRuntimes] = macRuntimes();
  markedRuntimes["kokoro-tts"]!.sha256 = "a".repeat(64);
  markedRuntimes["kokoro-tts"]!.inventorySha256 = "b".repeat(64);
  markedRuntimes["kokoro-tts"]!.pendingPublication = true;
  expect(() => parseManagedRuntimeManifest(marked)).toThrow(
    "pendingPublication must be declared together",
  );

  const [published, publishedRuntimes] = macRuntimes();
  publishedRuntimes["kokoro-tts"] = {
    ...publishedRuntimes["kokoro-tts"],
    sha256: "a".repeat(64),
    inventorySha256: "b".repeat(64),
    expectedSizeBytes: 123,
  };
  delete publishedRuntimes["kokoro-tts"].pendingPublication;
  expect(() => parseManagedRuntimeManifest(published)).not.toThrow();

  const [native, nativeRuntimes] = macRuntimes();
  nativeRuntimes["llama-server"]!.pendingPublication = true;
  nativeRuntimes["llama-server"]!.sha256 = "0".repeat(64);
  expect(() => parseManagedRuntimeManifest(native)).toThrow(
    "only packaged runtimes may await publication",
  );
});

test("the packaged Kokoro runtime must pin an inventory digest and ship as tar.gz", () => {
  const [missing, missingRuntimes] = macRuntimes();
  delete missingRuntimes["kokoro-tts"]!.inventorySha256;
  missingRuntimes["kokoro-tts"]!.sha256 = "a".repeat(64);
  delete missingRuntimes["kokoro-tts"]!.pendingPublication;
  expect(() => parseManagedRuntimeManifest(missing)).toThrow(
    "packaged runtimes must pin their inventory digest",
  );

  const [zip, zipRuntimes] = macRuntimes();
  zipRuntimes["kokoro-tts"]!.format = "zip";
  expect(() => parseManagedRuntimeManifest(zip)).toThrow(
    "packaged runtimes must ship as tar.gz archives",
  );

  const [native, nativeRuntimes] = macRuntimes();
  nativeRuntimes["llama-server"]!.inventorySha256 = "b".repeat(64);
  expect(() => parseManagedRuntimeManifest(native)).toThrow(
    "only packaged runtimes may pin an inventory digest",
  );
});
