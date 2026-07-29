import {
  chmodSync,
  linkSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  realpathSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { basename, dirname, isAbsolute, join, resolve } from "node:path";
import { z } from "zod";

export const localBaseRootInputSchema = z
  .string()
  .min(1)
  .refine(isAbsolute, "must be an absolute path")
  .refine((value) => !/[\u0000\r\n]/.test(value), {
    message: "must not contain NUL or line breaks",
  });

export const canonicalLocalBaseRootSchema = localBaseRootInputSchema.refine(
  (value) => resolve(value) === value,
  "must be normalized",
);

export class LocalBaseRootError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "LocalBaseRootError";
  }
}

const rootMarkerName = ".localbase-root.json";
const rootInitializationLockName = ".localbase-initialize.lock";
const rootInitializationOwnerName = "owner.json";

export const localBaseRootMarkerSchema = z
  .object({
    version: z.literal(1),
    root: canonicalLocalBaseRootSchema,
    rootHash: z.string().regex(/^[a-f0-9]{64}$/),
    identity: z.uuid(),
  })
  .strict();

export type LocalBaseRootMarker = z.infer<typeof localBaseRootMarkerSchema>;

const rootInitializationOwnerSchema = z
  .object({
    version: z.literal(1),
    token: z.uuid(),
    root: canonicalLocalBaseRootSchema,
    rootHash: z.string().regex(/^[a-f0-9]{64}$/),
    pid: z.number().int().positive(),
    startedAt: z.iso.datetime({ offset: true }),
  })
  .strict();

function rootHash(root: string): string {
  return new Bun.CryptoHasher("sha256").update(root).digest("hex");
}

function resolveExistingAncestor(path: string): string {
  let probe = path;
  const suffix: string[] = [];
  while (true) {
    try {
      return canonicalLocalBaseRootSchema.parse(
        join(realpathSync(probe), ...suffix),
      );
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== "ENOENT" && code !== "ENOTDIR") throw error;
      const parent = dirname(probe);
      if (parent === probe) throw error;
      suffix.unshift(basename(probe));
      probe = parent;
    }
  }
}

/**
 * Resolves lexical and symlink aliases without creating the requested root.
 */
export function canonicalLocalBaseRoot(root: string): string {
  const parsed = localBaseRootInputSchema.safeParse(root);
  if (!parsed.success) {
    throw new LocalBaseRootError(
      `LocalBase root is invalid: ${parsed.error.issues[0]?.message ?? "invalid path"}.`,
    );
  }
  return resolveExistingAncestor(resolve(parsed.data));
}

function rootMarkerPath(root: string): string {
  return join(root, rootMarkerName);
}

function assertUnprotectedRoot(root: string): void {
  const filesystemRoot = canonicalLocalBaseRoot("/");
  const userHome = canonicalLocalBaseRoot(
    resolve(process.env.HOME || homedir()),
  );
  if (root === filesystemRoot || root === userHome) {
    throw new LocalBaseRootError(
      `Refusing LocalBase operation for protected directory: ${root}.`,
    );
  }
}

function assertRootDirectory(root: string): void {
  let info: ReturnType<typeof lstatSync>;
  try {
    info = lstatSync(root);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT") {
      throw new LocalBaseRootError(
        `LocalBase root does not exist: ${root}. Initialize it before using a destructive command.`,
      );
    }
    throw error;
  }
  if (!info.isDirectory() || info.isSymbolicLink()) {
    throw new LocalBaseRootError(
      `LocalBase root must be a real directory: ${root}.`,
    );
  }
}

function parseRootMarker(root: string): LocalBaseRootMarker | undefined {
  const path = rootMarkerPath(root);
  let info: ReturnType<typeof lstatSync>;
  try {
    info = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
  if (!info.isFile() || info.isSymbolicLink()) {
    throw new LocalBaseRootError(
      `LocalBase root marker is not a regular file: ${path}.`,
    );
  }
  if (info.size > 4_096) {
    throw new LocalBaseRootError(
      `LocalBase root marker is too large: ${path}.`,
    );
  }
  const uid =
    typeof process.getuid === "function" ? process.getuid() : undefined;
  if (uid !== undefined && info.uid !== uid) {
    throw new LocalBaseRootError(
      `LocalBase root marker is not owned by the current user: ${path}.`,
    );
  }
  if ((info.mode & 0o077) !== 0) {
    throw new LocalBaseRootError(
      `LocalBase root marker must not be readable by other users: ${path}.`,
    );
  }
  try {
    const marker = localBaseRootMarkerSchema.parse(
      JSON.parse(readFileSync(path, "utf8")),
    );
    if (marker.root !== root || marker.rootHash !== rootHash(root)) {
      throw new LocalBaseRootError(
        `LocalBase root marker is not bound to ${root}.`,
      );
    }
    return marker;
  } catch (error) {
    if (error instanceof LocalBaseRootError) throw error;
    throw new LocalBaseRootError(`LocalBase root marker is invalid: ${path}.`, {
      cause: error,
    });
  }
}

function processIsAbsent(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return false;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ESRCH") return true;
    if (code === "EPERM") return false;
    throw error;
  }
}

function parseInitializationOwner(path: string, root: string) {
  try {
    const info = lstatSync(path);
    if (
      !info.isFile() ||
      info.isSymbolicLink() ||
      info.size > 4_096 ||
      (info.mode & 0o077) !== 0
    ) {
      return undefined;
    }
    const uid =
      typeof process.getuid === "function" ? process.getuid() : undefined;
    if (uid !== undefined && info.uid !== uid) return undefined;
    const parsed = rootInitializationOwnerSchema.safeParse(
      JSON.parse(readFileSync(path, "utf8")),
    );
    if (
      !parsed.success ||
      parsed.data.root !== root ||
      parsed.data.rootHash !== rootHash(root)
    ) {
      return undefined;
    }
    return parsed.data;
  } catch {
    return undefined;
  }
}

function releaseInitializationLock(
  root: string,
  lockPath: string,
  token: string,
): void {
  const owner = parseInitializationOwner(
    join(lockPath, rootInitializationOwnerName),
    root,
  );
  if (!owner || owner.token !== token) return;
  const released = join(root, `.localbase-initialize.${token}.released`);
  try {
    renameSync(lockPath, released);
    const moved = parseInitializationOwner(
      join(released, rootInitializationOwnerName),
      root,
    );
    if (moved?.token === token) {
      rmSync(released, { recursive: true, force: true });
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

function acquireInitializationLock(root: string): {
  path: string;
  token: string;
} {
  const lockPath = join(root, rootInitializationLockName);
  for (let attempt = 0; attempt < 3; attempt += 1) {
    const token = crypto.randomUUID();
    const candidate = join(
      dirname(root),
      `.${basename(root)}.localbase-initialize.${token}.candidate`,
    );
    mkdirSync(candidate, { mode: 0o700 });
    writeFileSync(
      join(candidate, rootInitializationOwnerName),
      JSON.stringify({
        version: 1,
        token,
        root,
        rootHash: rootHash(root),
        pid: process.pid,
        startedAt: new Date().toISOString(),
      }),
      { encoding: "utf8", mode: 0o600, flag: "wx" },
    );
    try {
      renameSync(candidate, lockPath);
      return { path: lockPath, token };
    } catch (error) {
      rmSync(candidate, { recursive: true, force: true });
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== "EEXIST" && code !== "ENOTEMPTY") throw error;
    }

    const owner = parseInitializationOwner(
      join(lockPath, rootInitializationOwnerName),
      root,
    );
    if (!owner || !processIsAbsent(owner.pid)) {
      throw new LocalBaseRootError(
        `LocalBase root initialization is already in progress: ${root}.`,
      );
    }
    const abandoned = join(
      root,
      `.localbase-initialize.${owner.token}.abandoned`,
    );
    try {
      renameSync(lockPath, abandoned);
      const moved = parseInitializationOwner(
        join(abandoned, rootInitializationOwnerName),
        root,
      );
      if (!moved || moved.token !== owner.token) {
        throw new LocalBaseRootError(
          `LocalBase root initialization ownership changed: ${root}.`,
        );
      }
      rmSync(abandoned, { recursive: true, force: true });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }
  throw new LocalBaseRootError(
    `Could not acquire LocalBase root initialization ownership: ${root}.`,
  );
}

/** Creates the immutable marker that authorizes LocalBase to manage a root. */
export function ensureLocalBaseRootMarker(root: string): LocalBaseRootMarker {
  const canonical = canonicalLocalBaseRoot(root);
  assertUnprotectedRoot(canonical);
  mkdirSync(canonical, { recursive: true, mode: 0o700 });
  assertRootDirectory(canonical);
  const existing = parseRootMarker(canonical);
  if (existing) return existing;

  const destination = rootMarkerPath(canonical);
  const initialization = acquireInitializationLock(canonical);

  try {
    const entries = readdirSync(canonical);
    if (entries.length !== 1 || entries[0] !== rootInitializationLockName) {
      throw new LocalBaseRootError(
        `Refusing to initialize nonempty LocalBase root: ${canonical}.`,
      );
    }
    const marker = localBaseRootMarkerSchema.parse({
      version: 1,
      root: canonical,
      rootHash: rootHash(canonical),
      identity: crypto.randomUUID(),
    });
    const temporary = join(initialization.path, "marker.json");
    writeFileSync(temporary, JSON.stringify(marker), {
      encoding: "utf8",
      mode: 0o600,
      flag: "wx",
    });
    chmodSync(temporary, 0o600);
    linkSync(temporary, destination);
  } finally {
    releaseInitializationLock(
      canonical,
      initialization.path,
      initialization.token,
    );
  }
  return (
    parseRootMarker(canonical) ??
    (() => {
      throw new LocalBaseRootError(
        `LocalBase root marker could not be created: ${destination}.`,
      );
    })()
  );
}

/** Validates an initialized LocalBase root without creating files or opening SQLite. */
export function assertInitializedLocalBaseRoot(
  root: string,
): LocalBaseRootMarker {
  const canonical = canonicalLocalBaseRoot(root);
  assertUnprotectedRoot(canonical);
  assertRootDirectory(canonical);
  const marker = parseRootMarker(canonical);
  if (!marker) {
    throw new LocalBaseRootError(
      `Refusing destructive LocalBase operation for uninitialized directory: ${canonical}.`,
    );
  }
  return marker;
}

/** Validates that a destructive command targets an initialized LocalBase root. */
export function assertDestructiveLocalBaseRoot(
  root: string,
): LocalBaseRootMarker {
  return assertInitializedLocalBaseRoot(root);
}
