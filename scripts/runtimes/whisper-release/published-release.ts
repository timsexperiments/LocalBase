import { z } from "zod";
import { parseChecksumFile } from "../../src/utils/checksum";
import {
  archiveSpecification,
  expectedDownloadUrl,
  filePathSchema,
  issueSummary,
  repositorySchema,
  safeFilenameSchema,
  validateWhisperReleaseTag,
  whisperReleaseReceiptSchema,
  whisperTargetSchema,
  type Fetcher,
  type WhisperReleaseReceipt,
} from "./contracts";

const expectedAssetNames = [
  "checksums.txt",
  "whisper-server-linux-x64.tar.gz",
  "whisper-server-macos-arm64.zip",
] as const;

const githubAssetSchema = z
  .object({
    name: safeFilenameSchema,
    size: z.number().int().positive(),
    digest: z.string().regex(/^sha256:[a-fA-F0-9]{64}$/),
    browser_download_url: z.string().url(),
  })
  .passthrough();

const githubReleaseSchema = z
  .object({
    id: z.number().int().positive(),
    tag_name: z.string(),
    draft: z.boolean(),
    prerelease: z.boolean(),
    assets: z.array(githubAssetSchema),
  })
  .passthrough()
  .superRefine((release, context) => {
    const names = release.assets.map((asset) => asset.name).sort();
    const expected = [...expectedAssetNames].sort();
    if (
      names.length !== expected.length ||
      names.some((name, index) => name !== expected[index])
    ) {
      context.addIssue({
        code: "custom",
        path: ["assets"],
        message:
          "must contain exactly checksums.txt and the two canonical Whisper archives",
      });
    }
  });

function apiBaseUrl(): string {
  return z
    .string()
    .url()
    .parse(process.env.GITHUB_API_URL ?? "https://api.github.com");
}

function githubHeaders(): HeadersInit {
  const token = process.env.GH_TOKEN ?? process.env.GITHUB_TOKEN;
  return {
    Accept: "application/vnd.github+json",
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  };
}

async function githubJson(
  fetcher: Fetcher,
  url: string,
  description: string,
): Promise<unknown> {
  const response = await fetcher(url, { headers: githubHeaders() });
  if (!response.ok) {
    throw new Error(
      `GitHub ${description} request failed: HTTP ${response.status} ${response.statusText}.`,
    );
  }
  try {
    return await response.json();
  } catch (error) {
    throw new Error(`GitHub ${description} response is not JSON.`, {
      cause: error,
    });
  }
}

export async function assertWhisperReleaseAvailable(
  repositoryInput: unknown,
  tagInput: unknown,
  fetcher: Fetcher = fetch,
): Promise<void> {
  const repository = repositorySchema.parse(repositoryInput);
  const tag = validateWhisperReleaseTag(tagInput);
  const baseUrl = apiBaseUrl();
  const urls = [
    `${baseUrl}/repos/${repository}/releases/tags/${encodeURIComponent(tag)}`,
    `${baseUrl}/repos/${repository}/git/ref/tags/${encodeURIComponent(tag)}`,
  ];
  for (const url of urls) {
    const response = await fetcher(url, { headers: githubHeaders() });
    if (response.status === 404) continue;
    if (!response.ok) {
      throw new Error(
        `GitHub immutable release check failed: HTTP ${response.status} ${response.statusText}.`,
      );
    }
    throw new Error(
      `Whisper release tag ${tag} already exists and cannot be republished.`,
    );
  }
}

function parsePublishedRelease(input: unknown, tag: string) {
  const parsed = githubReleaseSchema.safeParse(input);
  if (!parsed.success) {
    throw new Error(
      `Invalid GitHub release response: ${issueSummary(parsed.error)}.`,
    );
  }
  if (parsed.data.tag_name !== tag) {
    throw new Error(
      `GitHub release tag mismatch: expected ${tag}, received ${parsed.data.tag_name}.`,
    );
  }
  if (parsed.data.draft || parsed.data.prerelease) {
    throw new Error(
      `GitHub release ${tag} must be a published non-draft, non-prerelease release.`,
    );
  }
  return parsed.data;
}

async function downloadVerifiedAsset(
  asset: z.infer<typeof githubAssetSchema>,
  expectedDigest: string,
  fetcher: Fetcher,
): Promise<void> {
  const response = await fetcher(asset.browser_download_url, {
    headers: githubHeaders(),
  });
  if (!response.ok) {
    throw new Error(
      `Failed to download ${asset.name}: HTTP ${response.status} ${response.statusText}.`,
    );
  }
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength !== asset.size) {
    throw new Error(
      `GitHub size mismatch for ${asset.name}: expected ${asset.size} bytes, received ${bytes.byteLength}.`,
    );
  }
  const digest = new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
  const githubDigest = asset.digest.slice("sha256:".length).toLowerCase();
  if (digest !== expectedDigest || digest !== githubDigest) {
    throw new Error(`Checksum mismatch for published asset ${asset.name}.`);
  }
}

export async function verifyPublishedWhisperRelease(
  repositoryInput: unknown,
  tagInput: unknown,
  outputPathInput: unknown,
  fetcher: Fetcher = fetch,
): Promise<WhisperReleaseReceipt> {
  const repository = repositorySchema.parse(repositoryInput);
  const tag = validateWhisperReleaseTag(tagInput);
  const outputPath = filePathSchema.parse(outputPathInput);
  const baseUrl = apiBaseUrl();
  const release = parsePublishedRelease(
    await githubJson(
      fetcher,
      `${baseUrl}/repos/${repository}/releases/tags/${encodeURIComponent(tag)}`,
      `release ${tag}`,
    ),
    tag,
  );
  const assets = new Map(release.assets.map((asset) => [asset.name, asset]));
  const checksumAsset = assets.get("checksums.txt");
  if (!checksumAsset) {
    throw new Error("Published release is missing checksums.txt.");
  }
  if (
    checksumAsset.browser_download_url !==
    expectedDownloadUrl(repository, tag, checksumAsset.name)
  ) {
    throw new Error("Published checksums.txt URL is not canonical.");
  }
  const checksumsResponse = await fetcher(checksumAsset.browser_download_url, {
    headers: githubHeaders(),
  });
  if (!checksumsResponse.ok) {
    throw new Error(
      `Failed to download checksums.txt: HTTP ${checksumsResponse.status} ${checksumsResponse.statusText}.`,
    );
  }
  const checksumBytes = new Uint8Array(await checksumsResponse.arrayBuffer());
  if (checksumBytes.byteLength !== checksumAsset.size) {
    throw new Error("GitHub size mismatch for checksums.txt.");
  }
  const checksumDigest = new Bun.CryptoHasher("sha256")
    .update(checksumBytes)
    .digest("hex");
  if (
    checksumDigest !==
    checksumAsset.digest.slice("sha256:".length).toLowerCase()
  ) {
    throw new Error("Checksum mismatch for published checksums.txt.");
  }
  const checksums = parseChecksumFile(new TextDecoder().decode(checksumBytes));
  const expectedArchives = whisperTargetSchema.options.map(
    (target) => archiveSpecification[target].assetName,
  );
  if (
    checksums.size !== expectedArchives.length ||
    expectedArchives.some((name) => !checksums.has(name))
  ) {
    throw new Error(
      "checksums.txt must contain exactly the two canonical Whisper archive checksums.",
    );
  }

  const runtimeEntries = await Promise.all(
    whisperTargetSchema.options.map(async (target) => {
      const specification = archiveSpecification[target];
      const asset = assets.get(specification.assetName);
      if (!asset) {
        throw new Error(
          `Published release is missing ${specification.assetName}.`,
        );
      }
      if (
        asset.browser_download_url !==
        expectedDownloadUrl(repository, tag, specification.assetName)
      ) {
        throw new Error(
          `Published URL for ${specification.assetName} is not canonical.`,
        );
      }
      const digest = checksums.get(specification.assetName)!;
      await downloadVerifiedAsset(asset, digest, fetcher);
      return [
        target,
        {
          platform: specification.platform,
          architecture: specification.architecture,
          tag,
          assetName: specification.assetName,
          url: asset.browser_download_url,
          expectedSizeBytes: asset.size,
          sha256: digest,
          format: specification.format,
          stripComponents: 0,
        },
      ] as const;
    }),
  );
  const receipt = whisperReleaseReceiptSchema.parse({
    version: 1,
    repository,
    tag,
    releaseId: release.id,
    runtimes: Object.fromEntries(runtimeEntries),
  });
  await Bun.write(outputPath, `${JSON.stringify(receipt, null, 2)}\n`);
  return receipt;
}
