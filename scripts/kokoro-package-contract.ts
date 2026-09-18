import { join, resolve } from "node:path";
import { z } from "zod";
import { sha256Schema } from "../src/utils/checksum";
import { verifyInventory } from "../runtimes/kokoro/inventory";

export const kokoroTagSchema = z.literal("kokoro-v0.0.1");
export const kokoroTargetSchema = z.enum(["macos-arm64", "linux-x64"]);
export type KokoroTarget = z.infer<typeof kokoroTargetSchema>;
export const archiveName = (target: KokoroTarget) =>
  `kokoro-tts-${target}.tar.gz`;

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
