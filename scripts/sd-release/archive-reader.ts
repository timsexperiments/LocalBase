import { Readable } from "node:stream";
import { gunzipSync, unzipSync } from "fflate";
import { extract as createTarExtractor, type Headers } from "tar-stream";
import type { SdPublicationTarget } from "./contracts";

export type ArchiveEntry = {
  name: string;
  type?: string;
  bytes: Uint8Array;
};

async function readTar(bytes: Uint8Array): Promise<ArchiveEntry[]> {
  const extractor = createTarExtractor();
  const entries: ArchiveEntry[] = [];
  const completed = new Promise<void>((resolve, reject) => {
    extractor.once("finish", resolve);
    extractor.once("error", reject);
    extractor.on(
      "entry",
      (header: Headers, stream: Readable, next: (error?: unknown) => void) => {
        void (async () => {
          try {
            const chunks: Uint8Array[] = [];
            for await (const chunk of stream) chunks.push(chunk);
            const length = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
            const entry = new Uint8Array(length);
            let offset = 0;
            for (const chunk of chunks) {
              entry.set(chunk, offset);
              offset += chunk.length;
            }
            entries.push({
              name: header.name,
              type: header.type,
              bytes: entry,
            });
            next();
          } catch (error) {
            next(error);
          }
        })();
      },
    );
  });
  Readable.from([gunzipSync(bytes)]).pipe(extractor);
  await completed;
  return entries;
}

export async function readSdArchiveEntries(
  target: SdPublicationTarget,
  bytes: Uint8Array,
): Promise<ArchiveEntry[]> {
  if (target === "linux-x64") return readTar(bytes);
  try {
    return Object.entries(unzipSync(bytes)).map(([name, entry]) => ({
      name,
      bytes: entry,
    }));
  } catch (error) {
    throw new Error("Invalid macOS sd-server ZIP archive.", { cause: error });
  }
}
