import rawManifest from "./managed-runtime-manifest.json";
import { z } from "zod";
import { safeFilenameSchema, sha256Schema } from "../utils/checksum";

export const runtimeNameSchema = z.enum([
  "llama-server",
  "whisper-server",
  "sd-server",
]);
export type RuntimeName = z.infer<typeof runtimeNameSchema>;

const platformSchema = z.enum(["darwin", "linux"]);
const architectureSchema = z.enum(["arm64", "x64"]);
const platformSupportTierSchema = z.enum(["managed", "cli-only"]);

const managedRuntimeArtifactSchema = z
  .object({
    tag: z.string().min(1),
    assetName: safeFilenameSchema,
    url: z.string().url(),
    expectedSizeBytes: z.number().int().positive(),
    sha256: sha256Schema,
    format: z.enum(["binary", "tar.gz", "zip"]),
  })
  .strict();

const manifestTargetSchema = z
  .object({
    platform: platformSchema,
    architecture: architectureSchema,
    tier: platformSupportTierSchema,
    runtimes: z.partialRecord(runtimeNameSchema, managedRuntimeArtifactSchema),
  })
  .strict();

export const managedRuntimeManifestSchema = z
  .object({
    version: z.literal(1),
    targets: z.array(manifestTargetSchema).length(4),
  })
  .strict()
  .superRefine((manifest, ctx) => {
    const expectedTargets = new Map<
      string,
      z.infer<typeof platformSupportTierSchema>
    >([
      ["darwin:arm64", "managed"],
      ["darwin:x64", "cli-only"],
      ["linux:arm64", "cli-only"],
      ["linux:x64", "managed"],
    ]);
    const seenTargets = new Set<string>();

    for (const [index, target] of manifest.targets.entries()) {
      const key = `${target.platform}:${target.architecture}`;
      if (seenTargets.has(key)) {
        ctx.addIssue({
          code: "custom",
          message: "platform and architecture must be unique",
          path: ["targets", index],
        });
      }
      seenTargets.add(key);

      if (target.tier !== expectedTargets.get(key)) {
        ctx.addIssue({
          code: "custom",
          message: "platform support tier does not match the supported target",
          path: ["targets", index, "tier"],
        });
      }

      if (target.tier === "managed") {
        for (const name of runtimeNameSchema.options) {
          if (!target.runtimes[name]) {
            ctx.addIssue({
              code: "custom",
              message: "managed targets must define every managed runtime",
              path: ["targets", index, "runtimes", name],
            });
          }
        }
      } else {
        for (const name of ["whisper-server", "sd-server"] as const) {
          if (target.runtimes[name]) {
            ctx.addIssue({
              code: "custom",
              message:
                "CLI-only targets cannot define LocalBase-managed runtimes",
              path: ["targets", index, "runtimes", name],
            });
          }
        }
      }
    }

    for (const key of expectedTargets.keys()) {
      if (!seenTargets.has(key)) {
        ctx.addIssue({
          code: "custom",
          message: `missing supported target ${key}`,
          path: ["targets"],
        });
      }
    }
  });

export type PlatformTarget = { os: string; cpu: string };
export type PlatformSupportTier = "managed" | "cli-only" | "unsupported";
export type ManagedRuntimeRelease = z.infer<
  typeof managedRuntimeArtifactSchema
> & { name: RuntimeName };

export function parseManagedRuntimeManifest(input: unknown) {
  return managedRuntimeManifestSchema.parse(input);
}

const managedRuntimeManifest = parseManagedRuntimeManifest(rawManifest);

export function platformSupportTier(
  target: PlatformTarget,
): PlatformSupportTier {
  return (
    managedRuntimeManifest.targets.find(
      (candidate) =>
        candidate.platform === target.os &&
        candidate.architecture === target.cpu,
    )?.tier ?? "unsupported"
  );
}

export function managedRuntimeRelease(
  name: RuntimeName,
  target: PlatformTarget,
): ManagedRuntimeRelease | undefined {
  const candidate = managedRuntimeManifest.targets.find(
    (entry) =>
      entry.platform === target.os && entry.architecture === target.cpu,
  );
  const artifact = candidate?.runtimes[name];
  return artifact ? { name, ...artifact } : undefined;
}
