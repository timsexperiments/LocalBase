import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { mkdtempSync, rmSync } from "node:fs";
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
