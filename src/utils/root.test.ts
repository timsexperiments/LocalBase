import { expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { canonicalLocalBaseRoot, ensureLocalBaseRootMarker } from "./root";

test("canonicalizes existing and not-yet-created roots without creating them", () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-root-"));
  const existing = join(directory, "existing");
  const missing = join(existing, "not-yet-created");
  mkdirSync(existing);

  try {
    expect(canonicalLocalBaseRoot(existing)).toBe(realpathSync(existing));
    expect(canonicalLocalBaseRoot(missing)).toBe(
      join(realpathSync(existing), "not-yet-created"),
    );
    expect(existsSync(missing)).toBe(false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("canonicalizes symlink aliases through a missing descendant", () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-root-alias-"));
  const target = join(directory, "target");
  const alias = join(directory, "alias");
  mkdirSync(target);
  symlinkSync(target, alias);

  try {
    expect(canonicalLocalBaseRoot(join(alias, "new-root"))).toBe(
      join(realpathSync(target), "new-root"),
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test.skipIf(process.platform !== "darwin")(
  "canonicalizes the macOS /var lexical alias",
  () => {
    expect(canonicalLocalBaseRoot("/var")).toBe(realpathSync("/var"));
  },
);

test("initializes only an empty root and never marks unrelated contents", () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-root-marker-"));
  const empty = join(directory, "empty");
  const unrelated = join(directory, "unrelated");
  mkdirSync(empty);
  mkdirSync(unrelated);
  writeFileSync(join(unrelated, "keep.txt"), "keep");

  try {
    expect(ensureLocalBaseRootMarker(empty).root).toBe(realpathSync(empty));
    expect(existsSync(join(empty, ".localbase-root.json"))).toBe(true);
    expect(() => ensureLocalBaseRootMarker(unrelated)).toThrow("nonempty");
    expect(existsSync(join(unrelated, ".localbase-root.json"))).toBe(false);
    expect(Bun.file(join(unrelated, "keep.txt")).text()).resolves.toBe("keep");
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("recovers only an authenticated initialization lock with a dead owner", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-root-interrupted-"));
  const staleRoot = join(directory, "stale");
  const liveRoot = join(directory, "live");
  mkdirSync(staleRoot);
  mkdirSync(liveRoot);
  const exited = Bun.spawn([process.execPath, "-e", ""], {
    stdout: "ignore",
    stderr: "ignore",
  });
  await exited.exited;

  const writeOwner = (root: string, pid: number) => {
    const lock = join(root, ".localbase-initialize.lock");
    mkdirSync(lock, { mode: 0o700 });
    writeFileSync(
      join(lock, "owner.json"),
      JSON.stringify({
        version: 1,
        token: crypto.randomUUID(),
        root: realpathSync(root),
        rootHash: new Bun.CryptoHasher("sha256")
          .update(realpathSync(root))
          .digest("hex"),
        pid,
        startedAt: new Date().toISOString(),
      }),
      { mode: 0o600 },
    );
  };

  try {
    writeOwner(staleRoot, exited.pid);
    expect(ensureLocalBaseRootMarker(staleRoot).root).toBe(
      realpathSync(staleRoot),
    );

    writeOwner(liveRoot, process.pid);
    expect(() => ensureLocalBaseRootMarker(liveRoot)).toThrow(
      "already in progress",
    );
    expect(existsSync(join(liveRoot, ".localbase-root.json"))).toBe(false);
    expect(existsSync(join(liveRoot, ".localbase-initialize.lock"))).toBe(true);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("does not reclaim unauthenticated interrupted initialization metadata", () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-root-invalid-lock-"));
  const lock = join(root, ".localbase-initialize.lock");
  mkdirSync(lock, { mode: 0o700 });
  writeFileSync(join(lock, "owner.json"), "invalid", { mode: 0o600 });

  try {
    expect(() => ensureLocalBaseRootMarker(root)).toThrow(
      "already in progress",
    );
    expect(existsSync(lock)).toBe(true);
    expect(existsSync(join(root, ".localbase-root.json"))).toBe(false);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
