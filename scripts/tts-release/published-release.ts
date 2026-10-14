import { z } from "zod";
import { parseChecksumFile } from "../../src/utils/checksum";
import { readSdArchiveEntries } from "../sd-release/archive-reader";
import {
  archiveSpecification,
  expectedDownloadUrl,
  filePathSchema,
  repositorySchema,
  safeFilenameSchema,
  sourceIdentitySchema,
  ttsPublicationTargetSchema,
  ttsReleaseReceiptSchema,
  ttsSourceProvenanceSchema,
  validateTtsReleaseTag,
  type Fetcher,
  type TtsReleaseReceipt,
} from "./contracts";

const expectedAssetNames = [
  "checksums.txt",
  "SOURCE.tts-server.json",
  "tts-server-linux-x64.tar.gz",
  "tts-server-macos-arm64.zip",
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
  .passthrough();

function headers(): HeadersInit {
  const token = process.env.GH_TOKEN ?? process.env.GITHUB_TOKEN;
  return {
    Accept: "application/vnd.github+json",
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  };
}

function apiBaseUrl(): string {
  return z
    .string()
    .url()
    .parse(process.env.GITHUB_API_URL ?? "https://api.github.com");
}

async function download(
  asset: z.infer<typeof githubAssetSchema>,
  fetcher: Fetcher,
): Promise<Uint8Array> {
  const response = await fetcher(asset.browser_download_url, {
    headers: headers(),
  });
  if (!response.ok)
    throw new Error(
      `Failed to download ${asset.name}: HTTP ${response.status}.`,
    );
  const bytes = new Uint8Array(await response.arrayBuffer());
  const digest = new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
  if (
    bytes.byteLength !== asset.size ||
    digest !== asset.digest.slice("sha256:".length).toLowerCase()
  ) {
    throw new Error(`Published asset metadata mismatch for ${asset.name}.`);
  }
  return bytes;
}

/** Fails when the tag already has a release or a git ref; tags are never moved. */
export async function assertTtsReleaseAvailable(
  repositoryInput: unknown,
  tagInput: unknown,
  fetcher: Fetcher = fetch,
): Promise<void> {
  const repository = repositorySchema.parse(repositoryInput);
  const tag = validateTtsReleaseTag(tagInput);
  for (const url of [
    `${apiBaseUrl()}/repos/${repository}/releases/tags/${encodeURIComponent(tag)}`,
    `${apiBaseUrl()}/repos/${repository}/git/ref/tags/${encodeURIComponent(tag)}`,
  ]) {
    const response = await fetcher(url, { headers: headers() });
    if (response.status === 404) continue;
    if (!response.ok)
      throw new Error(
        `GitHub immutable release check failed: HTTP ${response.status}.`,
      );
    throw new Error(
      `tts-server release tag ${tag} already exists and cannot be republished.`,
    );
  }
}

export async function verifyPublishedTtsRelease(
  repositoryInput: unknown,
  tagInput: unknown,
  outputPathInput: unknown,
  fetcher: Fetcher = fetch,
): Promise<TtsReleaseReceipt> {
  const repository = repositorySchema.parse(repositoryInput);
  const tag = validateTtsReleaseTag(tagInput);
  const outputPath = filePathSchema.parse(outputPathInput);
  const response = await fetcher(
    `${apiBaseUrl()}/repos/${repository}/releases/tags/${encodeURIComponent(tag)}`,
    { headers: headers() },
  );
  if (!response.ok)
    throw new Error(`GitHub release request failed: HTTP ${response.status}.`);
  const release = githubReleaseSchema.parse(await response.json());
  if (release.tag_name !== tag || release.draft || release.prerelease) {
    throw new Error(
      `GitHub release ${tag} is not the expected published release.`,
    );
  }
  const names = release.assets.map(({ name }) => name).sort();
  if (
    JSON.stringify(names) !== JSON.stringify([...expectedAssetNames].sort())
  ) {
    throw new Error(
      "Published release must contain exactly the canonical tts-server assets.",
    );
  }
  const assets = new Map(release.assets.map((asset) => [asset.name, asset]));
  for (const asset of release.assets) {
    if (
      asset.browser_download_url !==
      expectedDownloadUrl(repository, tag, asset.name)
    ) {
      throw new Error(`Published URL for ${asset.name} is not canonical.`);
    }
  }

  const checksumBytes = await download(assets.get("checksums.txt")!, fetcher);
  const checksums = parseChecksumFile(new TextDecoder().decode(checksumBytes));
  const checkedNames = ttsPublicationTargetSchema.options.map(
    (target) => archiveSpecification[target].assetName,
  );
  if (
    checksums.size !== checkedNames.length ||
    checkedNames.some((name) => !checksums.has(name))
  ) {
    throw new Error(
      "checksums.txt must contain exactly the two canonical tts-server archives.",
    );
  }

  const sourceBytes = await download(
    assets.get("SOURCE.tts-server.json")!,
    fetcher,
  );
  const provenance = ttsSourceProvenanceSchema.parse(
    JSON.parse(new TextDecoder().decode(sourceBytes)),
  );

  const runtimeEntries = await Promise.all(
    ttsPublicationTargetSchema.options.map(async (target) => {
      const specification = archiveSpecification[target];
      const asset = assets.get(specification.assetName)!;
      const bytes = await download(asset, fetcher);
      const digest = new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
      if (digest !== checksums.get(specification.assetName)) {
        throw new Error(`Checksum mismatch for ${specification.assetName}.`);
      }
      const embeddedSource = (await readSdArchiveEntries(target, bytes)).find(
        ({ name }) => name === "SOURCE.tts-server.json",
      )?.bytes;
      if (
        !embeddedSource ||
        !Buffer.from(embeddedSource).equals(Buffer.from(sourceBytes))
      ) {
        throw new Error(
          `${specification.assetName} source identity does not match the published SOURCE.tts-server.json.`,
        );
      }
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
  const source = sourceIdentitySchema.parse({
    manifestSha256: new Bun.CryptoHasher("sha256")
      .update(sourceBytes)
      .digest("hex"),
    llamaCppRevision: provenance.sources[0].revision,
    localSources: provenance.localSources,
  });
  const receipt = ttsReleaseReceiptSchema.parse({
    version: 1,
    repository,
    tag,
    releaseId: release.id,
    source,
    runtimes: Object.fromEntries(runtimeEntries),
  });
  await Bun.write(outputPath, `${JSON.stringify(receipt, null, 2)}\n`);
  return receipt;
}
