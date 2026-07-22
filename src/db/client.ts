import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { Database } from "bun:sqlite";
import { drizzle, type BunSQLiteDatabase } from "drizzle-orm/bun-sqlite";
import { migrate } from "drizzle-orm/bun-sqlite/migrator";
import { migrationsFolder } from "./migration-assets";
import { validateMigrationJournal } from "./migration-integrity";
import * as schema from "./schema";

export type LocalBaseDatabase = BunSQLiteDatabase<typeof schema>;

export function databasePath(root: string): string {
  return join(root, "local-base.db");
}

export function withDatabase<T>(
  root: string,
  operation: (db: LocalBaseDatabase) => T,
): T {
  mkdirSync(root, { recursive: true });
  const sqlite = new Database(databasePath(root));
  const db = drizzle({ client: sqlite, schema });
  try {
    migrate(db, { migrationsFolder: migrationsFolder() });
    validateMigrationJournal(db, root);
    return operation(db);
  } finally {
    sqlite.close();
  }
}
