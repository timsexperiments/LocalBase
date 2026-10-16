import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession, databasePath, openReadOnlyDatabase } from "./client";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { migrate } from "drizzle-orm/bun-sqlite/migrator";
import { readMigrationFiles } from "drizzle-orm/migrator";
import { migrationsFolder } from "./migration-assets";
import { validateMigrationJournal } from "./migration-integrity";
import * as schema from "./schema";
import { defaultConfig, loadConfig, saveConfig } from "../manager";

test("adds the default-off experimental opt-in without replacing the config row", () => {
  const root = mkdtempSync(
    join(tmpdir(), "local-base-experimental-migration-"),
  );
  const session = new DatabaseSession();
  const sqlite = new Database(":memory:");
  try {
    saveConfig(session, defaultConfig(root));
    const source = new Database(databasePath(root), { readonly: true });
    const row = source.query("SELECT * FROM config").get() as Record<
      string,
      unknown
    >;
    source.close();
    delete row.allow_experimental;
    delete row.ctx_mode;
    const history = readMigrationFiles({
      migrationsFolder: migrationsFolder(),
    });
    const experimentalMigration = history.findIndex((migration) =>
      migration.sql.some((statement) =>
        statement.includes("allow_experimental"),
      ),
    );
    if (experimentalMigration < 0) {
      throw new Error("Missing allow_experimental migration");
    }
    sqlite.exec(
      "CREATE TABLE __drizzle_migrations (id INTEGER PRIMARY KEY, hash TEXT NOT NULL, created_at INTEGER)",
    );
    for (const migration of history.slice(0, experimentalMigration)) {
      for (const statement of migration.sql) sqlite.exec(statement);
      sqlite
        .prepare(
          "INSERT INTO __drizzle_migrations (hash, created_at) VALUES (?, ?)",
        )
        .run(migration.hash, migration.folderMillis);
    }
    const columns = Object.keys(row);
    sqlite
      .prepare(
        `INSERT INTO config (${columns.join(",")}) VALUES (${columns.map(() => "?").join(",")})`,
      )
      .run(...(Object.values(row) as (string | number | null)[]));
    migrate(drizzle({ client: sqlite, schema }), {
      migrationsFolder: migrationsFolder(),
    });
    expect(sqlite.query("SELECT * FROM config").get()).toEqual({
      ...row,
      allow_experimental: 0,
      ctx_mode: "auto",
    });
    const config = loadConfig(session, root);
    config.allowExperimental = true;
    saveConfig(session, config);
    expect(loadConfig(session, root).allowExperimental).toBe(true);
  } finally {
    sqlite.close();
    session.close();
    rmSync(root, { recursive: true, force: true });
  }
});

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
      // Migration 0002 grants the default scopes of its release; later scopes
      // such as inference:decision are opt-in for existing keys.
      before.map(() => ({
        scopes: JSON.stringify([
          "inference:chat",
          "inference:embeddings",
          "inference:image",
          "inference:video",
          "inference:speech",
          "inference:transcription",
          "models:read",
        ]),
      })),
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

/** Holds an exclusive lock from another process, like a gateway committing a write. */
async function holdExclusiveLock(path: string, holdMs: number) {
  const holder = Bun.spawn(
    [
      process.execPath,
      "-e",
      [
        'import { Database } from "bun:sqlite";',
        "const db = new Database(Bun.argv.at(-2));",
        'db.exec("BEGIN EXCLUSIVE");',
        'process.stdout.write("locked\\n");',
        "Bun.sleepSync(Number(Bun.argv.at(-1)));",
        'db.exec("COMMIT");',
        "db.close();",
      ].join(""),
      path,
      String(holdMs),
    ],
    { stdout: "pipe", stderr: "inherit" },
  );
  const reader = holder.stdout.getReader();
  let output = "";
  while (!output.includes("locked")) {
    const { done, value } = await reader.read();
    if (done) throw new Error("Lock holder exited before locking.");
    output += new TextDecoder().decode(value);
  }
  reader.releaseLock();
  return holder;
}

test("waits for another process's write lock instead of failing with SQLITE_BUSY", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-db-busy-"));
  const setup = new DatabaseSession();
  const readers = new DatabaseSession();
  try {
    saveConfig(setup, defaultConfig(root, 0));
    setup.close();

    const holder = await holdExclusiveLock(databasePath(root), 300);
    try {
      // A fresh session migrates first, which is where CI saw SQLITE_BUSY.
      expect(() => loadConfig(readers, root)).not.toThrow();
    } finally {
      await holder.exited;
    }
    expect(holder.exitCode).toBe(0);

    const readOnlyHolder = await holdExclusiveLock(databasePath(root), 300);
    const readonly = openReadOnlyDatabase(root);
    try {
      expect(readonly.db.select().from(schema.configTable).get()?.id).toBe(
        "default",
      );
    } finally {
      readonly.close();
      await readOnlyHolder.exited;
    }
    expect(readOnlyHolder.exitCode).toBe(0);
  } finally {
    setup.close();
    readers.close();
    rmSync(root, { recursive: true, force: true });
  }
});
