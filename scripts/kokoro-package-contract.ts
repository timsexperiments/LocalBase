import { lstat } from "node:fs/promises";
import { join, resolve } from "node:path";
import { z } from "zod";
import { sha256Schema } from "../src/utils/checksum";
import { verifyInventory } from "../runtimes/kokoro/inventory";

export const kokoroTagSchema = z.literal("kokoro-v0.0.1");
export const kokoroTargetSchema = z.enum(["macos-arm64", "linux-x64"]);
export type KokoroTarget = z.infer<typeof kokoroTargetSchema>;
export const archiveName = (target: KokoroTarget) =>
  `kokoro-tts-${target}.tar.gz`;

export const requiredKokoroLicenses = [
  "LICENSE.Apache-2.0.txt",
  "LICENSE.BSD-3-Clause.txt",
  "LICENSE.BlueOak-1.0.0.txt",
  "LICENSE.CC0-1.0.txt",
  "LICENSE.GPL-3.0-or-later.txt",
  "LICENSE.ISC.txt",
  "LICENSE.LGPL-2.1-only.txt",
  "LICENSE.LGPL-3.0-or-later.txt",
  "LICENSE.MIT.txt",
  "LICENSE.bun.md",
  "LICENSE.onnxruntime.txt",
  "ThirdPartyNotices.onnxruntime.txt",
] as const;

export async function verifyKokoroLicenses(directory: string): Promise<void> {
  for (const license of requiredKokoroLicenses) {
    const path = join(directory, license);
    const info = await lstat(path).catch(() => undefined);
    if (!info?.isFile() || info.size === 0)
      throw new Error(`Required Kokoro license text is missing: ${license}`);
  }
}

export async function verifyDependencyLicenses(
  directory: string,
  packages: readonly { name: string; license: unknown }[],
): Promise<void> {
  for (const pkg of packages) {
    if (typeof pkg.license !== "string" || !pkg.license.trim())
      throw new Error(`Dependency has no declared license: ${pkg.name}`);
    const identifiers = pkg.license.match(/[A-Za-z0-9.-]+/g) ?? [];
    for (const identifier of identifiers) {
      if (["AND", "OR", "WITH"].includes(identifier)) continue;
      const filename = `LICENSE.${identifier}.txt`;
      const path = join(directory, filename);
      const info = await lstat(path).catch(() => undefined);
      if (!info?.isFile() || info.size === 0)
        throw new Error(
          `Dependency license text is missing: ${pkg.name} (${identifier})`,
        );
    }
  }
}

export const kokoroArtifactSchema = z
  .object({
    target: kokoroTargetSchema,
    assetName: z.string(),
    expectedSizeBytes: z.number().int().positive(),
    sha256: sha256Schema,
    inventorySha256: sha256Schema,
    format: z.literal("tar.gz"),
    stripComponents: z.literal(0),
  })
  .strict()
  .superRefine((artifact, context) => {
    if (artifact.assetName !== archiveName(artifact.target)) {
      context.addIssue({
        code: "custom",
        path: ["assetName"],
        message: "Noncanonical Kokoro archive.",
      });
    }
  });
export type KokoroArtifact = z.infer<typeof kokoroArtifactSchema>;

export const kokoroReleaseManifestSchema = z
  .object({
    version: z.literal(1),
    tag: kokoroTagSchema,
    runtimes: z
      .object({
        "macos-arm64": kokoroArtifactSchema,
        "linux-x64": kokoroArtifactSchema,
      })
      .strict(),
  })
  .strict()
  .superRefine((manifest, context) => {
    for (const target of kokoroTargetSchema.options) {
      if (manifest.runtimes[target].target !== target) {
        context.addIssue({
          code: "custom",
          path: ["runtimes", target],
          message: "Kokoro target mismatch.",
        });
      }
    }
  });

/** Callers obtain inventorySha256 from the verified, pinned release manifest. */
export async function kokoroInvocation(input: {
  packageDirectory: string;
  inventorySha256: string;
  arguments: string[];
  privateDirectory: string;
}) {
  const root = resolve(input.packageDirectory);
  const privateDirectory = resolve(input.privateDirectory);
  await verifyInventory(root, sha256Schema.parse(input.inventorySha256));
  return {
    command: [
      join(root, "kokoro-tts"),
      "--no-install",
      "--no-env-file",
      "--config",
      join(root, "bunfig.toml"),
      join(root, "cli.js"),
      ...input.arguments,
      "--inventory-sha256",
      sha256Schema.parse(input.inventorySha256),
    ],
    cwd: privateDirectory,
    // No inherited hooks, module paths, dynamic-loader overrides or host tools.
    env: {
      PATH: join(privateDirectory, "empty-path"),
      HOME: privateDirectory,
      TMPDIR: privateDirectory,
      LANG: "C",
      LC_ALL: "C",
    },
  };
}
