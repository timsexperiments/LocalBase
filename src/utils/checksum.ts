import { createReadStream, statSync } from "node:fs";
import {
  mkdir,
  readFile,
  readdir,
  rename,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { join, resolve } from "node:path";
import { setImmediate, setTimeout as sleep } from "node:timers/promises";
import { z } from "zod";
import { processIsAbsent } from "./root";

export const sha256Schema = z.string().regex(/^[a-fA-F0-9]{64}$/);
export const safeFilenameSchema = z
  .string()
  .min(1)
  .refine(
    (name) =>
      name === name.trim() &&
      name !== "." &&
      name !== ".." &&
      !name.includes("/") &&
      !name.includes("\\") &&
      !/[\u0000-\u001f\u007f]/.test(name),
    "must be a safe basename without path separators or control characters",
  );

const fileIdentitySchema = z
  .object({
    size: z.number().int().nonnegative(),
    mtimeMs: z.number().nonnegative(),
    ctimeMs: z.number().nonnegative(),
    dev: z.number().int().nonnegative(),
    ino: z.number().int().nonnegative(),
  })
  .strict();

const verificationEntrySchema = z
  .object({
    authoritativeSha256: sha256Schema,
    expectedSizeBytes: z.number().int().positive(),
    file: fileIdentitySchema,
  })
  .strict();

export const checksumStoreSchema = z
  .object({
    version: z.literal(1),
    entries: z.record(safeFilenameSchema, verificationEntrySchema),
  })
  .strict();

export type ChecksumStore = z.infer<typeof checksumStoreSchema>;

export type AuthoritativeChecksum = {
  filename: string;
  expectedSizeBytes: number;
  sha256: string;
};

export type AuthoritativeVerification = "cached-identity" | "sha256";

function emptyChecksumStore(): ChecksumStore {
  return { version: 1, entries: {} };
}

function issueSummary(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "value"}: ${issue.message}`)
    .join("; ");
}

function fileIdentity(filePath: string): z.infer<typeof fileIdentitySchema> {
  const stat = statSync(filePath);
  return {
    size: stat.size,
    mtimeMs: stat.mtimeMs,
    ctimeMs: stat.ctimeMs,
    dev: stat.dev,
    ino: stat.ino,
  };
}

/** Streams files so model-sized artifacts do not need to fit in memory. */
export async function computeSha256(filePath: string): Promise<string> {
  const hash = new Bun.CryptoHasher("sha256");
  const chunkSize = 1024 * 1024;
  const yieldEveryBytes = 8 * 1024 * 1024;
  let bytesSinceYield = 0;
  for await (const chunk of createReadStream(filePath, {
    highWaterMark: chunkSize,
  })) {
    hash.update(chunk);
    bytesSinceYield += chunk.byteLength;
    if (bytesSinceYield >= yieldEveryBytes) {
      // Buffered reads can otherwise keep hashing in the microtask queue.
      await setImmediate();
      bytesSinceYield = 0;
    }
  }
  return hash.digest("hex");
}

export async function verifyChecksum(
  filePath: string,
  expected: string,
  label: string,
): Promise<void> {
  const digest = sha256Schema.parse(expected).toLowerCase();
  console.log(`🔍 Verifying checksum for ${label}...`);
  const actual = await computeSha256(filePath);
  if (actual !== digest) {
    throw new Error(
      `Checksum mismatch for ${label}!\n` +
        `  Expected: ${digest}\n` +
        `  Got:      ${actual}\n` +
        `  File may be corrupted or tampered with. Delete it and retry.`,
    );
  }
  console.log(`✅ Checksum verified for ${label}`);
}

/** Parses a complete sha256sum response and rejects ambiguous or unsafe rows. */
export function parseChecksumFile(content: string): Map<string, string> {
  const entries = new Map<string, string>();
  for (const [index, line] of content.split(/\r?\n/).entries()) {
    if (!line.trim()) continue;
    const match = /^([a-fA-F0-9]{64})\s+[ *]?(.+)$/.exec(line);
    if (!match) {
      throw new Error(`Invalid checksums.txt entry on line ${index + 1}.`);
    }
    const parsed = z
      .object({ digest: sha256Schema, filename: safeFilenameSchema })
      .strict()
      .safeParse({ digest: match[1], filename: match[2] });
    if (!parsed.success) {
      throw new Error(
        `Invalid checksums.txt entry on line ${index + 1}: ${issueSummary(parsed.error)}.`,
      );
    }
    if (entries.has(parsed.data.filename)) {
      throw new Error(
        `Invalid checksums.txt entry on line ${index + 1}: duplicate filename "${parsed.data.filename}".`,
      );
    }
    entries.set(parsed.data.filename, parsed.data.digest.toLowerCase());
  }
  return entries;
}

function storeFilePath(dir: string): string {
  return join(dir, ".checksums.json");
}

const storeLocks = new Map<string, Promise<void>>();

/** Serializes store read-modify-write cycles per directory, in-process and across processes; a failed task does not poison later ones. */
async function withStoreLock<T>(dir: string, fn: () => Promise<T>): Promise<T> {
  const key = resolve(dir);
  const previous = storeLocks.get(key) ?? Promise.resolve();
  const result = previous.then(async () => {
    const owner = await acquireDirectoryLock(dir);
    try {
      await removeOrphanedTempFiles(dir);
      return await fn();
    } finally {
      await releaseDirectoryLock(dir, owner);
    }
  });
  const tail = result.then(
    () => undefined,
    () => undefined,
  );
  storeLocks.set(key, tail);
  try {
    return await result;
  } finally {
    if (storeLocks.get(key) === tail) storeLocks.delete(key);
  }
}

const LOCK_NAME = ".checksums.json.lock";
const LOCK_OWNER_NAME = "owner.json";
const LOCK_STALE_MS = 30_000;
const LOCK_DEADLINE_MS = 10_000;
const TEMP_STALE_MS = 60_000;

const lockOwnerSchema = z.object({
  pid: z.number().int().positive(),
  token: z.string().min(1),
  acquiredAt: z.number().nonnegative(),
});

type LockOwner = z.infer<typeof lockOwnerSchema>;

function errorCode(error: unknown): string | undefined {
  return (error as NodeJS.ErrnoException | undefined)?.code;
}

async function readLockOwner(lockPath: string): Promise<LockOwner | null> {
  try {
    const parsed = lockOwnerSchema.safeParse(
      JSON.parse(await readFile(join(lockPath, LOCK_OWNER_NAME), "utf8")),
    );
    return parsed.success ? parsed.data : null;
  } catch {
    return null;
  }
}

/** Breaks a dead or expired lock; returns after the attempt so the caller retries. */
async function breakStaleLock(lockPath: string): Promise<void> {
  const owner = await readLockOwner(lockPath);
  let acquiredAt = owner?.acquiredAt;
  if (acquiredAt === undefined) {
    try {
      acquiredAt = (await stat(lockPath)).mtimeMs;
    } catch (error) {
      if (errorCode(error) === "ENOENT") return;
      throw error;
    }
  }
  const expired = Date.now() - acquiredAt > LOCK_STALE_MS;
  if (!expired && !(owner && processIsAbsent(owner.pid))) return;

  const stale = `${lockPath}.${owner?.token ?? crypto.randomUUID()}.stale`;
  try {
    await rename(lockPath, stale);
  } catch (error) {
    if (errorCode(error) === "ENOENT") return;
    throw error;
  }
  // Another breaker may have replaced the lock between our read and rename.
  const moved = await readLockOwner(stale);
  if ((moved?.token ?? null) !== (owner?.token ?? null)) {
    try {
      await rename(stale, lockPath);
      return;
    } catch {
      // A new lock already exists; fall through and discard the moved one.
    }
  }
  await rm(stale, { recursive: true, force: true });
}

async function acquireDirectoryLock(dir: string): Promise<LockOwner> {
  const lockPath = join(dir, LOCK_NAME);
  const deadline = Date.now() + LOCK_DEADLINE_MS;
  for (;;) {
    try {
      await mkdir(lockPath);
      const owner: LockOwner = {
        pid: process.pid,
        token: crypto.randomUUID(),
        acquiredAt: Date.now(),
      };
      try {
        await writeFile(
          join(lockPath, LOCK_OWNER_NAME),
          JSON.stringify(owner),
          { flag: "wx", mode: 0o600 },
        );
      } catch (error) {
        await rm(lockPath, { recursive: true, force: true });
        throw error;
      }
      return owner;
    } catch (error) {
      if (errorCode(error) !== "EEXIST") throw error;
    }
    await breakStaleLock(lockPath);
    if (Date.now() >= deadline) {
      throw new Error(
        `Timed out waiting for the checksum store lock at ${lockPath}. ` +
          `If no other LocalBase process is running, delete that directory and retry.`,
      );
    }
    await sleep(10 + Math.floor(Math.random() * 40));
  }
}

async function releaseDirectoryLock(
  dir: string,
  owner: LockOwner,
): Promise<void> {
  const lockPath = join(dir, LOCK_NAME);
  const current = await readLockOwner(lockPath);
  if (current?.token !== owner.token) return;
  const released = `${lockPath}.${owner.token}.stale`;
  try {
    await rename(lockPath, released);
  } catch (error) {
    if (errorCode(error) === "ENOENT") return;
    throw error;
  }
  await rm(released, { recursive: true, force: true });
}

/** Removes temp files left by crashed or failed writers. Call only while holding the lock. */
async function removeOrphanedTempFiles(dir: string): Promise<void> {
  let names: string[];
  try {
    names = await readdir(dir);
  } catch {
    return;
  }
  for (const name of names) {
    const match = /^\.checksums\.json\.(.+)\.tmp$/.exec(name);
    if (!match) continue;
    const pid = Number(/^(\d+)\./.exec(match[1]!)?.[1]);
    const path = join(dir, name);
    try {
      const old = Date.now() - (await stat(path)).mtimeMs > TEMP_STALE_MS;
      const dead =
        Number.isInteger(pid) && pid > 0 ? processIsAbsent(pid) : false;
      if (old || dead) await rm(path, { force: true });
    } catch {
      // Best-effort cleanup; the file may have vanished or be unreadable.
    }
  }
}

/** This cache records prior verification; its digest never replaces upstream authority. */
export async function readChecksumStore(dir: string): Promise<ChecksumStore> {
  const filePath = storeFilePath(dir);
  const file = Bun.file(filePath);
  if (!(await file.exists())) return emptyChecksumStore();

  const reject = (reason: string) => {
    console.warn(
      `⚠️ Ignoring invalid continuity checksum cache at ${filePath}: ${reason}. Files will be re-verified.`,
    );
    return emptyChecksumStore();
  };

  let value: unknown;
  try {
    value = JSON.parse(await file.text());
  } catch {
    return reject("malformed JSON");
  }
  const parsed = checksumStoreSchema.safeParse(value);
  if (!parsed.success) return reject(issueSummary(parsed.error));
  return parsed.data;
}

export async function writeChecksumStore(
  dir: string,
  store: ChecksumStore,
): Promise<void> {
  const parsed = checksumStoreSchema.safeParse(store);
  if (!parsed.success) {
    throw new Error(
      `Invalid continuity checksum cache: ${issueSummary(parsed.error)}.`,
    );
  }
  // Write-then-rename keeps the final file whole if the process dies mid-write.
  const tempPath = join(
    dir,
    `.checksums.json.${process.pid}.${crypto.randomUUID()}.tmp`,
  );
  try {
    await Bun.write(tempPath, JSON.stringify(parsed.data, null, 2));
    await rename(tempPath, storeFilePath(dir));
  } catch (error) {
    await rm(tempPath, { force: true });
    throw error;
  }
}

/** Skips rehashing only when catalog authority and stable file identity all match. */
export async function verifyAuthoritativeFile(
  filePath: string,
  authority: AuthoritativeChecksum,
  cacheDir: string,
): Promise<AuthoritativeVerification> {
  const parsed = z
    .object({
      filename: safeFilenameSchema,
      expectedSizeBytes: z.number().int().positive(),
      sha256: sha256Schema,
    })
    .strict()
    .parse(authority);
  const identity = fileIdentity(filePath);
  if (identity.size !== parsed.expectedSizeBytes) {
    throw new Error(
      `Size mismatch for ${parsed.filename}: expected ${parsed.expectedSizeBytes} bytes, got ${identity.size} bytes.`,
    );
  }

  const store = await readChecksumStore(cacheDir);
  const cached = store.entries[parsed.filename];
  const digest = parsed.sha256.toLowerCase();
  if (
    cached?.authoritativeSha256.toLowerCase() === digest &&
    cached.expectedSizeBytes === parsed.expectedSizeBytes &&
    Object.entries(identity).every(
      ([key, value]) => cached.file[key as keyof typeof identity] === value,
    )
  ) {
    return "cached-identity";
  }

  // Hashing can take minutes, so the lock is taken only for the merge-and-write.
  await verifyChecksum(filePath, digest, parsed.filename);
  await withStoreLock(cacheDir, async () => {
    const fresh = await readChecksumStore(cacheDir);
    fresh.entries[parsed.filename] = {
      authoritativeSha256: digest,
      expectedSizeBytes: parsed.expectedSizeBytes,
      file: identity,
    };
    await writeChecksumStore(cacheDir, fresh);
  });
  return "sha256";
}
