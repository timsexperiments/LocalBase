import { z } from "zod";
import { safeFilenameSchema, sha256Schema } from "../../src/utils/checksum";

export const ttsReleaseTagSchema = z
  .string()
  .regex(/^tts-server-v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/);
export const repositorySchema = z
  .string()
  .regex(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/);
export const filePathSchema = z.string().min(1);
export const ttsPublicationTargetSchema = z.enum(["linux-x64", "macos-arm64"]);
export type TtsPublicationTarget = z.infer<typeof ttsPublicationTargetSchema>;
export type Fetcher = typeof fetch;

export const LLAMA_CPP_REVISION = "aee56b3abfeb134218fe5c496c329a1faac1ef28";
export const LLAMA_CPP_SHA256 =
  "43ed9540769b26a2f180e60d0f14ffd2eaef7f844ab12b40f5b7b6fa37a2c387";

const localSourceSchema = (name: string) =>
  z.object({ name: z.literal(name), sha256: sha256Schema }).strict();

/** SOURCE.tts-server.json, written by scripts/build-tts-server.ts. */
export const ttsSourceProvenanceSchema = z
  .object({
    version: z.literal(1),
    sources: z.tuple([
      z
        .object({
          name: z.literal("llama.cpp"),
          tag: z.literal("b10419"),
          revision: z.literal(LLAMA_CPP_REVISION),
          sha256: z.literal(LLAMA_CPP_SHA256),
          url: z.string().url(),
        })
        .strict(),
    ]),
    localSources: z.tuple([
      localSourceSchema("CMakeLists.txt"),
      localSourceSchema("tts-server.cpp"),
    ]),
    runtimeRequirements: z
      .object({
        "linux-x64": z
          .object({
            buildBaseline: z.literal("Ubuntu 24.04 x86_64"),
            gpu: z.literal("Vulkan loader and a compatible Vulkan GPU driver"),
            systemLibraries: z.literal(
              "glibc and libstdc++ compatible with the Ubuntu 24.04 build baseline",
            ),
          })
          .strict(),
        "macos-arm64": z
          .object({
            buildBaseline: z.literal("macOS 14 arm64"),
            gpu: z.literal("Apple Metal"),
            systemLibraries: z.literal("macOS system frameworks"),
          })
          .strict(),
      })
      .strict(),
  })
  .strict();

export const sourceIdentitySchema = z
  .object({
    manifestSha256: sha256Schema,
    llamaCppRevision: z.literal(LLAMA_CPP_REVISION),
    localSources: ttsSourceProvenanceSchema.shape.localSources,
  })
  .strict();

export const archiveSpecification = {
  "linux-x64": {
    assetName: "tts-server-linux-x64.tar.gz",
    format: "tar.gz" as const,
    platform: "linux" as const,
    architecture: "x64" as const,
  },
  "macos-arm64": {
    assetName: "tts-server-macos-arm64.zip",
    format: "zip" as const,
    platform: "darwin" as const,
    architecture: "arm64" as const,
  },
} satisfies Record<TtsPublicationTarget, object>;

const runtimeReceiptSchema = z
  .object({
    platform: z.enum(["darwin", "linux"]),
    architecture: z.enum(["arm64", "x64"]),
    tag: ttsReleaseTagSchema,
    assetName: z.enum([
      "tts-server-linux-x64.tar.gz",
      "tts-server-macos-arm64.zip",
    ]),
    url: z.string().url(),
    expectedSizeBytes: z.number().int().positive(),
    sha256: sha256Schema,
    format: z.enum(["tar.gz", "zip"]),
    stripComponents: z.literal(0),
  })
  .strict();

export const ttsReleaseReceiptSchema = z
  .object({
    version: z.literal(1),
    repository: repositorySchema,
    tag: ttsReleaseTagSchema,
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
    for (const target of ttsPublicationTargetSchema.options) {
      const expected = archiveSpecification[target];
      const runtime = receipt.runtimes[target];
      if (
        runtime.platform !== expected.platform ||
        runtime.architecture !== expected.architecture ||
        runtime.assetName !== expected.assetName ||
        runtime.format !== expected.format ||
        runtime.tag !== receipt.tag ||
        runtime.url !==
          expectedDownloadUrl(
            receipt.repository,
            receipt.tag,
            runtime.assetName,
          )
      ) {
        context.addIssue({
          code: "custom",
          path: ["runtimes", target],
          message: "does not match the canonical tts-server runtime target",
        });
      }
    }
  });

export type TtsReleaseReceipt = z.infer<typeof ttsReleaseReceiptSchema>;

export function validateTtsReleaseTag(tag: unknown): string {
  return ttsReleaseTagSchema.parse(tag);
}

export function expectedDownloadUrl(
  repository: string,
  tag: string,
  assetName: string,
): string {
  return `https://github.com/${repository}/releases/download/${tag}/${assetName}`;
}

/** The published manifest entry; omitting `pendingPublication` enables downloads. */
export function ttsRuntimeEntry(
  receipt: TtsReleaseReceipt,
  target: TtsPublicationTarget,
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
