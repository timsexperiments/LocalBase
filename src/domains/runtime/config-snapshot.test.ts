import { expect, test } from "bun:test";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import { defaultConfig, readConfig, saveConfig } from "../../manager";
import { RuntimeConfigController } from "./config-snapshot";

function withController(
  run: (
    controller: RuntimeConfigController,
    database: DatabaseSession,
    root: string,
    config: ReturnType<typeof defaultConfig>,
  ) => Promise<void> | void,
): Promise<void> {
  const root = mkdtempSync(join(tmpdir(), "local-base-runtime-config-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  return Promise.resolve(run(controller, database, root, config)).finally(
    () => {
      database.close();
      rmSync(root, { recursive: true, force: true });
    },
  );
}

test("runtime configuration snapshots are immutable and detached from inputs", async () => {
  await withController((controller, _database, _root, source) => {
    const snapshot = controller.read();
    source.selectedLlmModels.push("other-model");
    source.parallel = 2;

    expect(snapshot.config.selectedLlmModels).not.toContain("other-model");
    expect(snapshot.config.parallel).toBe("auto");
    expect(Object.isFrozen(snapshot)).toBe(true);
    expect(Object.isFrozen(snapshot.config)).toBe(true);
    expect(Object.isFrozen(snapshot.config.selectedLlmModels)).toBe(true);
    expect(() => {
      Reflect.apply(Array.prototype.push, snapshot.config.selectedLlmModels, [
        "other-model",
      ]);
    }).toThrow();
  });
});

test("runtime configuration refresh is read-only and revisions change only with configuration", async () => {
  await withController(async (controller, database, root) => {
    const initial = controller.read();
    const entriesBeforeRefresh = readdirSync(root).sort();
    expect(await controller.refresh()).toBe(initial);
    expect(readdirSync(root).sort()).toEqual(entriesBeforeRefresh);

    const externallyUpdated = await readConfig(root);
    externallyUpdated.parallel = 2;
    saveConfig(database, externallyUpdated);

    const updated = await controller.refresh();
    expect(updated.revision).toBe(initial.revision + 1);
    expect(updated.config.parallel).toBe(2);
    expect(await controller.refresh()).toBe(updated);
  });
});

test("runtime configuration updates persist without allowing root changes", async () => {
  await withController(async (controller, _database, root) => {
    const updated = controller.update((config) => {
      config.parallel = 2;
    });
    expect(updated.revision).toBe(1);
    expect((await readConfig(root)).parallel).toBe(2);

    expect(() =>
      controller.update((config) => {
        config.root = join(root, "another-root");
      }),
    ).toThrow("Runtime configuration cannot change the process root.");
    expect(controller.read()).toBe(updated);
  });
});
