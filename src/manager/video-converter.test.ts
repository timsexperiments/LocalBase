import { afterEach, expect, spyOn, test } from "bun:test";
import { mkdtempSync, readdirSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { installManagedRuntime } from "./binaries";
import { ensureVideoConverter } from "./video-converter";
import { videoConverterRelease } from "./video-converter-release";

const roots: string[] = [];
function fetchImplementation(
  implementation: (...args: Parameters<typeof fetch>) => Promise<Response>,
): typeof fetch {
  return Object.assign(implementation, {
    preconnect: globalThis.fetch.preconnect,
  });
}
afterEach(() => {
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

function fixture() {
  const root = mkdtempSync(join(tmpdir(), "localbase-converter-"));
  roots.push(root);
  const binary = "fixture converter";
  const sha256 = (text: string) =>
    new Bun.CryptoHasher("sha256").update(text).digest("hex");
  const release = {
    name: "ffmpeg",
    tag: "fixture",
    assetName: "ffmpeg-fixture",
    url: "https://fixture.test/ffmpeg",
    expectedSizeBytes: binary.length,
    sha256: sha256(binary),
    format: "binary",
    stripComponents: 0,
  } satisfies Parameters<typeof installManagedRuntime>[1];
  const supportFiles = ["LICENSE", "README"].map((filename) => ({
    filename,
    url: `https://fixture.test/${filename}`,
    expectedSizeBytes: filename.length,
    sha256: sha256(filename),
  }));
  const packageDir = join(root, "bin", "runtimes", "ffmpeg", release.sha256);
  return { root, binary, release, supportFiles, packageDir };
}

test("pins converter binaries and notices for all four targets without inference runtimes", () => {
  for (const os of ["darwin", "linux"]) {
    for (const cpu of ["arm64", "x64"]) {
      const { release, supportFiles } = videoConverterRelease({ os, cpu });
      expect(release.name).toBe("ffmpeg");
      expect(release.format).toBe("binary");
      expect(release.url).toBe(
        `https://github.com/eugeneware/ffmpeg-static/releases/download/b6.1.1/ffmpeg-${os}-${cpu}`,
      );
      expect(release.sha256).toMatch(/^[a-f0-9]{64}$/);
      expect(release.expectedSizeBytes).toBeGreaterThan(0);
      expect(supportFiles.map((file) => file.filename)).toEqual([
        "LICENSE",
        "README",
      ]);
      for (const file of supportFiles) {
        expect(file.sha256).toMatch(/^[a-f0-9]{64}$/);
        expect(file.expectedSizeBytes).toBeGreaterThan(0);
        expect(file.url).toEndWith(`/${os}-${cpu}.${file.filename}`);
      }
    }
  }
  expect(() => videoConverterRelease({ os: "win32", cpu: "x64" })).toThrow(
    "No packaged video converter",
  );
});

test("coalesces installs, publishes verified notices together, and reuses offline", async () => {
  const f = fixture();
  const fetchMock = spyOn(globalThis, "fetch").mockImplementation(
    fetchImplementation(async (input) => {
      expect(await Bun.file(join(f.packageDir, "ffmpeg")).exists()).toBe(false);
      const name = new URL(String(input)).pathname.slice(1);
      return new Response(name === "ffmpeg" ? f.binary : name);
    }),
  );
  try {
    const paths = await Promise.all(
      [0, 1].map(() =>
        installManagedRuntime({ root: f.root }, f.release, {
          supportFiles: f.supportFiles,
        }),
      ),
    );
    expect(paths).toEqual([
      join(f.packageDir, "ffmpeg"),
      join(f.packageDir, "ffmpeg"),
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(statSync(paths[0] ?? "").mode & 0o111).toBe(0o111);
    expect(await Bun.file(join(f.packageDir, "LICENSE")).text()).toBe(
      "LICENSE",
    );
    expect(await Bun.file(join(f.packageDir, "README")).text()).toBe("README");
    fetchMock.mockImplementation(
      fetchImplementation(async () => {
        throw new Error("offline");
      }),
    );
    expect(
      await installManagedRuntime({ root: f.root }, f.release, {
        supportFiles: f.supportFiles,
      }),
    ).toBe(paths[0]);
    await Bun.write(join(f.packageDir, "LICENSE"), "CHANGED");
    await expect(
      installManagedRuntime({ root: f.root }, f.release, {
        supportFiles: f.supportFiles,
      }),
    ).rejects.toThrow("Checksum mismatch");
  } finally {
    fetchMock.mockRestore();
  }
});

test("rejects bad notices before publishing any converter files", async () => {
  const f = fixture();
  const fetchMock = spyOn(globalThis, "fetch").mockImplementation(
    fetchImplementation(
      async (input) =>
        new Response(String(input) === f.release.url ? f.binary : "CORRUPT"),
    ),
  );
  try {
    await expect(
      installManagedRuntime({ root: f.root }, f.release, {
        supportFiles: f.supportFiles,
      }),
    ).rejects.toThrow("Checksum mismatch");
    expect(readdirSync(dirname(f.packageDir))).toEqual([]);
  } finally {
    fetchMock.mockRestore();
  }
});

test("bounds downloads, cancels oversized bodies, and rejects short downloads", async () => {
  const f = fixture();
  let cancelled = false;
  const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(new Uint8Array(f.release.expectedSizeBytes + 1));
        },
        cancel() {
          cancelled = true;
        },
      }),
    ),
  );
  try {
    await expect(
      installManagedRuntime({ root: f.root }, f.release),
    ).rejects.toThrow("exceeds expected size");
    expect(cancelled).toBe(true);
    expect(readdirSync(dirname(f.packageDir))).toEqual([]);
    fetchMock.mockResolvedValue(new Response("short"));
    await expect(
      installManagedRuntime({ root: f.root }, f.release),
    ).rejects.toThrow("Size mismatch");
    expect(readdirSync(dirname(f.packageDir))).toEqual([]);
  } finally {
    fetchMock.mockRestore();
  }
});

test("rejects pre-aborted converter resolution without downloading", async () => {
  const f = fixture();
  const fetchMock = spyOn(globalThis, "fetch");
  try {
    await expect(
      ensureVideoConverter(f.root, AbortSignal.abort(new Error("cancelled"))),
    ).rejects.toThrow("cancelled");
    expect(fetchMock).not.toHaveBeenCalled();
    expect(readdirSync(f.root)).toEqual([]);
  } finally {
    fetchMock.mockRestore();
  }
});

test("aborts a queued install without cancelling the active caller", async () => {
  const f = fixture();
  const download = Promise.withResolvers<Response>();
  const started = Promise.withResolvers<void>();
  const fetchMock = spyOn(globalThis, "fetch").mockImplementation(
    fetchImplementation(() => {
      started.resolve();
      return download.promise;
    }),
  );
  try {
    const active = installManagedRuntime({ root: f.root }, f.release);
    await started.promise;
    const controller = new AbortController();
    const queued = installManagedRuntime({ root: f.root }, f.release, {
      signal: controller.signal,
    });
    controller.abort(new Error("queued cancellation"));
    await expect(queued).rejects.toThrow("queued cancellation");
    download.resolve(new Response(f.binary));
    expect(await active).toBe(join(f.packageDir, "ffmpeg"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  } finally {
    download.resolve(new Response(f.binary));
    fetchMock.mockRestore();
  }
});

test("aborts an active body download and leaves no published converter", async () => {
  const f = fixture();
  const started = Promise.withResolvers<void>();
  const controller = new AbortController();
  const fetchMock = spyOn(globalThis, "fetch").mockImplementation(
    fetchImplementation(async (_input, options) => {
      expect(options?.signal).toBe(controller.signal);
      return new Response(
        new ReadableStream<Uint8Array>({
          start(body) {
            body.enqueue(new TextEncoder().encode("partial"));
            controller.signal.addEventListener(
              "abort",
              () => body.error(controller.signal.reason),
              { once: true },
            );
            started.resolve();
          },
        }),
      );
    }),
  );
  try {
    const installation = installManagedRuntime({ root: f.root }, f.release, {
      signal: controller.signal,
    });
    await started.promise;
    controller.abort(new Error("download cancellation"));
    await expect(installation).rejects.toThrow("download cancellation");
    // A serialized retry waits for cancelled staging cleanup before publishing.
    fetchMock.mockResolvedValue(new Response(f.binary));
    expect(await installManagedRuntime({ root: f.root }, f.release)).toBe(
      join(f.packageDir, "ffmpeg"),
    );
    expect(readdirSync(dirname(f.packageDir))).toEqual([f.release.sha256]);
  } finally {
    fetchMock.mockRestore();
  }
});
