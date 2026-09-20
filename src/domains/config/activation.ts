import { and, eq } from "drizzle-orm";
import { configActivationTable, configTable } from "../../db/schema";
import {
  databasePath,
  openReadOnlyDatabase,
  type DatabaseSession,
  type LocalBaseDatabase,
} from "../../db/client";
import type { LocalBaseConfig } from "../../manager";

type StaticConfiguration = Pick<
  LocalBaseConfig,
  "gatewayHost" | "gatewayPort" | "memory"
>;

function fingerprint(config: StaticConfiguration): string {
  return new Bun.CryptoHasher("sha256")
    .update(
      JSON.stringify([
        config.gatewayHost,
        config.gatewayPort,
        config.memory.systemReserve.percent,
        config.memory.systemReserve.minimumGb,
        config.memory.acceleratorReserve.percent,
        config.memory.acceleratorReserve.minimumGb,
      ]),
    )
    .digest("hex");
}

export function savedStaticConfiguration(
  db: LocalBaseDatabase,
): StaticConfiguration | undefined {
  const row = db
    .select({
      gatewayHost: configTable.gatewayHost,
      gatewayPort: configTable.gatewayPort,
      systemPercent: configTable.memorySystemReservePercent,
      systemMinimumGb: configTable.memorySystemReserveMinimumGb,
      acceleratorPercent: configTable.memoryAcceleratorReservePercent,
      acceleratorMinimumGb: configTable.memoryAcceleratorReserveMinimumGb,
    })
    .from(configTable)
    .where(eq(configTable.id, "default"))
    .get();
  return (
    row && {
      gatewayHost: row.gatewayHost,
      gatewayPort: row.gatewayPort,
      memory: {
        systemReserve: {
          percent: row.systemPercent,
          minimumGb: row.systemMinimumGb,
        },
        acceleratorReserve: {
          percent: row.acceleratorPercent,
          minimumGb: row.acceleratorMinimumGb,
        },
      },
    }
  );
}

export function staticConfigurationChanged(
  before: StaticConfiguration | undefined,
  after: StaticConfiguration,
): boolean {
  return before === undefined || fingerprint(before) !== fingerprint(after);
}

/** Runs in the same transaction as the settings write, or under the restart operation lock. */
export function markRestartPending(
  db: LocalBaseDatabase,
  config: StaticConfiguration,
): void {
  const pendingStaticConfig = fingerprint(config);
  db.insert(configActivationTable)
    .values({ id: "default", pendingStaticConfig })
    .onConflictDoUpdate({
      target: configActivationTable.id,
      set: { pendingStaticConfig },
    })
    .run();
}

export async function restartPending(root: string): Promise<boolean> {
  if (!(await Bun.file(databasePath(root)).exists())) return false;
  const database = openReadOnlyDatabase(root);
  try {
    return (
      database.db
        .select({ id: configActivationTable.id })
        .from(configActivationTable)
        .where(eq(configActivationTable.id, "default"))
        .get() !== undefined
    );
  } finally {
    database.close();
  }
}

/** A managed, ready listener acknowledges its startup settings, never a later database snapshot. */
export function acknowledgeStaticConfiguration(
  database: DatabaseSession,
  root: string,
  started: StaticConfiguration,
): void {
  const db = database.get(root);
  db.transaction(
    () => {
      const saved = savedStaticConfiguration(db);
      if (staticConfigurationChanged(saved, started)) return;
      db.delete(configActivationTable)
        .where(
          and(
            eq(configActivationTable.id, "default"),
            eq(configActivationTable.pendingStaticConfig, fingerprint(started)),
          ),
        )
        .run();
    },
    { behavior: "immediate" },
  );
}
