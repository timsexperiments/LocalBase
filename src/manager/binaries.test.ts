import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { zipSync } from "fflate";
import { pack } from "tar-stream";
import {
  installManagedRuntime,
  type ManagedRuntimeRelease,
  type RuntimeName,
} from "./binaries";

const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

function createRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "local-base-binaries-"));
  roots.push(root);
  return root;
}

function sha256(bytes: Uint8Array): string {
  return new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
}

function release(
  name: RuntimeName,
  format: ManagedRuntimeRelease["format"],
  asset: Uint8Array,
  url: string,
): ManagedRuntimeRelease {
  return {
    name,
    tag: "test-release",
    assetName: `test-${name}.${format}`,
    url,
    expectedSizeBytes: asset.byteLength,
    sha256: sha256(asset),
    format,
  };
}

async function tarGz(entries: Record<string, Uint8Array>): Promise<Uint8Array> {
  const archive = pack();
  for (const [name, contents] of Object.entries(entries)) {
    archive.entry({ name, mode: 0o644 }, Buffer.from(contents));
  }
  archive.finalize();

  const chunks: Uint8Array[] = [];
  for await (const chunk of archive) chunks.push(chunk);
  const length = chunks.reduce((total, chunk) => total + chunk.byteLength, 0);
  const tar = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    tar.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return Bun.gzipSync(tar);
}

async function withArchive<T>(
  archive: Uint8Array,
  callback: (url: string) => Promise<T>,
): Promise<T> {
  const originalFetch = globalThis.fetch;
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    value: async () => new Response(archive.buffer as ArrayBuffer),
  });
  try {
    return await callback("https://releases.local/runtime");
  } finally {
    Object.defineProperty(globalThis, "fetch", {
      configurable: true,
      value: originalFetch,
    });
  }
}

test("installs a verified tar.gz runtime with its staged support files", async () => {
  const binary = new TextEncoder().encode("llama executable");
  const supportFile = new TextEncoder().encode("support library");
  const archive = await tarGz({
    "release/llama-server": binary,
    "release/libsupport.dylib": supportFile,
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const installed = await installManagedRuntime(
      { root },
      release("llama-server", "tar.gz", archive, url),
    );

    expect(await Bun.file(installed).bytes()).toEqual(binary);
    expect(
      await Bun.file(join(root, "bin", "libsupport.dylib")).bytes(),
    ).toEqual(supportFile);
    expect(statSync(installed).mode & 0o111).toBe(0o111);
    expect(
      await Bun.file(join(root, "bin", ".managed-binaries.json")).json(),
    ).toMatchObject({
      version: 1,
      runtimes: { "llama-server": { authoritativeSha256: sha256(archive) } },
    });
  });
});

test("installs a verified ZIP runtime without host archive utilities", async () => {
  const binary = new TextEncoder().encode("sd executable");
  const model = new TextEncoder().encode("runtime support file");
  const archive = zipSync({
    "sd-server": binary,
    "models/support.bin": model,
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const installed = await installManagedRuntime(
      { root },
      release("sd-server", "zip", archive, url),
    );

    expect(await Bun.file(installed).bytes()).toEqual(binary);
    expect(
      await Bun.file(join(root, "bin", "models", "support.bin")).bytes(),
    ).toEqual(model);
  });
});

test("rejects unverified downloads before they reach the managed bin directory", async () => {
  const archive = new TextEncoder().encode("unverified runtime");
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const pinned = release("whisper-server", "binary", archive, url);
    pinned.sha256 = "0".repeat(64);

    await expect(installManagedRuntime({ root }, pinned)).rejects.toThrow(
      "Checksum mismatch",
    );
    expect(await Bun.file(join(root, "bin", "whisper-server")).exists()).toBe(
      false,
    );
    expect(
      await Bun.file(
        join(root, "bin", `.${pinned.assetName}.partial`),
      ).exists(),
    ).toBe(false);
  });
});

test("rejects archive paths that would escape the staging directory", async () => {
  const archive = zipSync({
    "../outside": new TextEncoder().encode("unsafe"),
    "sd-server": new TextEncoder().encode("sd executable"),
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    await expect(
      installManagedRuntime(
        { root },
        release("sd-server", "zip", archive, url),
      ),
    ).rejects.toThrow("Failed to extract");
    expect(await Bun.file(join(root, "outside")).exists()).toBe(false);
    expect(await Bun.file(join(root, "bin", "sd-server")).exists()).toBe(false);
  });
});
