import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession, databasePath } from "./client";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { migrate } from "drizzle-orm/bun-sqlite/migrator";
import { readMigrationFiles } from "drizzle-orm/migrator";
import { migrationsFolder } from "./migration-assets";
import { validateMigrationJournal } from "./migration-integrity";
import * as schema from "./schema";
import { defaultApiKeyScopes } from "../domains/auth/authorization";

test("migrates existing active, expired, and revoked keys without changing credentials or ownership", () => {
  const sqlite = new Database(":memory:");
  const db = drizzle({ client: sqlite, schema });
  const history = readMigrationFiles({ migrationsFolder: migrationsFolder() });
  try {
    sqlite.exec(
      "CREATE TABLE __drizzle_migrations (id INTEGER PRIMARY KEY, hash TEXT NOT NULL, created_at INTEGER)",
    );
    for (const migration of history.slice(0, 2)) {
      for (const statement of migration.sql) sqlite.exec(statement);
      sqlite
        .prepare(
          "INSERT INTO __drizzle_migrations (hash, created_at) VALUES (?, ?)",
        )
        .run(migration.hash, migration.folderMillis);
    }
    for (const [id, expires, revoked] of [
      ["active", null, null],
      ["expired", "2001-01-01T00:00:00.000Z", null],
      ["revoked", null, "2001-01-01T00:00:00.000Z"],
    ]) {
      sqlite
        .prepare("INSERT INTO api_keys VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
        .run(
          id,
          id,
          "lb_prefix",
          "a".repeat(64),
          "2000-01-01T00:00:00.000Z",
          "2000-01-01T00:00:00.000Z",
          expires,
          revoked,
        );
    }
    const credentials = sqlite.query(
      "SELECT id, name, prefix, key_hash, created_at, last_rotated_at, expires_at, revoked_at FROM api_keys ORDER BY id",
    );
    const before = credentials.all();
    migrate(db, { migrationsFolder: migrationsFolder() });
    validateMigrationJournal(db, ":memory:");
    expect(credentials.all()).toEqual(before);
    expect(
      db
        .select({ scopes: schema.apiKeysTable.scopes })
        .from(schema.apiKeysTable)
        .all(),
    ).toEqual(
      before.map(() => ({ scopes: JSON.stringify(defaultApiKeyScopes) })),
    );
    db.update(schema.apiKeysTable).set({ scopes: "[]" }).run();
    migrate(db, { migrationsFolder: migrationsFolder() });
    expect(
      db
        .select({ scopes: schema.apiKeysTable.scopes })
        .from(schema.apiKeysTable)
        .all(),
    ).toEqual([{ scopes: "[]" }, { scopes: "[]" }, { scopes: "[]" }]);
  } finally {
    sqlite.close();
  }
});

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
