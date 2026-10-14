import {
  ggufMetadataPath,
  readLlmKvGeometry,
  readLlmTrainingContextLength,
  type LlmKvGeometry,
} from "./gguf-metadata";

type HeaderRead<T> = (path: string) => Promise<T | null>;
type HeaderCacheEntry<T> = Readonly<{
  signature: string;
  value: Promise<T | null>;
}>;

/**
 * Caches a parsed GGUF header value per path while the metadata shard's size
 * and mtime stay fixed. Null results and failures are not cached, so a model
 * that finishes installing is picked up on the next read.
 */
function createGgufHeaderCache<T>(
  read: HeaderRead<T>,
): (path: string) => Promise<T | null> {
  const cache = new Map<string, HeaderCacheEntry<T>>();
  return async (path) => {
    const stat = await Bun.file(ggufMetadataPath(path))
      .stat()
      .catch(() => null);
    if (!stat) return null;

    const signature = `${stat.size}:${stat.mtimeMs}`;
    let entry = cache.get(path);
    if (!entry || entry.signature !== signature) {
      entry = { signature, value: read(path) };
      cache.set(path, entry);
    }
    try {
      const value = await entry.value;
      if (value === null && cache.get(path) === entry) cache.delete(path);
      return value;
    } catch (error) {
      if (cache.get(path) === entry) cache.delete(path);
      throw error;
    }
  };
}

/** Cache parsed geometry only while the model file's size and mtime stay fixed. */
export function createLlmKvGeometryReader(
  read: HeaderRead<LlmKvGeometry> = readLlmKvGeometry,
): (path: string) => Promise<LlmKvGeometry | null> {
  return createGgufHeaderCache(read);
}

/** Cache the GGUF training context only while the model file is unchanged. */
export function createLlmTrainingContextLengthReader(
  read: HeaderRead<number> = readLlmTrainingContextLength,
): (path: string) => Promise<number | null> {
  return createGgufHeaderCache(read);
}
