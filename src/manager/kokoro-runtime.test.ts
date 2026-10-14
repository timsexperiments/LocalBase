import { afterEach, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { gzipSync } from "fflate";
import { pack } from "tar-stream";
import {
  ensureKokoroRuntime,
  kokoroRuntimeCommand,
  kokoroRuntimeRelease,
} from "./kokoro-runtime";
import type { ManagedRuntimeRelease } from "./managed-runtime-manifest";

const cleanups: Array<() => Promise<void> | void> = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

async function temp(): Promise<string> {
  const directory = await mkdtemp("/tmp/localbase-kokoro-runtime-");
  cleanups.push(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

const sha256 = (bytes: Uint8Array | string) =>
  createHash("sha256").update(bytes).digest("hex");

const packageContents = {
  "kokoro-tts": { body: "#!/bin/sh\nexit 0\n", executable: true },
  "bunfig.toml": { body: "[install]\nauto = false\n", executable: false },
  "cli.js": { body: "console.log('kokoro');\n", executable: false },
} as const;

async function fixtureArchive(
  overrides: Partial<Record<string, string>> = {},
): Promise<{ archive: Uint8Array; inventorySha256: string }> {
  const files = Object.entries(packageContents)
    .map(([path, { body, executable }]) => ({
      path,
      size: Buffer.byteLength(body),
      sha256: sha256(body),
      executable,
    }))
    .sort((left, right) => left.path.localeCompare(right.path));
  const inventory = `${JSON.stringify({ version: 1, target: "macos-arm64", files })}\n`;
  const tar = pack();
  const chunks: Uint8Array[] = [];
  const complete = new Promise<Uint8Array>((resolve, reject) => {
    tar.on("data", (chunk: Uint8Array) => chunks.push(chunk));
    tar.once("error", reject);
    tar.once("end", () => resolve(gzipSync(Buffer.concat(chunks))));
  });
  // Archive modes are deliberately wrong: the installer restores them from the
  // digest-verified inventory rather than trusting the archive.
  tar.entry({ name: "inventory.json", mode: 0o600 }, inventory);
  for (const [path, { body }] of Object.entries(packageContents))
    tar.entry({ name: path, mode: 0o600 }, overrides[path] ?? body);
  tar.finalize();
  return { archive: await complete, inventorySha256: sha256(inventory) };
}

function serve(archive: Uint8Array): string {
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => new Response(new Blob([archive as Uint8Array<ArrayBuffer>])),
  });
  cleanups.push(() => server.stop(true));
  return `http://127.0.0.1:${server.port}/kokoro-tts-macos-arm64.tar.gz`;
}

function release(
  archive: Uint8Array,
  inventorySha256: string,
  url: string,
): ManagedRuntimeRelease {
  return {
    name: "kokoro-tts",
    tag: "kokoro-v0.0.1",
    assetName: "kokoro-tts-macos-arm64.tar.gz",
    url,
    expectedSizeBytes: archive.length,
    sha256: sha256(archive),
    format: "tar.gz",
    stripComponents: 0,
    inventorySha256,
  };
}

test("the repository pins refuse to install until the Kokoro package is published", () => {
  for (const target of [
    { os: "darwin", cpu: "arm64" },
    { os: "linux", cpu: "x64" },
  ]) {
    expect(() => kokoroRuntimeRelease(target)).toThrow(
      "has not been published yet",
    );
  }
  expect(() => kokoroRuntimeRelease({ os: "darwin", cpu: "x64" })).toThrow(
    "no managed runtime package",
  );
});

test("installs a verified package with inventory modes and reuses it", async () => {
  const root = await temp();
  const { archive, inventorySha256 } = await fixtureArchive();
  const pinned = release(archive, inventorySha256, serve(archive));
  const runtime = await ensureKokoroRuntime({ root }, { release: pinned });

  expect(runtime.packageDir).toBe(
    join(root, "bin", "runtimes", "kokoro-tts", pinned.sha256),
  );
  expect(runtime.inventorySha256).toBe(inventorySha256);
  expect((await stat(runtime.executable)).mode & 0o777).toBe(0o755);
  expect((await stat(runtime.entrypoint)).mode & 0o777).toBe(0o644);
  expect((await stat(runtime.bunfig)).mode & 0o777).toBe(0o644);

  // A second call verifies the existing package without downloading again.
  const offline = { ...pinned, url: "http://127.0.0.1:9/unreachable" };
  expect(await ensureKokoroRuntime({ root }, { release: offline })).toEqual(
    runtime,
  );

  await writeFile(runtime.entrypoint, "console.log('tampered');\n");
  await expect(
    ensureKokoroRuntime({ root }, { release: offline }),
  ).rejects.toThrow("Remove it and retry");
});

test("rejects pending pins, archive digest mismatches, and inventory mismatches", async () => {
  const root = await temp();
  const { archive, inventorySha256 } = await fixtureArchive();
  const url = serve(archive);

  await expect(
    ensureKokoroRuntime(
      { root },
      {
        release: {
          ...release(archive, inventorySha256, url),
          pendingPublication: true,
        },
      },
    ),
  ).rejects.toThrow("has not been published yet");

  await expect(
    ensureKokoroRuntime(
      { root },
      {
        release: {
          ...release(archive, inventorySha256, url),
          sha256: "a".repeat(64),
        },
      },
    ),
  ).rejects.toThrow();

  await expect(
    ensureKokoroRuntime(
      { root },
      { release: release(archive, "b".repeat(64), url) },
    ),
  ).rejects.toThrow("inventory does not match");

  const tampered = await fixtureArchive({ "cli.js": "console.log('evil');\n" });
  await expect(
    ensureKokoroRuntime(
      { root },
      {
        release: release(
          tampered.archive,
          tampered.inventorySha256,
          serve(tampered.archive),
        ),
      },
    ),
  ).rejects.toThrow();
  // Failed installs leave no package or staging directory behind.
  const family = join(root, "bin", "runtimes", "kokoro-tts");
  expect(
    await Array.fromAsync(
      new Bun.Glob("*").scan({ cwd: family, onlyFiles: false, dot: true }),
    ),
  ).toEqual([]);
});

test("the runtime command passes the bunfig as one joined flag", () => {
  const command = kokoroRuntimeCommand({
    packageDir: "/runtime",
    executable: "/runtime/kokoro-tts",
    bunfig: "/runtime/bunfig.toml",
    entrypoint: "/runtime/cli.js",
    inventorySha256: "c".repeat(64),
  });
  // A separate "--config" argument makes the compiled runtime exit 0 without
  // running the entrypoint.
  expect(command).toEqual([
    "/runtime/kokoro-tts",
    "--no-install",
    "--no-env-file",
    "--config=/runtime/bunfig.toml",
    "/runtime/cli.js",
  ]);
  expect(command).not.toContain("--config");
});
