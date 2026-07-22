import { asc, eq } from "drizzle-orm";
import { migrate } from "drizzle-orm/bun-sqlite/migrator";
import { readMigrationFiles } from "drizzle-orm/migrator";
import { integer, sqliteTable, text } from "drizzle-orm/sqlite-core";
import {
  bootstrapMigrationsFolder,
  migrationsFolder,
} from "./migration-assets";
import type { LocalBaseDatabase } from "./client";

const sqliteMaster = sqliteTable("sqlite_master", {
  type: text("type").notNull(),
  name: text("name").notNull(),
  sql: text("sql"),
});

const drizzleMigrations = sqliteTable("__drizzle_migrations", {
  id: integer("id").primaryKey(),
  hash: text("hash").notNull(),
  createdAt: integer("created_at").notNull(),
});

const legacyConfigColumns = [
  "id text primary key",
  "root text not null",
  "llm_models_dir text not null",
  "stt_models_dir text not null",
  "image_models_dir text",
  "runtime_backend text not null",
  "stt_backend text not null",
  "host text not null",
  "port integer not null",
  "ctx_size integer not null",
  "stt_host text not null",
  "stt_port integer not null",
  "startup_on_boot integer not null",
  "selected_llm_models text not null",
  "selected_stt_models text not null",
  "selected_image_models text",
  "active_llm_model text not null",
  "active_stt_model text not null",
  "active_image_model text",
  "system_prompt text",
  "hf_token text",
];

const strictCurrentConfigColumns = [
  "id text primary key",
  "root text not null",
  "llm_models_dir text not null",
  "stt_models_dir text not null",
  "image_models_dir text not null",
  "runtime_backend text not null",
  "stt_backend text not null",
  "host text not null",
  "port integer not null",
  "ctx_size integer not null",
  "stt_host text not null",
  "stt_port integer not null",
  "startup_on_boot integer not null",
  "selected_llm_models text not null",
  "selected_stt_models text not null",
  "selected_image_models text not null",
  "active_llm_model text not null",
  "active_stt_model text not null",
  "active_image_model text not null",
  "system_prompt text not null",
  "hf_token text not null",
  "parallel text not null default auto",
];

const generatedCurrentConfigColumns = [
  "id text primary key not null",
  ...legacyConfigColumns.slice(1),
  "parallel text default auto not null",
];

const legacyUpgradedConfigColumns = [
  ...legacyConfigColumns,
  "parallel text default auto not null",
];

const generatedLegacyConfigColumns = [
  "id text primary key not null",
  ...legacyConfigColumns.slice(1),
];

const legacyApiKeyColumns = [
  "id text primary key",
  "name text not null",
  "prefix text not null",
  "key_hash text not null",
  "created_at text not null",
  "last_rotated_at text not null",
  "expires_at text",
  "revoked_at text",
];

const generatedApiKeyColumns = [
  "id text primary key not null",
  ...legacyApiKeyColumns.slice(1),
];

function normalizeColumn(definition: string): string {
  return definition
    .toLowerCase()
    .replace(/["'`\[\]]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

function tableColumns(createSql: string | null): string[] | undefined {
  if (!createSql) return undefined;
  const start = createSql.indexOf("(");
  const end = createSql.lastIndexOf(")");
  if (start === -1 || end <= start) return undefined;
  return createSql
    .slice(start + 1, end)
    .split(",")
    .map((definition) => normalizeColumn(definition));
}

function hasExactColumns(
  createSql: string | null | undefined,
  expected: string[],
): boolean {
  const actual = tableColumns(createSql ?? null);
  return (
    actual !== undefined &&
    actual.length === expected.length &&
    actual.every((column, index) => column === expected[index])
  );
}

function isKnownApiKeyTable(createSql: string | null | undefined): boolean {
  return (
    hasExactColumns(createSql, legacyApiKeyColumns) ||
    hasExactColumns(createSql, generatedApiKeyColumns)
  );
}

function isKnownCurrentConfigTable(
  createSql: string | null | undefined,
): boolean {
  return [
    strictCurrentConfigColumns,
    generatedCurrentConfigColumns,
    legacyUpgradedConfigColumns,
  ].some((columns) => hasExactColumns(createSql, columns));
}

function unsupportedSchema(root: string): Error {
  return new Error(
    `Unsupported LocalBase database schema in ${root}/local-base.db. ` +
      "Expected an empty database, the known legacy config schema without parallel, or the pre-Drizzle current schema with parallel. " +
      `Back up the database and run "local-base reset --root ${root} --yes".`,
  );
}

function generatedMigrationHistory() {
  const migrations = readMigrationFiles({
    migrationsFolder: migrationsFolder(),
  });
  if (migrations.length < 2) {
    throw new Error("LocalBase's generated migration history is incomplete.");
  }
  return migrations;
}

function validateJournal(
  db: LocalBaseDatabase,
  migrations: ReturnType<typeof generatedMigrationHistory>,
  root: string,
): number {
  const records = db
    .select({
      hash: drizzleMigrations.hash,
      createdAt: drizzleMigrations.createdAt,
    })
    .from(drizzleMigrations)
    .orderBy(asc(drizzleMigrations.createdAt))
    .all();
  if (
    records.length > migrations.length ||
    records.some(
      (record, index) =>
        record.hash !== migrations[index]?.hash ||
        record.createdAt !== migrations[index]?.folderMillis,
    )
  ) {
    throw new Error(
      `Invalid Drizzle migration journal in ${root}/local-base.db. ` +
        "Its hashes or order do not match LocalBase's generated migrations.",
    );
  }
  return records.length;
}

function recordBaseline(
  db: LocalBaseDatabase,
  migrations: ReturnType<typeof generatedMigrationHistory>,
): void {
  migrate(db, { migrationsFolder: bootstrapMigrationsFolder() });
  db.transaction((tx) => {
    tx.insert(drizzleMigrations)
      .values(
        migrations.map((migration) => ({
          hash: migration.hash,
          createdAt: migration.folderMillis,
        })),
      )
      .run();
  });
}

/**
 * Recognizes only databases produced by LocalBase before Drizzle. It records a
 * matching baseline so the regular Drizzle migrator can safely take over.
 */
export function adoptLegacyDatabase(db: LocalBaseDatabase, root: string): void {
  const tables = new Map(
    db
      .select({ name: sqliteMaster.name, sql: sqliteMaster.sql })
      .from(sqliteMaster)
      .where(eq(sqliteMaster.type, "table"))
      .all()
      .map((table) => [table.name, table.sql]),
  );
  const configSql = tables.get("config");
  const apiKeysSql = tables.get("api_keys");
  const hasJournal = tables.has("__drizzle_migrations");

  if (hasJournal) {
    const appliedMigrations = validateJournal(
      db,
      generatedMigrationHistory(),
      root,
    );
    if (
      appliedMigrations === 0 &&
      ((!configSql && !apiKeysSql) ||
        (hasExactColumns(configSql, legacyConfigColumns) &&
          (!apiKeysSql || isKnownApiKeyTable(apiKeysSql))))
    ) {
      return;
    }
    if (
      appliedMigrations === 1 &&
      hasExactColumns(configSql, generatedLegacyConfigColumns) &&
      hasExactColumns(apiKeysSql, generatedApiKeyColumns)
    ) {
      return;
    }
    if (
      appliedMigrations === 2 &&
      isKnownCurrentConfigTable(configSql) &&
      isKnownApiKeyTable(apiKeysSql)
    ) {
      return;
    }
    throw unsupportedSchema(root);
  }

  if (!configSql && !apiKeysSql) return;
  if (!configSql || (apiKeysSql && !isKnownApiKeyTable(apiKeysSql))) {
    throw unsupportedSchema(root);
  }

  if (
    hasExactColumns(configSql, legacyConfigColumns) &&
    (!apiKeysSql || isKnownApiKeyTable(apiKeysSql))
  ) {
    return;
  }

  if (isKnownCurrentConfigTable(configSql)) {
    if (!apiKeysSql || !isKnownApiKeyTable(apiKeysSql)) {
      throw unsupportedSchema(root);
    }
    recordBaseline(db, generatedMigrationHistory());
    return;
  }

  throw unsupportedSchema(root);
}
