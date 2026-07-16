import { createHash } from "node:crypto";
import { createReadStream, existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

/**
 * Computes the SHA-256 hex digest of a file using a streaming reader,
 * safe for arbitrarily large files (model weights, binary releases, etc.).
 */
export async function computeSha256(filePath: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const hash = createHash("sha256");
    const stream = createReadStream(filePath);
    stream.on("data", (chunk) => hash.update(chunk));
    stream.on("end", () => resolve(hash.digest("hex")));
    stream.on("error", reject);
  });
}

/**
 * Verifies that a file on disk matches an expected SHA-256 hex digest.
 * Throws a descriptive error on mismatch so callers can surface it clearly.
 */
export async function verifyChecksum(filePath: string, expected: string, label: string): Promise<void> {
  console.log(`🔍 Verifying checksum for ${label}...`);
  const actual = await computeSha256(filePath);
  if (actual !== expected.toLowerCase().trim()) {
    throw new Error(
      `Checksum mismatch for ${label}!\n` +
      `  Expected: ${expected.toLowerCase()}\n` +
      `  Got:      ${actual}\n` +
      `  File may be corrupted or tampered with. Delete it and retry.`
    );
  }
  console.log(`✅ Checksum verified for ${label}`);
}

/**
 * Parses a checksums.txt file in standard sha256sum format:
 *   <hex>  <filename>
 * Returns a Map of filename → sha256 hex string.
 */
export function parseChecksumFile(content: string): Map<string, string> {
  const map = new Map<string, string>();
  for (const line of content.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const spaceIdx = trimmed.search(/\s+/);
    if (spaceIdx === -1) continue;
    const hash = trimmed.slice(0, spaceIdx).trim();
    const name = trimmed.slice(spaceIdx).trim();
    if (hash && name) map.set(name, hash);
  }
  return map;
}

// ---------------------------------------------------------------------------
// Local checksum store — a JSON file recording hashes of previously verified
// files. Used to quickly detect corruption on re-runs without re-downloading.
// ---------------------------------------------------------------------------

type ChecksumStore = Record<string, string>;

function storeFilePath(dir: string): string {
  return join(dir, ".checksums.json");
}

export function readChecksumStore(dir: string): ChecksumStore {
  const file = storeFilePath(dir);
  if (!existsSync(file)) return {};
  try {
    return JSON.parse(readFileSync(file, "utf8")) as ChecksumStore;
  } catch {
    return {};
  }
}

export function writeChecksumStore(dir: string, store: ChecksumStore): void {
  writeFileSync(storeFilePath(dir), JSON.stringify(store, null, 2), "utf8");
}

/**
 * Records the SHA-256 of a file into the local checksum store for the given
 * directory. Subsequent calls to `verifyStoredChecksum` will use this value.
 */
export async function recordChecksum(dir: string, filename: string, filePath: string): Promise<string> {
  const hash = await computeSha256(filePath);
  const store = readChecksumStore(dir);
  store[filename] = hash;
  writeChecksumStore(dir, store);
  return hash;
}

/**
 * Verifies a file against its previously recorded checksum in the local store.
 * Returns true if a stored checksum exists and matches, false if no stored
 * checksum exists (allowing first-run installs through), and throws on mismatch.
 */
export async function verifyStoredChecksum(dir: string, filename: string, filePath: string): Promise<boolean> {
  const store = readChecksumStore(dir);
  const expected = store[filename];
  if (!expected) return false; // No recorded checksum yet — first run
  await verifyChecksum(filePath, expected, filename);
  return true;
}
