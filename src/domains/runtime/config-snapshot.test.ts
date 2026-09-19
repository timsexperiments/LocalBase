import { expect, test } from "bun:test";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import {
  defaultConfig,
  readConfig,
  readConfigSync,
  saveConfig,
} from "../../manager";
import { withRootOperation } from "../service/ownership";
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
    source.memory.systemReserve.percent = 20;

    expect(snapshot.config.selectedLlmModels).not.toContain("other-model");
    expect(snapshot.config.parallel).toBe("auto");
    expect(snapshot.config.memory.systemReserve.percent).toBe(15);
    expect(Object.isFrozen(snapshot)).toBe(true);
    expect(Object.isFrozen(snapshot.config)).toBe(true);
    expect(Object.isFrozen(snapshot.config.selectedLlmModels)).toBe(true);
    expect(Object.isFrozen(snapshot.config.memory)).toBe(true);
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
    const updated = await controller.update((config) => {
      config.parallel = 2;
    });
    expect(updated.revision).toBe(1);
    expect((await readConfig(root)).parallel).toBe(2);

    await expect(
      controller.update((config) => {
        config.root = join(root, "another-root");
      }),
    ).rejects.toThrow("Runtime configuration cannot change the process root.");
    expect(controller.read()).toBe(updated);
  });
});

test("runtime updates preserve configuration saved by a concurrent root operation", async () => {
  await withController(async (controller, database, root) => {
    const runtimeSelectedModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    let runtimeUpdate: ReturnType<typeof controller.update> | undefined;
    await withRootOperation(root, "test", async () => {
      runtimeUpdate = controller.update((config) => {
        config.activeLlmModel = runtimeSelectedModel;
      });
      const external = await readConfig(root);
      external.ctxSize = 8192;
      external.selectedLlmModels = [
        ...external.selectedLlmModels,
        runtimeSelectedModel,
      ];
      saveConfig(database, external);
    });

    await runtimeUpdate;
    expect(await readConfig(root)).toMatchObject({
      activeLlmModel: runtimeSelectedModel,
      ctxSize: 8192,
    });
  });
});

test("updates preserve externally persisted settings without an explicit refresh", async () => {
  await withController(async (controller, database, root) => {
    const external = readConfigSync(root);
    external.parallel = 3;
    saveConfig(database, external);
    expect(controller.copy().parallel).not.toBe(3);
    await controller.update((config) => {
      config.otelSampleRatio = 50;
    });
    expect(readConfigSync(root)).toMatchObject({
      parallel: 3,
      otelSampleRatio: 50,
    });
  });
});

test("failed transactional updates leave persisted config and snapshot unchanged", async () => {
  await withController(async (controller, _database, root) => {
    const snapshot = controller.read();
    await expect(
      controller.update((config) => {
        config.parallel = 3;
        throw new Error("abort update");
      }),
    ).rejects.toThrow("abort update");
    expect(readConfigSync(root).parallel).toBe(snapshot.config.parallel);
    expect(controller.read()).toBe(snapshot);
  });
});
