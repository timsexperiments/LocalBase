import { constants } from "node:fs";
import { lstat, rename } from "node:fs/promises";
import { join } from "node:path";
import { canonicalLocalBaseRoot } from "../../utils/root";
import {
  ensureOwnedPrivateDirectory,
  openOwnedRegularFile,
  readExact,
  syncOwnedPrivateDirectory,
} from "./secure-log-files";

/**
 * launchd never rotates job output, so the managed macOS gateway sends stderr to one
 * private, size-bounded file that exists for output the structured sink cannot
 * record: failures before it opens, Bun panics, and uncaught errors. Console copies
 * of structured events are suppressed once the sink is active, and stdout stays
 * discarded because it only carries those copies and startup chatter.
 */
export const SERVICE_STDERR_FILENAME = "service-stderr.log";
export const SERVICE_STDERR_ARCHIVE_FILENAME = "service-stderr.log.1";
export const MAX_SERVICE_STDERR_BYTES = 1024 * 1024;

// Mirrors LOG_DIRECTORY_NAME without importing the logging module into service definitions.
const SERVICE_STDERR_DIRECTORY_NAME = "logs";

export function serviceStderrDirectory(root: string): string {
  return join(canonicalLocalBaseRoot(root), SERVICE_STDERR_DIRECTORY_NAME);
}

export function serviceStderrPath(root: string): string {
  return join(serviceStderrDirectory(root), SERVICE_STDERR_FILENAME);
}

export function serviceStderrArchivePath(root: string): string {
  return join(serviceStderrDirectory(root), SERVICE_STDERR_ARCHIVE_FILENAME);
}

/** True when this process is a launchd-managed gateway whose stderr is captured to a file. */
export function capturesServiceStderr(
  environment: Record<string, string | undefined>,
  platform: NodeJS.Platform = process.platform,
): boolean {
  return platform === "darwin" && Boolean(environment.LOCALBASE_SERVICE_TOKEN);
}

/** Creates the private directory and file before launchd opens the path for the job. */
export async function prepareServiceStderrLog(root: string): Promise<void> {
  await ensureOwnedPrivateDirectory(serviceStderrDirectory(root));
  const opened = await openOwnedRegularFile(
    serviceStderrPath(root),
    constants.O_CREAT | constants.O_WRONLY | constants.O_APPEND,
  );
  await opened.handle.close();
}

/**
 * Keeps the captured stderr bounded across restarts. When the file exceeds the cap,
 * its newest MAX_SERVICE_STDERR_BYTES move to the `.1` archive and the active file is
 * truncated in place; launchd opens it with O_APPEND, so later writes start at zero.
 */
export async function boundServiceStderrLog(
  root: string,
  maxBytes = MAX_SERVICE_STDERR_BYTES,
): Promise<boolean> {
  const directory = serviceStderrDirectory(root);
  let opened: Awaited<ReturnType<typeof openOwnedRegularFile>>;
  try {
    opened = await openOwnedRegularFile(
      serviceStderrPath(root),
      constants.O_RDWR,
    );
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
  try {
    const size = opened.stat.size;
    if (size <= maxBytes) return false;
    const tail = await readExact(opened.handle, size - maxBytes, maxBytes);
    const temporary = join(
      directory,
      `.service-stderr.${crypto.randomUUID()}.tmp`,
    );
    const archive = await openOwnedRegularFile(
      temporary,
      constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY,
    );
    try {
      try {
        await archive.handle.writeFile(tail);
        await archive.handle.sync();
      } finally {
        await archive.handle.close();
      }
      await rename(temporary, serviceStderrArchivePath(root));
    } catch (error) {
      await Bun.file(temporary)
        .delete()
        .catch(() => {});
      throw error;
    }
    await opened.handle.truncate(0);
    await syncOwnedPrivateDirectory(directory);
    return true;
  } finally {
    await opened.handle.close();
  }
}

export type ServiceStderrSummary = {
  path: string;
  bytes: number;
  modifiedAt: string;
  archiveBytes: number;
};

async function regularFileSize(
  path: string,
): Promise<{ bytes: number; modifiedAt: Date } | undefined> {
  try {
    const info = await lstat(path);
    if (!info.isFile() || info.isSymbolicLink()) return undefined;
    return { bytes: info.size, modifiedAt: info.mtime };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
}

/**
 * Reports captured service stderr without reading it. The content is raw process
 * output that never passed through log redaction, so callers surface only metadata.
 */
export async function inspectServiceStderrLog(
  root: string,
): Promise<ServiceStderrSummary | undefined> {
  const [active, archive] = await Promise.all([
    regularFileSize(serviceStderrPath(root)),
    regularFileSize(serviceStderrArchivePath(root)),
  ]);
  const bytes = active?.bytes ?? 0;
  const archiveBytes = archive?.bytes ?? 0;
  if (bytes === 0 && archiveBytes === 0) return undefined;
  const modifiedAt =
    active && active.bytes > 0 ? active.modifiedAt : archive!.modifiedAt;
  return {
    path: serviceStderrPath(root),
    bytes,
    modifiedAt: modifiedAt.toISOString(),
    archiveBytes,
  };
}
