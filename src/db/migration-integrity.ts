import { asc } from "drizzle-orm";
import { readMigrationFiles } from "drizzle-orm/migrator";
import { integer, sqliteTable, text } from "drizzle-orm/sqlite-core";
import type { LocalBaseDatabase } from "./client";
import { migrationsFolder } from "./migration-assets";

const drizzleMigrations = sqliteTable("__drizzle_migrations", {
  id: integer("id").primaryKey(),
  hash: text("hash").notNull(),
  createdAt: integer("created_at").notNull(),
});

/** Rejects database state that does not match this build's migration history. */
export function validateMigrationJournal(
  db: LocalBaseDatabase,
  root: string,
): void {
  const expected = readMigrationFiles({ migrationsFolder: migrationsFolder() });
  const actual = db
    .select({
      hash: drizzleMigrations.hash,
      createdAt: drizzleMigrations.createdAt,
    })
    .from(drizzleMigrations)
    .orderBy(asc(drizzleMigrations.createdAt))
    .all();

  if (
    actual.length !== expected.length ||
    actual.some(
      (record, index) =>
        record.hash !== expected[index]?.hash ||
        record.createdAt !== expected[index]?.folderMillis,
    )
  ) {
    throw new Error(
      `Invalid Drizzle migration journal in ${root}/local-base.db. Its hashes or order do not match this LocalBase build.`,
    );
  }
}
