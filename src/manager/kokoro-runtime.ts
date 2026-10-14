import { chmod, lstat, mkdir, mkdtemp, rename, rm } from "node:fs/promises";
import { dirname, join } from "node:path";
import {
  digestFile,
  parseInventory,
  verifyInventory,
} from "../../runtimes/kokoro/inventory";
import { verifyAuthoritativeFile } from "../utils/checksum";
import { shouldShowOperationalOutput } from "../utils/operational-output";
import {
  currentPlatformTarget,
  downloadRelease,
  extractTarGz,
  serializeManagedInstall,
  type RuntimeConfig,
} from "./binaries";
import {
  managedRuntimeRelease,
  type ManagedRuntimeRelease,
  type PlatformTarget,
} from "./managed-runtime-manifest";

export type KokoroRuntime = Readonly<{
  packageDir: string;
  executable: string;
  bunfig: string;
  entrypoint: string;
  inventorySha256: string;
}>;

export type KokoroRuntimeOptions = Readonly<{
  target?: PlatformTarget;
  /** Test seam: replaces the manifest pin for the current target. */
  release?: ManagedRuntimeRelease;
  signal?: AbortSignal;
}>;

function runtimeFor(packageDir: string, inventorySha256: string) {
  return {
    packageDir,
    executable: join(packageDir, "kokoro-tts"),
    bunfig: join(packageDir, "bunfig.toml"),
    entrypoint: join(packageDir, "cli.js"),
    inventorySha256,
  } satisfies KokoroRuntime;
}

async function exists(path: string): Promise<boolean> {
  try {
    await lstat(path);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
}

/** Resolves the pinned Kokoro release, refusing unpublished placeholder pins. */
export function kokoroRuntimeRelease(
  target: PlatformTarget = currentPlatformTarget(),
): ManagedRuntimeRelease {
  const release = managedRuntimeRelease("kokoro-tts", target);
  if (!release) {
    throw new Error(
      `Kokoro TTS has no managed runtime package for ${target.os} ${target.cpu}.`,
    );
  }
  assertPublished(release);
  return release;
}

function assertPublished(release: ManagedRuntimeRelease): string {
  if (release.pendingPublication || !release.inventorySha256) {
    throw new Error(
      `Kokoro TTS requires the ${release.tag} runtime package, which has not been published yet.`,
    );
  }
  if (release.format !== "tar.gz") {
    throw new Error("Kokoro runtime packages must be tar.gz archives.");
  }
  return release.inventorySha256;
}

/**
 * Installs the pinned, self-contained Kokoro package. The package directory is
 * verified against its inventory (contents and executable bits) and carries no
 * LocalBase receipt, so the inventory remains the only authority.
 */
export async function ensureKokoroRuntime(
  config: RuntimeConfig,
  options: KokoroRuntimeOptions = {},
): Promise<KokoroRuntime> {
  const release = options.release ?? kokoroRuntimeRelease(options.target);
  const inventorySha256 = assertPublished(release);
  const binDir = join(config.root, "bin");
  const packageDir = join(
    binDir,
    "runtimes",
    "kokoro-tts",
    release.sha256.toLowerCase(),
  );

  const verifyExisting = async (): Promise<KokoroRuntime | undefined> => {
    if (!(await exists(packageDir))) return undefined;
    try {
      await verifyInventory(packageDir, inventorySha256);
    } catch (error) {
      throw new Error(
        `Installed Kokoro runtime at ${packageDir} failed verification (${error instanceof Error ? error.message : String(error)}). Remove it and retry to reinstall ${release.tag}.`,
      );
    }
    return runtimeFor(packageDir, inventorySha256);
  };

  const existing = await verifyExisting();
  if (existing) return existing;

  return await serializeManagedInstall(config.root, async () => {
    const raced = await verifyExisting();
    if (raced) return raced;
    options.signal?.throwIfAborted();

    const familyDir = dirname(packageDir);
    await mkdir(familyDir, { recursive: true });
    const installDir = await mkdtemp(join(familyDir, ".install-"));
    const downloadPath = join(installDir, release.assetName);
    const stagingDir = join(installDir, "package");
    await mkdir(stagingDir, { mode: 0o700 });
    try {
      if (shouldShowOperationalOutput())
        console.log(
          `⬇️  Downloading pinned kokoro-tts release ${release.tag}...`,
        );
      await downloadRelease(
        release.url,
        downloadPath,
        release.expectedSizeBytes,
        options.signal,
      );
      await verifyAuthoritativeFile(
        downloadPath,
        {
          filename: release.assetName,
          expectedSizeBytes: release.expectedSizeBytes,
          sha256: release.sha256,
        },
        binDir,
      );
      options.signal?.throwIfAborted();
      await extractTarGz(downloadPath, stagingDir, release.stripComponents);

      const inventoryPath = join(stagingDir, "inventory.json");
      if ((await digestFile(inventoryPath)) !== inventorySha256) {
        throw new Error(
          "Kokoro inventory does not match the pinned release digest.",
        );
      }
      const inventory = parseInventory(
        JSON.parse(await Bun.file(inventoryPath).text()),
      );
      // The extractor does not preserve modes; restore them from the
      // digest-verified inventory before the full integrity check.
      await chmod(inventoryPath, 0o644);
      for (const file of inventory.files) {
        const target = join(stagingDir, file.path);
        if (!(await lstat(target)).isFile()) {
          throw new Error(`Kokoro package integrity failed: ${file.path}`);
        }
        await chmod(target, file.executable ? 0o755 : 0o644);
      }
      await verifyInventory(stagingDir, inventorySha256);
      await chmod(stagingDir, 0o755);
      await rename(stagingDir, packageDir);
    } finally {
      await rm(installDir, { recursive: true, force: true });
    }
    return runtimeFor(packageDir, inventorySha256);
  });
}
