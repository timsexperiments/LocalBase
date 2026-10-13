import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig, loadConfig, saveConfig } from "../../../manager";
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

test("service install consent preserves the persisted configuration", () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-service-consent-merge-"));
  roots.push(root);
  const database = new DatabaseSession();
  try {
    const persisted = defaultConfig(root);
    persisted.gatewayPort = 24567;
    persisted.activeLlmModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    persisted.selectedLlmModels = [persisted.activeLlmModel];
    persisted.activeSttModel = "";
    persisted.selectedSttModels = [];
    saveConfig(database, persisted);

    const staleDefaults = defaultConfig(root);
    const ctx = { config: staleDefaults, database } as Parameters<
      typeof persistInstallConsent
    >[1];
    persistInstallConsent({ installMissing: true }, ctx);

    expect(loadConfig(database, root)).toMatchObject({
      gatewayPort: 24567,
      activeLlmModel: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      selectedSttModels: [],
      installMissingModels: true,
    });
  } finally {
    database.close();
  }
});
