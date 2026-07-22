import { statSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";

export const Sha256Schema = z.string().regex(/^[a-fA-F0-9]{64}$/);
export const SafeFilenameSchema = z
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

const FileIdentitySchema = z
  .object({
    size: z.number().int().nonnegative(),
    mtimeMs: z.number().nonnegative(),
    ctimeMs: z.number().nonnegative(),
    dev: z.number().int().nonnegative(),
    ino: z.number().int().nonnegative(),
  })
  .strict();

const VerificationEntrySchema = z
  .object({
    authoritativeSha256: Sha256Schema,
    expectedSizeBytes: z.number().int().positive(),
    file: FileIdentitySchema,
  })
  .strict();

export const ChecksumStoreSchema = z
  .object({
    version: z.literal(1),
    entries: z.record(SafeFilenameSchema, VerificationEntrySchema),
  })
  .strict();

export type ChecksumStore = z.infer<typeof ChecksumStoreSchema>;

export type AuthoritativeChecksum = {
  filename: string;
  expectedSizeBytes: number;
  sha256: string;
};

function emptyChecksumStore(): ChecksumStore {
  return { version: 1, entries: {} };
}

function issueSummary(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "value"}: ${issue.message}`)
    .join("; ");
}

function fileIdentity(filePath: string): z.infer<typeof FileIdentitySchema> {
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
  for await (const chunk of Bun.file(filePath).stream()) {
    hash.update(chunk);
  }
  return hash.digest("hex");
}

export async function verifyChecksum(
  filePath: string,
  expected: string,
  label: string,
): Promise<void> {
  const digest = Sha256Schema.parse(expected).toLowerCase();
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
      .object({ digest: Sha256Schema, filename: SafeFilenameSchema })
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

/** This cache records prior verification; its digest never replaces upstream authority. */
export async function readChecksumStore(dir: string): Promise<ChecksumStore> {
  const filePath = storeFilePath(dir);
  const file = Bun.file(filePath);
  if (!(await file.exists())) return emptyChecksumStore();

  let value: unknown;
  try {
    value = JSON.parse(await file.text());
  } catch (error) {
    throw new Error(
      `Invalid continuity checksum cache at ${filePath}: malformed JSON. Delete the cache and retry authoritative verification.`,
      { cause: error },
    );
  }
  const parsed = ChecksumStoreSchema.safeParse(value);
  if (!parsed.success) {
    throw new Error(
      `Invalid continuity checksum cache at ${filePath}: ${issueSummary(parsed.error)}. Delete the cache and retry authoritative verification.`,
    );
  }
  return parsed.data;
}

export async function writeChecksumStore(
  dir: string,
  store: ChecksumStore,
): Promise<void> {
  const parsed = ChecksumStoreSchema.safeParse(store);
  if (!parsed.success) {
    throw new Error(
      `Invalid continuity checksum cache: ${issueSummary(parsed.error)}.`,
    );
  }
  await Bun.write(storeFilePath(dir), JSON.stringify(parsed.data, null, 2));
}

/** Skips rehashing only when catalog authority and stable file identity all match. */
export async function verifyAuthoritativeFile(
  filePath: string,
  authority: AuthoritativeChecksum,
  cacheDir: string,
): Promise<void> {
  const parsed = z
    .object({
      filename: SafeFilenameSchema,
      expectedSizeBytes: z.number().int().positive(),
      sha256: Sha256Schema,
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
    return;
  }

  await verifyChecksum(filePath, digest, parsed.filename);
  store.entries[parsed.filename] = {
    authoritativeSha256: digest,
    expectedSizeBytes: parsed.expectedSizeBytes,
    file: identity,
  };
  await writeChecksumStore(cacheDir, store);
}
