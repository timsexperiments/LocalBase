import { constants, type Stats } from "node:fs";
import { lstat, mkdir, open, type FileHandle } from "node:fs/promises";
import { dirname } from "node:path";

const CURRENT_UID =
  typeof process.getuid === "function" ? process.getuid() : undefined;
const NO_FOLLOW = constants.O_NOFOLLOW ?? 0;
const DIRECTORY = constants.O_DIRECTORY ?? 0;

function ownedRegularFile(info: Stats, path: string): void {
  if (!info.isFile() || info.isSymbolicLink()) {
    throw new Error(`LocalBase log path is not a regular file: ${path}.`);
  }
  if (CURRENT_UID !== undefined && info.uid !== CURRENT_UID) {
    throw new Error(
      `LocalBase log file is not owned by the current user: ${path}.`,
    );
  }
  if (info.nlink === 0) {
    const error = new Error(
      `LocalBase log file was unlinked while opening: ${path}.`,
    ) as NodeJS.ErrnoException;
    error.code = "ENOENT";
    throw error;
  }
  if (info.nlink > 1) {
    throw new Error(`LocalBase log file has unsafe hard links: ${path}.`);
  }
}

function sameFile(left: Stats, right: Stats): boolean {
  return left.dev === right.dev && left.ino === right.ino;
}

/**
 * Opens a root-owned regular file without following links and verifies that the
 * pathname still names the opened inode. Callers must keep using this handle.
 */
export async function openOwnedRegularFile(
  path: string,
  flags: number,
  mode = 0o600,
): Promise<{ handle: FileHandle; stat: Stats }> {
  let before: Stats | undefined;
  try {
    before = await lstat(path);
    ownedRegularFile(before, path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    if ((flags & constants.O_CREAT) === 0) throw error;
  }

  let handle: FileHandle | undefined;
  try {
    handle = await open(path, flags | NO_FOLLOW, mode);
    const handleStat = await handle.stat();
    ownedRegularFile(handleStat, path);
    const after = await lstat(path);
    ownedRegularFile(after, path);
    if (!sameFile(handleStat, after) || (before && !sameFile(before, after))) {
      throw new Error(`LocalBase log path changed while opening: ${path}.`);
    }
    await handle.chmod(0o600);
    return { handle, stat: handleStat };
  } catch (error) {
    await handle?.close().catch(() => {});
    throw error;
  }
}

export async function ensureOwnedPrivateDirectory(path: string): Promise<void> {
  await mkdir(path, { recursive: true, mode: 0o700 });
  const info = await lstat(path);
  if (!info.isDirectory() || info.isSymbolicLink()) {
    throw new Error(
      `LocalBase log directory is not a real directory: ${path}.`,
    );
  }
  if (CURRENT_UID !== undefined && info.uid !== CURRENT_UID) {
    throw new Error(
      `LocalBase log directory is not owned by the current user: ${path}.`,
    );
  }
  if ((info.mode & 0o077) !== 0) {
    throw new Error(
      `LocalBase log directory is readable by other users: ${path}.`,
    );
  }
}

export async function syncOwnedPrivateDirectory(path: string): Promise<void> {
  await ensureOwnedPrivateDirectory(path);
  const before = await lstat(path);
  let handle: FileHandle | undefined;
  try {
    handle = await open(path, constants.O_RDONLY | DIRECTORY | NO_FOLLOW);
    const opened = await handle.stat();
    if (!opened.isDirectory() || !sameFile(before, opened)) {
      throw new Error(
        `LocalBase log directory changed while opening: ${path}.`,
      );
    }
    try {
      await handle.sync();
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== "EINVAL" && code !== "ENOTSUP" && code !== "ENOSYS") {
        throw error;
      }
    }
  } finally {
    await handle?.close().catch(() => {});
  }
}

export async function assertOwnedPrivateDirectory(path: string): Promise<void> {
  const parent = dirname(path);
  if (parent !== path) {
    const parentInfo = await lstat(parent);
    if (parentInfo.isSymbolicLink()) {
      throw new Error(`LocalBase log parent is a symbolic link: ${parent}.`);
    }
  }
  await ensureOwnedPrivateDirectory(path);
}

export async function readExact(
  handle: FileHandle,
  position: number,
  length: number,
): Promise<Buffer> {
  const output = Buffer.allocUnsafe(length);
  let offset = 0;
  while (offset < length) {
    const result = await handle.read(
      output,
      offset,
      length - offset,
      position + offset,
    );
    if (result.bytesRead === 0) break;
    offset += result.bytesRead;
  }
  return output.subarray(0, offset);
}

export function fileIdentity(info: Stats): string {
  return `${info.dev}:${info.ino}`;
}
