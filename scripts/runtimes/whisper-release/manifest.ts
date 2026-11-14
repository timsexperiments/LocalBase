import {
  managedRuntimeManifestSchema,
  parseManagedRuntimeManifest,
} from "../../src/manager/managed-runtime-manifest";
import {
  filePathSchema,
  parseJson,
  whisperReleaseReceiptSchema,
  whisperRuntimeEntry,
  whisperTargetSchema,
  type WhisperReleaseReceipt,
} from "./contracts";
import { z } from "zod";

type RuntimeManifest = z.infer<typeof managedRuntimeManifestSchema>;

function manifestTarget(
  manifest: RuntimeManifest,
  target: "linux-x64" | "macos-arm64",
) {
  const expected =
    target === "linux-x64"
      ? { platform: "linux", architecture: "x64" }
      : { platform: "darwin", architecture: "arm64" };
  const entry = manifest.targets.find(
    (candidate) =>
      candidate.platform === expected.platform &&
      candidate.architecture === expected.architecture,
  );
  if (!entry || entry.tier !== "managed") {
    throw new Error(`Managed manifest target ${target} is missing.`);
  }
  return entry;
}

function equalWhisperEntry(
  actual: unknown,
  expected: ReturnType<typeof whisperRuntimeEntry>,
): boolean {
  return JSON.stringify(actual) === JSON.stringify(expected);
}

export function updateWhisperManifest(
  manifestInput: unknown,
  receiptInput: unknown,
): RuntimeManifest {
  const manifest = parseManagedRuntimeManifest(manifestInput);
  const receipt = whisperReleaseReceiptSchema.parse(receiptInput);
  const updated = structuredClone(manifest);
  for (const target of whisperTargetSchema.options) {
    manifestTarget(updated, target).runtimes["whisper-server"] =
      whisperRuntimeEntry(receipt, target);
  }
  return parseManagedRuntimeManifest(updated);
}

export function whisperManifestMatches(
  manifestInput: unknown,
  receiptInput: unknown,
): boolean {
  const manifest = parseManagedRuntimeManifest(manifestInput);
  const receipt = whisperReleaseReceiptSchema.parse(receiptInput);
  return whisperTargetSchema.options.every((target) =>
    equalWhisperEntry(
      manifestTarget(manifest, target).runtimes["whisper-server"],
      whisperRuntimeEntry(receipt, target),
    ),
  );
}

export function verifyWhisperManifest(
  manifestInput: unknown,
  receiptInput: unknown,
): void {
  if (!whisperManifestMatches(manifestInput, receiptInput)) {
    throw new Error(
      "Managed Whisper manifest entries do not match the release receipt.",
    );
  }
}

export async function readWhisperReleaseReceipt(
  pathInput: unknown,
): Promise<WhisperReleaseReceipt> {
  const path = filePathSchema.parse(pathInput);
  const file = Bun.file(path);
  if (!(await file.exists())) {
    throw new Error(`Release receipt does not exist: ${path}.`);
  }
  return whisperReleaseReceiptSchema.parse(
    parseJson(await file.text(), "Whisper release receipt"),
  );
}

export async function updateWhisperManifestFile(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<void> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  const manifestFile = Bun.file(manifestPath);
  if (!(await manifestFile.exists())) {
    throw new Error(`Manifest does not exist: ${manifestPath}.`);
  }
  const updated = updateWhisperManifest(
    parseJson(await manifestFile.text(), "managed runtime manifest"),
    await readWhisperReleaseReceipt(receiptPathInput),
  );
  await Bun.write(manifestPath, `${JSON.stringify(updated, null, 2)}\n`);
}

export async function verifyWhisperManifestFile(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<void> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  verifyWhisperManifest(
    parseJson(await Bun.file(manifestPath).text(), "managed runtime manifest"),
    await readWhisperReleaseReceipt(receiptPathInput),
  );
}

export async function whisperManifestStatus(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<boolean> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  return whisperManifestMatches(
    parseJson(await Bun.file(manifestPath).text(), "managed runtime manifest"),
    await readWhisperReleaseReceipt(receiptPathInput),
  );
}
