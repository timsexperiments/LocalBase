import { afterEach, expect, test } from "bun:test";
import {
  chmodSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readlinkSync,
  rmSync,
  statSync,
  symlinkSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { zipSync } from "fflate";
import { pack } from "tar-stream";
import {
  ensureBinary,
  installManagedRuntime,
  managedExecutableRelease,
  managedRuntimeRelease,
  type ManagedExecutableName,
  type ManagedRuntimeRelease,
} from "./binaries";

const roots: string[] = [];
const originalPath = process.env.PATH;

afterEach(() => {
  process.env.PATH = originalPath;
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
  name: ManagedExecutableName,
  format: ManagedRuntimeRelease["format"],
  asset: Uint8Array,
  url: string,
  stripComponents: number,
): Parameters<typeof installManagedRuntime>[1] {
  return {
    name,
    tag: "test-release",
    assetName: `test-runtime.${format}`,
    url,
    expectedSizeBytes: asset.byteLength,
    sha256: sha256(asset),
    format,
    stripComponents,
  };
}

type TestRelease = ReturnType<typeof release>;

function familyName(name: ManagedExecutableName): string {
  if (name === "llama-server" || name === "llama-tts") {
    return "llama-server";
  }
  if (name === "sd-server" || name === "sd-cli") return "sd-server";
  return "whisper-server";
}

function packageDirectory(root: string, pinned: TestRelease): string {
  return join(
    root,
    "bin",
    "runtimes",
    familyName(pinned.name),
    pinned.sha256.toLowerCase(),
  );
}

test("maps helpers to the existing pinned runtime artifact families", () => {
  const target = { os: "darwin", cpu: "arm64" };
  const llamaServer = managedRuntimeRelease("llama-server", target);
  const llamaTts = managedExecutableRelease("llama-tts", target);
  const sdServer = managedRuntimeRelease("sd-server", target);
  const sdCli = managedExecutableRelease("sd-cli", target);
  if (!llamaServer || !llamaTts || !sdServer || !sdCli) {
    throw new Error("Expected managed macOS arm64 runtime releases.");
  }

  expect(llamaTts).toEqual({ ...llamaServer, name: "llama-tts" });
  expect(sdCli).toEqual({ ...sdServer, name: "sd-cli" });
});

async function tarGz(
  entries: Record<string, Uint8Array | { linkname: string }>,
): Promise<Uint8Array> {
  const archive = pack();
  for (const [name, contents] of Object.entries(entries)) {
    if (contents instanceof Uint8Array) {
      archive.entry({ name, mode: 0o644 }, Buffer.from(contents));
    } else {
      archive.entry({
        name,
        type: "symlink",
        linkname: contents.linkname,
      });
    }
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

async function withFirstArchiveDownloadHeld<T>(
  archive: Uint8Array,
  callback: (options: {
    url: string;
    firstDownloadStarted: Promise<void>;
    releaseFirstDownload: () => void;
    fetchCount: () => number;
  }) => Promise<T>,
): Promise<T> {
  const originalFetch = globalThis.fetch;
  let notifyFirstDownloadStarted: () => void = () => {
    throw new Error("First download start was not initialized.");
  };
  const firstDownloadStarted = new Promise<void>((resolve) => {
    notifyFirstDownloadStarted = resolve;
  });
  let releaseFirstDownload: () => void = () => {
    throw new Error("First download release was not initialized.");
  };
  const firstDownloadReleased = new Promise<void>((resolve) => {
    releaseFirstDownload = resolve;
  });
  let fetches = 0;
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    value: async () => {
      fetches += 1;
      if (fetches === 1) {
        notifyFirstDownloadStarted();
        await firstDownloadReleased;
      }
      return new Response(archive.buffer as ArrayBuffer);
    },
  });
  try {
    return await callback({
      url: "https://releases.local/runtime",
      firstDownloadStarted,
      releaseFirstDownload,
      fetchCount: () => fetches,
    });
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
    "release/libsupport.dylib.link": { linkname: "libsupport.dylib" },
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const pinned = release("llama-server", "tar.gz", archive, url, 1);
    const installed = await installManagedRuntime({ root }, pinned);
    const packageDir = packageDirectory(root, pinned);

    expect(installed).toBe(join(packageDir, "llama-server"));
    expect(await Bun.file(installed).bytes()).toEqual(binary);
    expect(
      await Bun.file(join(packageDir, "libsupport.dylib")).bytes(),
    ).toEqual(supportFile);
    expect(statSync(installed).mode & 0o111).toBe(0o111);
    const installedLink = join(packageDir, "libsupport.dylib.link");
    expect(lstatSync(installedLink).isSymbolicLink()).toBe(true);
    expect(readlinkSync(installedLink)).toBe("libsupport.dylib");
    expect(
      await Bun.file(join(packageDir, ".managed-binaries.json")).json(),
    ).toMatchObject({
      version: 1,
      runtimes: {
        "llama-server": {
          authoritativeSha256: sha256(archive),
          format: "tar.gz",
          stripComponents: 1,
        },
      },
    });
  });
});

test("isolates archive versions and resolves helpers from their family package", async () => {
  const llamaV1 = await tarGz({
    "release/llama-server": new TextEncoder().encode("llama server v1"),
    "release/llama-tts": new TextEncoder().encode("llama helper v1"),
    "release/libggml.dylib": new TextEncoder().encode("llama ABI v1"),
  });
  const llamaV2 = await tarGz({
    "release/llama-server": new TextEncoder().encode("llama server v2"),
    "release/llama-tts": new TextEncoder().encode("llama helper v2"),
    "release/libggml.dylib": new TextEncoder().encode("llama ABI v2"),
  });
  const sd = zipSync({
    "sd-server": new TextEncoder().encode("sd server"),
    "sd-cli": new TextEncoder().encode("sd helper"),
    "libggml.dylib": new TextEncoder().encode("sd ABI"),
  });
  const root = createRoot();
  let llamaV1Release: TestRelease | undefined;
  let llamaV2Release: TestRelease | undefined;
  let sdRelease: TestRelease | undefined;

  await withArchive(llamaV1, async (url) => {
    llamaV1Release = release("llama-server", "tar.gz", llamaV1, url, 1);
    await installManagedRuntime({ root }, llamaV1Release);
  });
  await withArchive(llamaV2, async (url) => {
    llamaV2Release = release("llama-server", "tar.gz", llamaV2, url, 1);
    await installManagedRuntime({ root }, llamaV2Release);
  });
  await withArchive(sd, async (url) => {
    sdRelease = release("sd-server", "zip", sd, url, 0);
    await installManagedRuntime({ root }, sdRelease);
  });
  if (!llamaV1Release || !llamaV2Release || !sdRelease) {
    throw new Error("Expected fixture releases to be installed.");
  }

  const llamaV1Dir = packageDirectory(root, llamaV1Release);
  const llamaV2Dir = packageDirectory(root, llamaV2Release);
  const sdDir = packageDirectory(root, sdRelease);
  expect(llamaV1Dir).not.toBe(llamaV2Dir);
  expect(await Bun.file(join(llamaV1Dir, "libggml.dylib")).text()).toBe(
    "llama ABI v1",
  );
  expect(await Bun.file(join(llamaV2Dir, "libggml.dylib")).text()).toBe(
    "llama ABI v2",
  );
  expect(await Bun.file(join(sdDir, "libggml.dylib")).text()).toBe("sd ABI");

  const originalFetch = globalThis.fetch;
  let downloads = 0;
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    value: async () => {
      downloads += 1;
      return new Response(null, { status: 503 });
    },
  });
  try {
    const llamaHelper = await installManagedRuntime(
      { root },
      { ...llamaV2Release, name: "llama-tts" },
    );
    const sdHelper = await installManagedRuntime(
      { root },
      { ...sdRelease, name: "sd-cli" },
    );
    expect(llamaHelper).toBe(join(llamaV2Dir, "llama-tts"));
    expect(await Bun.file(llamaHelper).text()).toBe("llama helper v2");
    expect(sdHelper).toBe(join(sdDir, "sd-cli"));
    expect(await Bun.file(sdHelper).text()).toBe("sd helper");
    expect(downloads).toBe(0);
  } finally {
    Object.defineProperty(globalThis, "fetch", {
      configurable: true,
      value: originalFetch,
    });
  }

  expect(await Bun.file(join(llamaV1Dir, "libggml.dylib")).text()).toBe(
    "llama ABI v1",
  );
});

test("never treats paths under the managed bin directory as PATH fallbacks", async () => {
  const target = { os: process.platform, cpu: process.arch };
  if (
    !managedExecutableRelease("llama-server", target) ||
    !managedExecutableRelease("whisper-server", target) ||
    !managedExecutableRelease("sd-server", target)
  ) {
    return;
  }

  const root = createRoot();
  const binDir = join(root, "bin");
  const nestedDir = join(binDir, "runtimes", "unverified", "archive");
  const managedTargetDir = join(binDir, "unverified-target");
  const operatorBinDir = join(root, "operator-bin");
  mkdirSync(nestedDir, { recursive: true });
  mkdirSync(managedTargetDir);
  mkdirSync(operatorBinDir);
  const legacyBinary = join(binDir, "whisper-server");
  const nestedBinary = join(nestedDir, "llama-server");
  const managedSymlinkTarget = join(managedTargetDir, "sd-server");
  const candidates = [legacyBinary, nestedBinary, managedSymlinkTarget];
  for (const candidate of candidates) {
    await Bun.write(candidate, "unverified executable");
    chmodSync(candidate, 0o755);
  }
  symlinkSync(managedSymlinkTarget, join(operatorBinDir, "sd-server"));
  process.env.PATH = [binDir, nestedDir, operatorBinDir].join(delimiter);

  const originalFetch = globalThis.fetch;
  let downloads = 0;
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    value: async () => {
      downloads += 1;
      return new Response(null, { status: 503, statusText: "fixture failure" });
    },
  });
  try {
    for (const name of [
      "whisper-server",
      "llama-server",
      "sd-server",
    ] as const) {
      await expect(ensureBinary({ root }, name)).rejects.toThrow(
        "Failed to download",
      );
    }
  } finally {
    Object.defineProperty(globalThis, "fetch", {
      configurable: true,
      value: originalFetch,
    });
  }
  expect(downloads).toBe(3);
});

test("reuses verified family helpers in either installation order", async () => {
  const server = new TextEncoder().encode("llama server executable");
  const helper = new TextEncoder().encode("llama tts executable");
  const supportFile = new TextEncoder().encode("support library");
  const archive = await tarGz({
    "release/llama-server": server,
    "release/llama-tts": helper,
    "release/libsupport.dylib": supportFile,
  });
  await withArchive(archive, async (url) => {
    for (const [first, second] of [
      ["llama-server", "llama-tts"],
      ["llama-tts", "llama-server"],
    ] as const) {
      const root = createRoot();
      const firstRelease = release(first, "tar.gz", archive, url, 1);
      const secondRelease = release(second, "tar.gz", archive, url, 1);
      const firstPath = await installManagedRuntime({ root }, firstRelease);
      const packageDir = packageDirectory(root, firstRelease);
      const secondPath = join(packageDir, second);
      expect(statSync(firstPath).mode & 0o111).toBe(0o111);
      expect(statSync(secondPath).mode & 0o111).toBe(0o111);
      expect(statSync(join(packageDir, "libsupport.dylib")).mode & 0o111).toBe(
        0,
      );

      const installedSecond = await installManagedRuntime(
        { root },
        secondRelease,
      );
      expect(installedSecond).toBe(secondPath);
      expect(await Bun.file(firstPath).bytes()).toEqual(
        first === "llama-server" ? server : helper,
      );
      expect(await Bun.file(secondPath).bytes()).toEqual(
        second === "llama-server" ? server : helper,
      );
      expect(statSync(firstPath).mode & 0o111).toBe(0o111);
      expect(statSync(secondPath).mode & 0o111).toBe(0o111);
      expect(
        await Bun.file(join(packageDir, ".managed-binaries.json")).json(),
      ).toMatchObject({
        runtimes: {
          "llama-server": {
            authoritativeSha256: sha256(archive),
            binarySha256: sha256(server),
          },
          "llama-tts": {
            authoritativeSha256: sha256(archive),
            binarySha256: sha256(helper),
          },
        },
      });
    }
  });
});

test("serializes concurrent helper and primary package installation", async () => {
  const server = new TextEncoder().encode("llama server executable");
  const helper = new TextEncoder().encode("llama tts executable");
  const archive = await tarGz({
    "release/llama-server": server,
    "release/llama-tts": helper,
    "release/libsupport.dylib": new TextEncoder().encode("support library"),
  });
  const root = createRoot();

  await withFirstArchiveDownloadHeld(
    archive,
    async ({ url, firstDownloadStarted, releaseFirstDownload, fetchCount }) => {
      const helperInstall = installManagedRuntime(
        { root },
        release("llama-tts", "tar.gz", archive, url, 1),
      );
      const primaryInstall = installManagedRuntime(
        { root },
        release("llama-server", "tar.gz", archive, url, 1),
      );

      await firstDownloadStarted;
      expect(fetchCount()).toBe(1);
      releaseFirstDownload();
      await Promise.all([helperInstall, primaryInstall]);

      expect(fetchCount()).toBe(1);
      const packageDir = packageDirectory(
        root,
        release("llama-server", "tar.gz", archive, url, 1),
      );
      expect(statSync(join(packageDir, "llama-tts")).mode & 0o111).toBe(0o111);
      expect(statSync(join(packageDir, "llama-server")).mode & 0o111).toBe(
        0o111,
      );
    },
  );
});

test("ignores a legacy flat helper when installing its package", async () => {
  const server = new TextEncoder().encode("llama server executable");
  const helper = new TextEncoder().encode("llama tts executable");
  const supportFile = new TextEncoder().encode("support library");
  const archive = await tarGz({
    "release/llama-server": server,
    "release/llama-tts": helper,
    "release/libsupport.dylib": supportFile,
  });
  const root = createRoot();
  const helperPath = join(root, "bin", "llama-tts");
  mkdirSync(join(root, "bin"), { recursive: true });
  await Bun.write(helperPath, "legacy helper");

  await withArchive(archive, async (url) => {
    const pinned = release("llama-tts", "tar.gz", archive, url, 1);
    const installed = await installManagedRuntime({ root }, pinned);

    expect(installed).toBe(join(packageDirectory(root, pinned), "llama-tts"));
    expect(await Bun.file(installed).bytes()).toEqual(helper);
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "llama-server"),
      ).bytes(),
    ).toEqual(server);
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "libsupport.dylib"),
      ).bytes(),
    ).toEqual(supportFile);
  });

  expect(await Bun.file(helperPath).text()).toBe("legacy helper");
});

test("rejects a modified packaged helper through binary continuity", async () => {
  const server = new TextEncoder().encode("llama server executable");
  const helper = new TextEncoder().encode("llama tts executable");
  const archive = await tarGz({
    "release/llama-server": server,
    "release/llama-tts": helper,
    "release/libsupport.dylib": new TextEncoder().encode("support library"),
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const serverRelease = release("llama-server", "tar.gz", archive, url, 1);
    await installManagedRuntime({ root }, serverRelease);
    const helperPath = join(packageDirectory(root, serverRelease), "llama-tts");
    await Bun.write(helperPath, "modified helper");
    await expect(
      installManagedRuntime(
        { root },
        release("llama-tts", "tar.gz", archive, url, 1),
      ),
    ).rejects.toThrow("failed its continuity check");
    expect(await Bun.file(helperPath).text()).toBe("modified helper");
  });
});

test("rejects type and symlink target mismatches before copying archive assets", async () => {
  const server = new TextEncoder().encode("llama server executable");
  const helper = new TextEncoder().encode("llama tts executable");
  const support = new TextEncoder().encode("support library");
  const archive = await tarGz({
    "release/llama-server": server,
    "release/llama-tts": helper,
    "release/libsupport.dylib": support,
    "release/libsupport.dylib.link": { linkname: "libsupport.dylib" },
  });

  await withArchive(archive, async (url) => {
    const pinned = release("llama-tts", "tar.gz", archive, url, 1);
    const typeMismatchRoot = createRoot();
    const typeMismatchPackage = packageDirectory(typeMismatchRoot, pinned);
    mkdirSync(typeMismatchPackage, { recursive: true });
    await Bun.write(join(typeMismatchPackage, "llama-tts"), helper);
    mkdirSync(join(typeMismatchPackage, "libsupport.dylib"));
    await expect(
      installManagedRuntime({ root: typeMismatchRoot }, pinned),
    ).rejects.toThrow("Refusing to replace existing managed runtime asset");
    expect(
      await Bun.file(join(typeMismatchPackage, "llama-server")).exists(),
    ).toBe(false);

    const linkMismatchRoot = createRoot();
    const linkMismatchPackage = packageDirectory(linkMismatchRoot, pinned);
    mkdirSync(linkMismatchPackage, { recursive: true });
    await Bun.write(join(linkMismatchPackage, "llama-tts"), helper);
    await Bun.write(join(linkMismatchPackage, "libsupport.dylib"), support);
    symlinkSync(
      "unexpected-target",
      join(linkMismatchPackage, "libsupport.dylib.link"),
    );
    await expect(
      installManagedRuntime({ root: linkMismatchRoot }, pinned),
    ).rejects.toThrow("Refusing to replace existing managed runtime asset");
    expect(
      await Bun.file(join(linkMismatchPackage, "llama-server")).exists(),
    ).toBe(false);
  });
});

test("installs a verified root-level tar.gz runtime", async () => {
  const binary = new TextEncoder().encode("whisper executable");
  const archive = await tarGz({ "whisper-server": binary });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const installed = await installManagedRuntime(
      { root },
      release("whisper-server", "tar.gz", archive, url, 0),
    );

    expect(await Bun.file(installed).bytes()).toEqual(binary);
  });
});

test("installs a verified root-level ZIP runtime without host archive utilities", async () => {
  const binary = new TextEncoder().encode("sd executable");
  const model = new TextEncoder().encode("runtime support file");
  const archive = zipSync({
    "sd-server": binary,
    "models/support.bin": model,
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const pinned = release("sd-server", "zip", archive, url, 0);
    const installed = await installManagedRuntime({ root }, pinned);

    expect(await Bun.file(installed).bytes()).toEqual(binary);
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "models", "support.bin"),
      ).bytes(),
    ).toEqual(model);
  });
});

test("rejects unverified downloads before they reach the managed bin directory", async () => {
  const archive = new TextEncoder().encode("unverified runtime");
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const pinned = release("whisper-server", "binary", archive, url, 0);
    pinned.sha256 = "0".repeat(64);

    await expect(installManagedRuntime({ root }, pinned)).rejects.toThrow(
      "Checksum mismatch",
    );
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "whisper-server"),
      ).exists(),
    ).toBe(false);
    const familyDir = dirname(packageDirectory(root, pinned));
    expect(
      readdirSync(familyDir).some((name) => name.startsWith(".install-")),
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
    const pinned = release("sd-server", "zip", archive, url, 0);
    await expect(installManagedRuntime({ root }, pinned)).rejects.toThrow(
      "Failed to extract",
    );
    expect(await Bun.file(join(root, "outside")).exists()).toBe(false);
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "sd-server"),
      ).exists(),
    ).toBe(false);
  });
});

test("rejects archive symlinks that would escape the staging directory", async () => {
  const archive = await tarGz({
    "release/llama-server": new TextEncoder().encode("llama executable"),
    "release/unsafe": { linkname: "../outside" },
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    await expect(
      installManagedRuntime(
        { root },
        release("llama-server", "tar.gz", archive, url, 1),
      ),
    ).rejects.toThrow("Failed to extract");
    expect(await Bun.file(join(root, "outside")).exists()).toBe(false);
  });
});

test("rejects archive files removed by stripComponents", async () => {
  const archive = await tarGz({
    "llama-server": new TextEncoder().encode("llama executable"),
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const pinned = release("llama-server", "tar.gz", archive, url, 1);
    await expect(installManagedRuntime({ root }, pinned)).rejects.toThrow(
      "Failed to extract",
    );
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "llama-server"),
      ).exists(),
    ).toBe(false);
  });
});

test("fails closed when a requested helper is absent from a verified archive", async () => {
  const archive = await tarGz({
    "release/llama-server": new TextEncoder().encode("llama executable"),
  });
  const root = createRoot();

  await withArchive(archive, async (url) => {
    const pinned = release("llama-tts", "tar.gz", archive, url, 1);
    await expect(installManagedRuntime({ root }, pinned)).rejects.toThrow(
      "llama-tts was not found after extracting",
    );
    expect(
      await Bun.file(
        join(packageDirectory(root, pinned), "llama-tts"),
      ).exists(),
    ).toBe(false);
  });
});
