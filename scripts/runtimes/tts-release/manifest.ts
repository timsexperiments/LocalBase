import { z } from "zod";
import {
  managedRuntimeManifestSchema,
  parseManagedRuntimeManifest,
} from "../../src/manager/managed-runtime-manifest";
import {
  filePathSchema,
  ttsPublicationTargetSchema,
  ttsReleaseReceiptSchema,
  ttsRuntimeEntry,
  type TtsReleaseReceipt,
} from "./contracts";

type RuntimeManifest = z.infer<typeof managedRuntimeManifestSchema>;

function targetEntry(
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

/** Replaces only the tts-server pins, which also clears `pendingPublication`. */
export function updateTtsManifest(
  manifestInput: unknown,
  receiptInput: unknown,
): RuntimeManifest {
  const updated = structuredClone(parseManagedRuntimeManifest(manifestInput));
  const receipt = ttsReleaseReceiptSchema.parse(receiptInput);
  for (const target of ttsPublicationTargetSchema.options) {
    targetEntry(updated, target).runtimes["tts-server"] = ttsRuntimeEntry(
      receipt,
      target,
    );
  }
  return parseManagedRuntimeManifest(updated);
}

export function ttsManifestMatches(
  manifestInput: unknown,
  receiptInput: unknown,
): boolean {
  const manifest = parseManagedRuntimeManifest(manifestInput);
  const receipt = ttsReleaseReceiptSchema.parse(receiptInput);
  return ttsPublicationTargetSchema.options.every(
    (target) =>
      JSON.stringify(targetEntry(manifest, target).runtimes["tts-server"]) ===
      JSON.stringify(ttsRuntimeEntry(receipt, target)),
  );
}

async function readJson(path: string, label: string): Promise<unknown> {
  const file = Bun.file(path);
  if (!(await file.exists()))
    throw new Error(`${label} does not exist: ${path}.`);
  try {
    return JSON.parse(await file.text());
  } catch (error) {
    throw new Error(`Invalid ${label}: malformed JSON.`, { cause: error });
  }
}

async function readReceipt(pathInput: unknown): Promise<TtsReleaseReceipt> {
  const path = filePathSchema.parse(pathInput);
  return ttsReleaseReceiptSchema.parse(
    await readJson(path, "tts-server release receipt"),
  );
}

export async function updateTtsManifestFile(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<void> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  const updated = updateTtsManifest(
    await readJson(manifestPath, "managed runtime manifest"),
    await readReceipt(receiptPathInput),
  );
  await Bun.write(manifestPath, `${JSON.stringify(updated, null, 2)}\n`);
}

export async function verifyTtsManifestFile(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<void> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  if (
    !ttsManifestMatches(
      await readJson(manifestPath, "managed runtime manifest"),
      await readReceipt(receiptPathInput),
    )
  ) {
    throw new Error(
      "Managed tts-server manifest entries do not match the release receipt.",
    );
  }
}
