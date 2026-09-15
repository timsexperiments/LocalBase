import { z } from "zod";
import { safeFilenameSchema, sha256Schema } from "../../src/utils/checksum";

export const sdReleaseTagSchema = z
  .string()
  .regex(/^sd-server-v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/);
export const repositorySchema = z
  .string()
  .regex(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/);
export const filePathSchema = z.string().min(1);
export const sdPublicationTargetSchema = z.enum(["linux-x64", "macos-arm64"]);
export type SdPublicationTarget = z.infer<typeof sdPublicationTargetSchema>;
export type Fetcher = typeof fetch;

export const sourceIdentitySchema = z
  .object({
    manifestSha256: sha256Schema,
    stableDiffusionRevision: z.literal(
      "07a85c74cb08cda3aa176f688c5d8f522615e2b9",
    ),
    patchSha256: sha256Schema,
  })
  .strict();

export const archiveSpecification = {
  "linux-x64": {
    assetName: "sd-server-linux-x64.tar.gz",
    format: "tar.gz" as const,
    platform: "linux" as const,
    architecture: "x64" as const,
  },
  "macos-arm64": {
    assetName: "sd-server-macos-arm64.zip",
    format: "zip" as const,
    platform: "darwin" as const,
    architecture: "arm64" as const,
  },
} satisfies Record<SdPublicationTarget, object>;

const runtimeReceiptSchema = z
  .object({
    platform: z.enum(["darwin", "linux"]),
    architecture: z.enum(["arm64", "x64"]),
    tag: sdReleaseTagSchema,
    assetName: z.enum([
      "sd-server-linux-x64.tar.gz",
      "sd-server-macos-arm64.zip",
    ]),
    url: z.string().url(),
    expectedSizeBytes: z.number().int().positive(),
    sha256: sha256Schema,
    format: z.enum(["tar.gz", "zip"]),
    stripComponents: z.literal(0),
  })
  .strict();

export const sdReleaseReceiptSchema = z
  .object({
    version: z.literal(1),
    repository: repositorySchema,
    tag: sdReleaseTagSchema,
    releaseId: z.number().int().positive(),
    source: sourceIdentitySchema,
    runtimes: z
      .object({
        "linux-x64": runtimeReceiptSchema,
        "macos-arm64": runtimeReceiptSchema,
      })
      .strict(),
  })
  .strict()
  .superRefine((receipt, context) => {
    for (const target of sdPublicationTargetSchema.options) {
      const expected = archiveSpecification[target];
      const runtime = receipt.runtimes[target];
      if (
        runtime.platform !== expected.platform ||
        runtime.architecture !== expected.architecture ||
        runtime.assetName !== expected.assetName ||
        runtime.format !== expected.format ||
        runtime.tag !== receipt.tag
      ) {
        context.addIssue({
          code: "custom",
          path: ["runtimes", target],
          message: "does not match the canonical sd-server runtime target",
        });
      }
    }
  });

export type SdReleaseReceipt = z.infer<typeof sdReleaseReceiptSchema>;

export function validateSdReleaseTag(tag: unknown): string {
  return sdReleaseTagSchema.parse(tag);
}

export function expectedDownloadUrl(
  repository: string,
  tag: string,
  assetName: string,
): string {
  return `https://github.com/${repository}/releases/download/${tag}/${assetName}`;
}

export function sdRuntimeEntry(
  receipt: SdReleaseReceipt,
  target: SdPublicationTarget,
) {
  const runtime = receipt.runtimes[target];
  return {
    tag: runtime.tag,
    assetName: runtime.assetName,
    url: runtime.url,
    expectedSizeBytes: runtime.expectedSizeBytes,
    sha256: runtime.sha256,
    format: runtime.format,
    stripComponents: 0 as const,
  };
}

export { safeFilenameSchema };
