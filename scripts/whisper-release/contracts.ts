import { z } from "zod";
import { safeFilenameSchema, sha256Schema } from "../../src/utils/checksum";

export const whisperTagSchema = z
  .string()
  .regex(/^whisper-v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/);
export const repositorySchema = z
  .string()
  .regex(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/);
export const filePathSchema = z.string().min(1);
export const whisperTargetSchema = z.enum(["linux-x64", "macos-arm64"]);
export const teamIdSchema = z.string().regex(/^[A-Z0-9]{10}$/);
export const archiveFilenameSchema = z.enum([
  "whisper-server-linux-x64.tar.gz",
  "whisper-server-macos-arm64.zip",
]);

export type WhisperTarget = z.infer<typeof whisperTargetSchema>;
export type Fetcher = typeof fetch;

export const archiveSpecification: Record<
  WhisperTarget,
  {
    assetName: z.infer<typeof archiveFilenameSchema>;
    format: "tar.gz" | "zip";
    platform: "darwin" | "linux";
    architecture: "arm64" | "x64";
  }
> = {
  "linux-x64": {
    assetName: "whisper-server-linux-x64.tar.gz",
    format: "tar.gz",
    platform: "linux",
    architecture: "x64",
  },
  "macos-arm64": {
    assetName: "whisper-server-macos-arm64.zip",
    format: "zip",
    platform: "darwin",
    architecture: "arm64",
  },
};

const releaseReceiptEntrySchema = z
  .object({
    platform: z.enum(["darwin", "linux"]),
    architecture: z.enum(["arm64", "x64"]),
    tag: whisperTagSchema,
    assetName: archiveFilenameSchema,
    url: z.string().url(),
    expectedSizeBytes: z.number().int().positive(),
    sha256: sha256Schema,
    format: z.enum(["tar.gz", "zip"]),
    stripComponents: z.literal(0),
  })
  .strict();

export const whisperReleaseReceiptSchema = z
  .object({
    version: z.literal(1),
    repository: repositorySchema,
    tag: whisperTagSchema,
    releaseId: z.number().int().positive(),
    runtimes: z
      .object({
        "macos-arm64": releaseReceiptEntrySchema,
        "linux-x64": releaseReceiptEntrySchema,
      })
      .strict(),
  })
  .strict()
  .superRefine((receipt, context) => {
    for (const target of whisperTargetSchema.options) {
      const specification = archiveSpecification[target];
      const runtime = receipt.runtimes[target];
      if (
        runtime.platform !== specification.platform ||
        runtime.architecture !== specification.architecture ||
        runtime.assetName !== specification.assetName ||
        runtime.format !== specification.format ||
        runtime.tag !== receipt.tag
      ) {
        context.addIssue({
          code: "custom",
          path: ["runtimes", target],
          message: "does not match the canonical Whisper runtime target",
        });
      }
    }
  });

export type WhisperReleaseReceipt = z.infer<typeof whisperReleaseReceiptSchema>;

export function validateWhisperReleaseTag(tag: unknown): string {
  return whisperTagSchema.parse(tag);
}

export function parseJson(input: string, label: string): unknown {
  try {
    return JSON.parse(input);
  } catch (error) {
    throw new Error(`Invalid ${label}: malformed JSON.`, { cause: error });
  }
}

export function issueSummary(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "value"}: ${issue.message}`)
    .join("; ");
}

export function expectedDownloadUrl(
  repository: string,
  tag: string,
  assetName: string,
): string {
  return `https://github.com/${repository}/releases/download/${tag}/${assetName}`;
}

export function whisperRuntimeEntry(
  receipt: WhisperReleaseReceipt,
  target: WhisperTarget,
) {
  const runtime = receipt.runtimes[target];
  return {
    tag: runtime.tag,
    assetName: runtime.assetName,
    url: runtime.url,
    expectedSizeBytes: runtime.expectedSizeBytes,
    sha256: runtime.sha256,
    format: runtime.format,
    stripComponents: runtime.stripComponents,
  };
}

export { safeFilenameSchema };
