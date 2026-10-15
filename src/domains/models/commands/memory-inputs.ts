import { existsSync } from "node:fs";
import type { ModelSpec } from "../../../catalog";
import type { AppContext } from "../../../context";
import { databasePath, openReadOnlyDatabase } from "../../../db/client";
import { createObservedMemoryStore } from "../../runtime/observed-memory";
import { observedMemoryInputs, type ModelMemoryInputs } from "../memory-fit";

/**
 * Observed peaks the gateway recorded for these models. Listing commands run
 * without the database, so this opens it read-only and never migrates or
 * creates it; without one every figure is a measurement or an estimate.
 */
export function cliMemoryInputs(
  ctx: Pick<AppContext, "config">,
  models: readonly ModelSpec[],
): ReadonlyMap<string, ModelMemoryInputs> {
  if (!existsSync(databasePath(ctx.config.root))) return new Map();
  let database: ReturnType<typeof openReadOnlyDatabase> | undefined;
  try {
    database = openReadOnlyDatabase(ctx.config.root);
    return observedMemoryInputs(models, createObservedMemoryStore(database.db));
  } catch {
    // A database from before migration 0013 has no observed peaks yet.
    return new Map();
  } finally {
    database?.close();
  }
}
