import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import type { LogEventInput } from "../observability/logging";
import { defaultConfig, saveConfig } from "../../manager";
import { RuntimeConfigController } from "./config-snapshot";
import { RuntimeReconciler } from "./runtime-reconciler";
import type { RuntimeSupervisorFactory } from "./supervisor-factory";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";

type ServiceRecord = {
  modality: "llm" | "stt" | "image";
  modelId: string;
  shutdowns: number;
  service: RuntimeSupervisor;
};

function activeModel(
  modality: ServiceRecord["modality"],
  config: ReturnType<RuntimeConfigController["read"]>["config"],
): string {
  if (modality === "llm") return config.activeLlmModel;
  if (modality === "stt") return config.activeSttModel;
  return config.activeImageModel;
}

test("coalesces revisions, isolates replacement, and recovers failed additions", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-runtime-reconciler-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  config.selectedSttModels = [];
  config.activeSttModel = "";
  config.selectedImageModels = [];
  config.activeImageModel = "";
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  const records: ServiceRecord[] = [];
  let failSttModel = "";
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create(modality, snapshot) {
      const modelId = activeModel(modality, snapshot.config);
      if (modality === "stt" && modelId === failSttModel) {
        throw new Error("STT launch plan is invalid");
      }
      const record: ServiceRecord = {
        modality,
        modelId,
        shutdowns: 0,
        service: {
          state: () => "idle",
          async ensureRunning() {},
          async kill() {},
          async shutdown() {
            record.shutdowns += 1;
          },
        },
      };
      records.push(record);
      return record.service;
    },
  };
  const initialLlm = factory.create("llm", controller.read());
  const registry = new SupervisorRegistry({ llm: initialLlm });
  const events: string[] = [];
  const reconciler = new RuntimeReconciler(controller, {}, registry, factory, {
    event: ({ eventName }: LogEventInput) => events.push(eventName),
  } as never);

  try {
    const enableStt = controller.copy();
    enableStt.activeSttModel = "whisper-large-v3-turbo";
    enableStt.selectedSttModels = [enableStt.activeSttModel];
    saveConfig(database, enableStt);
    await Promise.all([
      reconciler.refresh(),
      reconciler.refresh(),
      reconciler.refresh(),
    ]);
    expect(records.filter(({ modality }) => modality === "stt")).toHaveLength(
      1,
    );

    const replaceLlm = controller.copy();
    replaceLlm.parallel = 2;
    saveConfig(database, replaceLlm);
    await reconciler.refresh();
    expect(records.filter(({ modality }) => modality === "llm")).toHaveLength(
      2,
    );
    expect(records.filter(({ modality }) => modality === "stt")).toHaveLength(
      1,
    );
    expect(records[0]!.shutdowns).toBe(1);

    failSttModel = "whisper-tiny-en-q8_0";
    const failedStt = controller.copy();
    failedStt.activeSttModel = failSttModel;
    failedStt.selectedSttModels = [failSttModel];
    saveConfig(database, failedStt);
    await reconciler.refresh();
    expect(registry.state("stt", true).state).toBe("failed");
    expect(events).toContain("runtime.reconciliation-failed");

    const recoveredStt = controller.copy();
    recoveredStt.activeSttModel = "whisper-large-v3-turbo";
    recoveredStt.selectedSttModels = [recoveredStt.activeSttModel];
    saveConfig(database, recoveredStt);
    await reconciler.refresh();
    expect(registry.state("stt", true).state).toBe("idle");
    expect(records.filter(({ modality }) => modality === "stt")).toHaveLength(
      2,
    );
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});
