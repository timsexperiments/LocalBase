import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig, loadConfig } from "../../../manager";
import { installMissingModel } from "../../runtime/startup-preflight";
import { persistInstallConsent } from "./lifecycle";

const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

test("service install consent persists and is available to a later serve", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-service-consent-"));
  roots.push(root);
  const database = new DatabaseSession();
  try {
    const config = defaultConfig(root);
    const ctx = { config, database } as Parameters<
      typeof persistInstallConsent
    >[1];
    persistInstallConsent({ installMissing: true }, ctx);
    const laterServeConfig = loadConfig(database, root);
    expect(laterServeConfig.installMissingModels).toBe(true);
    let installations = 0;
    await expect(
      installMissingModel(
        laterServeConfig,
        undefined,
        "fixture-model",
        laterServeConfig.installMissingModels,
        async () => {
          installations += 1;
          return "/fixture/model";
        },
      ),
    ).resolves.toBe("/fixture/model");
    expect(installations).toBe(1);
  } finally {
    database.close();
  }
});
