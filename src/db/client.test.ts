import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession, databasePath } from "./client";

test("migrates once per root within a database session", () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-db-session-"));
  const session = new DatabaseSession();
  try {
    const first = session.get(root);
    const external = new Database(databasePath(root));
    external
      .prepare("UPDATE __drizzle_migrations SET hash = ?")
      .run("tampered");
    external.close();

    expect(session.get(root)).toBe(first);
    session.closeRoot(root);
    expect(() => session.get(root)).toThrow("migration journal");
  } finally {
    session.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("uses one database identity for canonical root aliases", () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-db-alias-"));
  const target = join(directory, "target");
  const alias = join(directory, "alias");
  const session = new DatabaseSession();
  mkdirSync(target);
  symlinkSync(target, alias);

  try {
    expect(databasePath(alias)).toBe(databasePath(target));
    expect(session.get(alias)).toBe(session.get(target));
  } finally {
    session.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
