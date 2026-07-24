import {
  accessSync,
  chmodSync,
  constants,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
} from "node:fs";
import { delimiter, dirname, join, resolve, sep } from "node:path";
import { Readable } from "node:stream";
import { unzip } from "fflate";
import { extract as createTarExtractor, type Headers } from "tar-stream";
import { z } from "zod";
import {
  computeSha256,
  SafeFilenameSchema,
  Sha256Schema,
  verifyAuthoritativeFile,
} from "../utils/checksum";

export type RuntimeName = "llama-server" | "whisper-server" | "sd-server";

export type PlatformTarget = {
  os: string;
  cpu: string;
};

export type PlatformSupportTier = "managed" | "cli-only" | "unsupported";

export type ManagedRuntimeRelease = {
  name: RuntimeName;
  tag: string;
  assetName: string;
  url: string;
  expectedSizeBytes: number;
  sha256: string;
  format: "binary" | "tar.gz" | "zip";
};

export type RuntimeConfig = { root: string };

const RELEASES: Record<string, ManagedRuntimeRelease> = {
  "llama-server:darwin:arm64": {
    name: "llama-server",
    tag: "b9741",
    assetName: "llama-b9741-bin-macos-arm64.tar.gz",
    url: "https://github.com/ggml-org/llama.cpp/releases/download/b9741/llama-b9741-bin-macos-arm64.tar.gz",
    expectedSizeBytes: 10944022,
    sha256: "8d8a9476a105b6c49682fd99e5653033469dba0f3f16495bcbc446838f2a96d1",
    format: "tar.gz",
  },
  "llama-server:darwin:x64": {
    name: "llama-server",
    tag: "b9741",
    assetName: "llama-b9741-bin-macos-x64.tar.gz",
    url: "https://github.com/ggml-org/llama.cpp/releases/download/b9741/llama-b9741-bin-macos-x64.tar.gz",
    expectedSizeBytes: 11242503,
    sha256: "4f0caef6b6a5776fdccf64e1e8c53ba224c4e9cd311be2998d587d89d42e162f",
    format: "tar.gz",
  },
  "llama-server:linux:x64": {
    name: "llama-server",
    tag: "b9741",
    assetName: "llama-b9741-bin-ubuntu-x64.tar.gz",
    url: "https://github.com/ggml-org/llama.cpp/releases/download/b9741/llama-b9741-bin-ubuntu-x64.tar.gz",
    expectedSizeBytes: 15601179,
    sha256: "68a9ab90359d6ecdba0e4a8e34c34aa2cca3b5b4b6e483c2fe52c4fbba62a255",
    format: "tar.gz",
  },
  "llama-server:linux:arm64": {
    name: "llama-server",
    tag: "b9741",
    assetName: "llama-b9741-bin-ubuntu-arm64.tar.gz",
    url: "https://github.com/ggml-org/llama.cpp/releases/download/b9741/llama-b9741-bin-ubuntu-arm64.tar.gz",
    expectedSizeBytes: 12619136,
    sha256: "8dce6449c0629e1166cce24d3264779945b31bbd2d6edbc18e1167f6f9a6ba43",
    format: "tar.gz",
  },
  "whisper-server:darwin:arm64": {
    name: "whisper-server",
    tag: "v0.0.1",
    assetName: "whisper-server-macos-arm64",
    url: "https://github.com/timsexperiments/LocalBase/releases/download/v0.0.1/whisper-server-macos-arm64",
    expectedSizeBytes: 3604256,
    sha256: "9164615ceab08d41a0f6eef914ddda18ffe3608fda68a50f33933e9f8be6277e",
    format: "binary",
  },
  "whisper-server:linux:x64": {
    name: "whisper-server",
    tag: "v0.0.1",
    assetName: "whisper-server-linux-x64",
    url: "https://github.com/timsexperiments/LocalBase/releases/download/v0.0.1/whisper-server-linux-x64",
    expectedSizeBytes: 3254120,
    sha256: "465c4b9071666ddf68843af741c6264d5a216698aee147c18ef0023b1f18ae28",
    format: "binary",
  },
  "sd-server:darwin:arm64": {
    name: "sd-server",
    tag: "master-778-c00a9e9",
    assetName: "sd-master-c00a9e9-bin-Darwin-macOS-26.4-arm64.zip",
    url: "https://github.com/leejet/stable-diffusion.cpp/releases/download/master-778-c00a9e9/sd-master-c00a9e9-bin-Darwin-macOS-26.4-arm64.zip",
    expectedSizeBytes: 49263580,
    sha256: "aeb478142401abba31a356550ba1cb5ff1dbd59ed694860c88a1c79910a1e516",
    format: "zip",
  },
  "sd-server:linux:x64": {
    name: "sd-server",
    tag: "master-778-c00a9e9",
    assetName: "sd-master-c00a9e9-bin-Linux-Ubuntu-24.04-x86_64.zip",
    url: "https://github.com/leejet/stable-diffusion.cpp/releases/download/master-778-c00a9e9/sd-master-c00a9e9-bin-Linux-Ubuntu-24.04-x86_64.zip",
    expectedSizeBytes: 32263875,
    sha256: "da223f809e56d7d24bebfe0ebcfa279394f3549eb557385e3ba8c443a8f8ac5e",
    format: "zip",
  },
};

const FileIdentitySchema = z
  .object({
    size: z.number().int().nonnegative(),
    mtimeMs: z.number().nonnegative(),
    ctimeMs: z.number().nonnegative(),
    dev: z.number().int().nonnegative(),
    ino: z.number().int().nonnegative(),
  })
  .strict();

const ReceiptEntrySchema = z
  .object({
    tag: z.string().min(1),
    assetName: SafeFilenameSchema,
    url: z.string().url(),
    expectedSizeBytes: z.number().int().positive(),
    authoritativeSha256: Sha256Schema,
    binarySha256: Sha256Schema,
    file: FileIdentitySchema,
  })
  .strict();

const ReceiptSchema = z
  .object({
    version: z.literal(1),
    runtimes: z.partialRecord(
      z.enum(["llama-server", "whisper-server", "sd-server"]),
      ReceiptEntrySchema,
    ),
  })
  .strict();

type Receipt = z.infer<typeof ReceiptSchema>;

function currentPlatformTarget(): PlatformTarget {
  return { os: process.platform, cpu: process.arch };
}

function pathBinary(name: RuntimeName): string | undefined {
  for (const directory of (process.env.PATH ?? "").split(delimiter)) {
    const candidate = join(directory || ".", name);
    try {
      accessSync(candidate, constants.X_OK);
      if (statSync(candidate).isFile()) return candidate;
    } catch {
      // Keep searching PATH entries that do not contain an executable binary.
    }
  }
  return Bun.which(name) ?? undefined;
}

function platformLabel(target: PlatformTarget): string {
  if (target.os === "darwin") return `macOS ${target.cpu}`;
  if (target.os === "linux") return `Linux ${target.cpu}`;
  if (target.os === "win32") return "Windows";
  return `${target.os} ${target.cpu}`;
}

export function platformSupportTier(
  target: PlatformTarget,
): PlatformSupportTier {
  if (
    (target.os === "darwin" && target.cpu === "arm64") ||
    (target.os === "linux" && target.cpu === "x64")
  ) {
    return "managed";
  }
  if (
    (target.os === "darwin" && target.cpu === "x64") ||
    (target.os === "linux" && target.cpu === "arm64")
  ) {
    return "cli-only";
  }
  return "unsupported";
}

export function managedRuntimeUnavailableError(
  name: "whisper-server" | "sd-server",
  target: PlatformTarget,
  binDir: string,
): Error {
  const label = platformLabel(target);
  if (platformSupportTier(target) === "cli-only") {
    return new Error(
      `LocalBase CLI-only compatibility on ${label} does not include a managed ${name} runtime. ` +
        `Place a compatible ${name} executable on PATH outside ${binDir}; it will be treated as user-managed and unverified.`,
    );
  }
  return new Error(
    `${label} is unsupported by LocalBase. ${name} cannot be downloaded automatically; ` +
      `place a compatible ${name} executable on PATH outside ${binDir} as a user-managed, unverified runtime.`,
  );
}

export function managedRuntimeRelease(
  name: RuntimeName,
  target: PlatformTarget,
): ManagedRuntimeRelease | undefined {
  return RELEASES[`${name}:${target.os}:${target.cpu}`];
}

function identity(path: string): z.infer<typeof FileIdentitySchema> {
  const stat = statSync(path);
  return {
    size: stat.size,
    mtimeMs: stat.mtimeMs,
    ctimeMs: stat.ctimeMs,
    dev: stat.dev,
    ino: stat.ino,
  };
}

function sameIdentity(
  left: z.infer<typeof FileIdentitySchema>,
  right: z.infer<typeof FileIdentitySchema>,
): boolean {
  return Object.entries(left).every(
    ([key, value]) => right[key as keyof typeof right] === value,
  );
}

function receiptPath(binDir: string): string {
  return join(binDir, ".managed-binaries.json");
}

async function readReceipt(binDir: string): Promise<Receipt | undefined> {
  const path = receiptPath(binDir);
  const file = Bun.file(path);
  if (!(await file.exists())) return undefined;
  let value: unknown;
  try {
    value = JSON.parse(await file.text());
  } catch (error) {
    throw new Error(
      `Invalid managed runtime receipt at ${path}. Remove the managed bin directory and reinstall.`,
      { cause: error },
    );
  }
  const parsed = ReceiptSchema.safeParse(value);
  if (!parsed.success) {
    throw new Error(
      `Invalid managed runtime receipt at ${path}. Remove the managed bin directory and reinstall.`,
    );
  }
  return parsed.data;
}

async function writeReceipt(binDir: string, receipt: Receipt): Promise<void> {
  await Bun.write(
    receiptPath(binDir),
    JSON.stringify(ReceiptSchema.parse(receipt), null, 2),
  );
}

function releaseMatches(
  entry: z.infer<typeof ReceiptEntrySchema>,
  release: ManagedRuntimeRelease,
): boolean {
  return (
    entry.tag === release.tag &&
    entry.assetName === release.assetName &&
    entry.url === release.url &&
    entry.expectedSizeBytes === release.expectedSizeBytes &&
    entry.authoritativeSha256.toLowerCase() === release.sha256
  );
}

async function verifyManagedBinary(
  binDir: string,
  path: string,
  release: ManagedRuntimeRelease,
): Promise<boolean> {
  const receipt = await readReceipt(binDir);
  const entry = receipt?.runtimes[release.name];
  if (!entry || !releaseMatches(entry, release)) return false;

  const currentIdentity = identity(path);
  if (!sameIdentity(currentIdentity, entry.file)) {
    const actual = await computeSha256(path);
    if (actual !== entry.binarySha256.toLowerCase()) {
      throw new Error(
        `Managed ${release.name} failed its continuity check. Remove ${path} and rerun to install the pinned release.`,
      );
    }
    entry.file = currentIdentity;
    await writeReceipt(binDir, receipt!);
  }
  return true;
}

async function downloadRelease(url: string, dest: string): Promise<void> {
  const response = await fetch(url);
  if (!response.ok) {
    throw new Error(
      `Failed to download ${url}: ${response.status} ${response.statusText}.`,
    );
  }
  await Bun.write(dest, response);
}

function archiveDestination(
  stagingDir: string,
  archivePath: string,
  stripComponents: number,
): string | undefined {
  const normalized = archivePath.replaceAll("\\", "/");
  if (
    !normalized ||
    normalized.includes("\0") ||
    normalized.startsWith("/") ||
    /^[a-zA-Z]:\//.test(normalized)
  ) {
    throw new Error(`Unsafe archive path: ${JSON.stringify(archivePath)}.`);
  }

  const components = normalized.split("/").filter(Boolean);
  if (components.some((component) => component === "." || component === "..")) {
    throw new Error(`Unsafe archive path: ${JSON.stringify(archivePath)}.`);
  }
  const destinationComponents = components.slice(stripComponents);
  if (destinationComponents.length === 0) return undefined;

  const root = resolve(stagingDir);
  const destination = resolve(root, ...destinationComponents);
  if (!destination.startsWith(`${root}${sep}`)) {
    throw new Error(`Unsafe archive path: ${JSON.stringify(archivePath)}.`);
  }
  return destination;
}

async function extractTarGz(
  archivePath: string,
  stagingDir: string,
): Promise<void> {
  const extractor = createTarExtractor();
  const completed = new Promise<void>((resolveExtraction, rejectExtraction) => {
    extractor.once("finish", resolveExtraction);
    extractor.once("error", rejectExtraction);
    extractor.on(
      "entry",
      (header: Headers, entry: Readable, next: (error?: unknown) => void) => {
        void (async () => {
          try {
            const destination = archiveDestination(stagingDir, header.name, 1);
            if (header.type === "directory") {
              if (destination) mkdirSync(destination, { recursive: true });
            } else if (!header.type || header.type === "file") {
              if (destination) {
                mkdirSync(dirname(destination), { recursive: true });
                const chunks: Uint8Array[] = [];
                for await (const chunk of entry) chunks.push(chunk);
                const size = chunks.reduce(
                  (total, chunk) => total + chunk.byteLength,
                  0,
                );
                const contents = new Uint8Array(size);
                let offset = 0;
                for (const chunk of chunks) {
                  contents.set(chunk, offset);
                  offset += chunk.byteLength;
                }
                await Bun.write(destination, contents);
              } else {
                entry.resume();
              }
            } else if (
              header.type !== "pax-header" &&
              header.type !== "pax-global-header" &&
              header.type !== "gnu-long-link-path" &&
              header.type !== "gnu-long-path"
            ) {
              throw new Error(
                `Unsupported ${header.type} entry in ${archivePath}.`,
              );
            } else {
              entry.resume();
            }
            next();
          } catch (error) {
            entry.resume();
            next(error);
          }
        })();
      },
    );
  });

  Readable.fromWeb(
    Bun.file(archivePath)
      .stream()
      .pipeThrough(
        new DecompressionStream("gzip"),
      ) as unknown as import("node:stream/web").ReadableStream,
  ).pipe(extractor);
  await completed;
}

async function unzipArchive(
  archivePath: string,
): Promise<Record<string, Uint8Array>> {
  const archive = new Uint8Array(await Bun.file(archivePath).arrayBuffer());
  return new Promise((resolveArchive, rejectArchive) => {
    unzip(archive, (error, files) => {
      if (error) rejectArchive(error);
      else resolveArchive(files);
    });
  });
}

async function extractZip(
  archivePath: string,
  stagingDir: string,
): Promise<void> {
  const files = await unzipArchive(archivePath);
  for (const [archivePath, contents] of Object.entries(files)) {
    const destination = archiveDestination(stagingDir, archivePath, 0);
    if (!destination) continue;
    if (archivePath.endsWith("/")) {
      mkdirSync(destination, { recursive: true });
      continue;
    }
    mkdirSync(dirname(destination), { recursive: true });
    await Bun.write(destination, contents);
  }
}

async function extractRelease(
  release: ManagedRuntimeRelease,
  archivePath: string,
  stagingDir: string,
): Promise<void> {
  try {
    if (release.format === "tar.gz") {
      await extractTarGz(archivePath, stagingDir);
    } else {
      await extractZip(archivePath, stagingDir);
    }
  } catch (error) {
    throw new Error(`Failed to extract ${release.assetName}.`, {
      cause: error,
    });
  }
}

function commitStagedRelease(
  stagingDir: string,
  binDir: string,
  binaryName: RuntimeName,
): void {
  const entries = readdirSync(stagingDir);
  for (const entry of entries) {
    const destination = join(binDir, entry);
    if (existsSync(destination)) {
      throw new Error(
        `Refusing to replace existing managed runtime asset at ${destination}.`,
      );
    }
  }
  for (const entry of [...entries].sort((left, right) => {
    if (left === binaryName) return 1;
    if (right === binaryName) return -1;
    return left.localeCompare(right);
  })) {
    renameSync(join(stagingDir, entry), join(binDir, entry));
  }
}

export async function installManagedRuntime(
  config: RuntimeConfig,
  release: ManagedRuntimeRelease,
): Promise<string> {
  const binDir = join(config.root, "bin");
  mkdirSync(binDir, { recursive: true });
  const downloadPath = join(binDir, `.${release.assetName}.partial`);
  const destPath = join(binDir, release.name);
  const stagingDir = mkdtempSync(join(binDir, ".extract-"));

  try {
    console.log(
      `⬇️  Downloading pinned ${release.name} release ${release.tag}...`,
    );
    await downloadRelease(release.url, downloadPath);
    await verifyAuthoritativeFile(
      downloadPath,
      {
        filename: release.assetName,
        expectedSizeBytes: release.expectedSizeBytes,
        sha256: release.sha256,
      },
      binDir,
    );

    if (release.format === "binary") {
      renameSync(downloadPath, destPath);
    } else {
      await extractRelease(release, downloadPath, stagingDir);
      const stagedBinary = join(stagingDir, release.name);
      if (!(await Bun.file(stagedBinary).exists())) {
        throw new Error(
          `${release.name} was not found after extracting ${release.assetName}.`,
        );
      }
      commitStagedRelease(stagingDir, binDir, release.name);
    }
    if (!(await Bun.file(destPath).exists())) {
      throw new Error(
        `${release.name} was not found after extracting ${release.assetName}.`,
      );
    }

    chmodSync(destPath, statSync(destPath).mode | 0o111);

    const receipt = (await readReceipt(binDir)) ?? { version: 1, runtimes: {} };
    receipt.runtimes[release.name] = {
      tag: release.tag,
      assetName: release.assetName,
      url: release.url,
      expectedSizeBytes: release.expectedSizeBytes,
      authoritativeSha256: release.sha256,
      binarySha256: await computeSha256(destPath),
      file: identity(destPath),
    };
    await writeReceipt(binDir, receipt);
    console.log(
      `✅ ${release.name} installed from authoritative release ${release.tag}.`,
    );
    return destPath;
  } finally {
    rmSync(stagingDir, { recursive: true, force: true });
    try {
      await Bun.file(downloadPath).delete();
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }
}

export async function ensureBinary(
  config: RuntimeConfig,
  name: RuntimeName,
): Promise<string> {
  const binDir = join(config.root, "bin");
  const localBin = join(binDir, name);
  const target = currentPlatformTarget();
  const release = managedRuntimeRelease(name, target);
  const userManagedBinary = pathBinary(name);

  if (await Bun.file(localBin).exists()) {
    if (release && (await verifyManagedBinary(binDir, localBin, release))) {
      return localBin;
    }
    if (userManagedBinary && resolve(userManagedBinary) !== resolve(localBin)) {
      console.log(
        `ℹ️  Using user-managed ${name} at ${userManagedBinary}; LocalBase does not verify user-managed binaries.`,
      );
      return userManagedBinary;
    }
    throw new Error(
      `Refusing untrusted ${name} in the LocalBase managed directory at ${localBin}. ` +
        `Remove it to install the pinned managed release, or put an explicit user-managed executable on PATH outside ${binDir}.`,
    );
  }

  if (userManagedBinary) {
    console.log(
      `ℹ️  Using user-managed ${name} at ${userManagedBinary}; LocalBase does not verify user-managed binaries.`,
    );
    return userManagedBinary;
  }
  if (!release) {
    if (name === "whisper-server" || name === "sd-server") {
      throw managedRuntimeUnavailableError(name, target, binDir);
    }
    throw new Error(
      `No pinned upstream llama.cpp binary is available for ${platformLabel(target)}. ` +
        `Provide a user-managed llama-server on PATH; LocalBase will not call it verified.`,
    );
  }
  return installManagedRuntime(config, release);
}
