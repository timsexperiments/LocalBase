import { readLlmKvGeometry, type LlmKvGeometry } from "./gguf-metadata";

type GeometryRead = (path: string) => Promise<LlmKvGeometry | null>;
type GeometryCacheEntry = Readonly<{
  signature: string;
  geometry: Promise<LlmKvGeometry | null>;
}>;

/** Cache parsed geometry only while the model file's size and mtime stay fixed. */
export function createLlmKvGeometryReader(
  read: GeometryRead = readLlmKvGeometry,
): (path: string) => Promise<LlmKvGeometry | null> {
  const cache = new Map<string, GeometryCacheEntry>();
  return async (path) => {
    const stat = await Bun.file(path)
      .stat()
      .catch(() => null);
    if (!stat) return null;

    const signature = `${stat.size}:${stat.mtimeMs}`;
    let entry = cache.get(path);
    if (!entry || entry.signature !== signature) {
      entry = { signature, geometry: read(path) };
      cache.set(path, entry);
    }
    try {
      const value = await entry.geometry;
      if (!value && cache.get(path) === entry) cache.delete(path);
      return value;
    } catch (error) {
      if (cache.get(path) === entry) cache.delete(path);
      throw error;
    }
  };
}
