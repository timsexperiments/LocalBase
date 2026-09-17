import { installManagedRuntime } from "./binaries";
import { videoConverterRelease } from "./video-converter-release";

/** Resolves only the pinned converter, never a user-managed PATH executable. */
export async function ensureVideoConverter(
  root: string,
  signal?: AbortSignal,
): Promise<string> {
  signal?.throwIfAborted();
  const { release, supportFiles } = videoConverterRelease({
    os: process.platform,
    cpu: process.arch,
  });
  return installManagedRuntime({ root }, release, { signal, supportFiles });
}
