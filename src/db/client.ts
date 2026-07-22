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

type OpenDatabase = {
  sqlite: Database;
  db: LocalBaseDatabase;
};

export class DatabaseSession {
  private readonly openDatabases = new Map<string, OpenDatabase>();

  get(root: string): LocalBaseDatabase {
    const path = databasePath(root);
    const existing = this.openDatabases.get(path);
    if (existing) return existing.db;

    mkdirSync(root, { recursive: true });
    const sqlite = new Database(path);
    const db = drizzle({ client: sqlite, schema });
    try {
      migrate(db, { migrationsFolder: migrationsFolder() });
      validateMigrationJournal(db, root);
    } catch (error) {
      sqlite.close();
      throw error;
    }
    this.openDatabases.set(path, { sqlite, db });
    return db;
  }

  closeRoot(root: string): void {
    const path = databasePath(root);
    this.openDatabases.get(path)?.sqlite.close();
    this.openDatabases.delete(path);
  }

  close(): void {
    for (const { sqlite } of this.openDatabases.values()) sqlite.close();
    this.openDatabases.clear();
  }
}

export function withDatabase<T>(
  session: DatabaseSession,
  root: string,
  operation: (db: LocalBaseDatabase) => T,
): T {
  return operation(session.get(root));
}
