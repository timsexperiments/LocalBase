import { z } from "zod";
import {
  managedRuntimeManifestSchema,
  parseManagedRuntimeManifest,
} from "../../src/manager/managed-runtime-manifest";
import {
  filePathSchema,
  sdPublicationTargetSchema,
  sdReleaseReceiptSchema,
  sdRuntimeEntry,
  type SdReleaseReceipt,
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

export function updateSdManifest(
  manifestInput: unknown,
  receiptInput: unknown,
): RuntimeManifest {
  const updated = structuredClone(parseManagedRuntimeManifest(manifestInput));
  const receipt = sdReleaseReceiptSchema.parse(receiptInput);
  for (const target of sdPublicationTargetSchema.options) {
    targetEntry(updated, target).runtimes["sd-server"] = sdRuntimeEntry(
      receipt,
      target,
    );
  }
  return parseManagedRuntimeManifest(updated);
}

export function sdManifestMatches(
  manifestInput: unknown,
  receiptInput: unknown,
): boolean {
  const manifest = parseManagedRuntimeManifest(manifestInput);
  const receipt = sdReleaseReceiptSchema.parse(receiptInput);
  return sdPublicationTargetSchema.options.every(
    (target) =>
      JSON.stringify(targetEntry(manifest, target).runtimes["sd-server"]) ===
      JSON.stringify(sdRuntimeEntry(receipt, target)),
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

async function readReceipt(pathInput: unknown): Promise<SdReleaseReceipt> {
  const path = filePathSchema.parse(pathInput);
  return sdReleaseReceiptSchema.parse(
    await readJson(path, "sd-server release receipt"),
  );
}

export async function updateSdManifestFile(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<void> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  const updated = updateSdManifest(
    await readJson(manifestPath, "managed runtime manifest"),
    await readReceipt(receiptPathInput),
  );
  await Bun.write(manifestPath, `${JSON.stringify(updated, null, 2)}\n`);
}

export async function verifySdManifestFile(
  manifestPathInput: unknown,
  receiptPathInput: unknown,
): Promise<void> {
  const manifestPath = filePathSchema.parse(manifestPathInput);
  if (
    !sdManifestMatches(
      await readJson(manifestPath, "managed runtime manifest"),
      await readReceipt(receiptPathInput),
    )
  ) {
    throw new Error(
      "Managed sd-server manifest entries do not match the release receipt.",
    );
  }
}
