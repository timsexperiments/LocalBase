import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import type { LogEventInput } from "../observability/logging";
import { defaultConfig, saveConfig } from "../../manager";
import { RuntimeConfigController } from "./config-snapshot";
import {
  RuntimeReconciler,
  RuntimeRequestAbortedError,
} from "./runtime-reconciler";
import type { RuntimeSupervisorFactory } from "./supervisor-factory";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";

type ServiceRecord = {
  modality: "llm" | "stt" | "image";
  modelId: string;
  memorySystemReservePercent: number;
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
        memorySystemReservePercent:
          snapshot.config.memory.systemReserve.percent,
        shutdowns: 0,
        service: {
          runtimeId: () => `${modality}:${modelId}`,
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
  const events: LogEventInput[] = [];
  const reconciler = new RuntimeReconciler(controller, {}, registry, factory, {
    event: (event: LogEventInput) => events.push(event),
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
    expect(events.map(({ eventName }) => eventName)).toContain(
      "runtime.reconciliation-failed",
    );

    const recoveredStt = controller.copy();
    recoveredStt.activeSttModel = "whisper-large-v3-turbo";
    recoveredStt.selectedSttModels = [recoveredStt.activeSttModel];
    saveConfig(database, recoveredStt);
    await reconciler.refresh();
    expect(registry.state("stt", true).state).toBe("idle");
    expect(records.filter(({ modality }) => modality === "stt")).toHaveLength(
      2,
    );

    const restartRequired = controller.copy();
    restartRequired.memory = {
      ...restartRequired.memory,
      systemReserve: { ...restartRequired.memory.systemReserve, percent: 20 },
    };
    const recordsBeforeRestartRequired = records.length;
    saveConfig(database, restartRequired);
    await reconciler.refresh();
    expect(records).toHaveLength(recordsBeforeRestartRequired);
    expect(
      records.map(
        ({ memorySystemReservePercent }) => memorySystemReservePercent,
      ),
    ).toEqual(Array(recordsBeforeRestartRequired).fill(15));
    expect(reconciler.read().config.memory.systemReserve.percent).toBe(15);
    expect(events.at(-1)).toMatchObject({
      eventName: "runtime.reconciliation-failed",
      message: "Runtime configuration change requires a gateway restart.",
    });
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("releases transition ownership before waiting for backend readiness", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-runtime-admission-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  const switchedModel = "qwen2.5-coder-7b-instruct-q4_k_m";
  config.selectedLlmModels = [config.activeLlmModel, switchedModel];
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  const lifecycle = { state: "idle" as "idle" | "starting" };
  let rejectStartup: (error: Error) => void;
  const startup = new Promise<void>((_resolve, reject) => {
    rejectStartup = reject;
  });
  let kills = 0;
  const initial: RuntimeSupervisor = {
    runtimeId: () => "llm:model:1",
    state: () => lifecycle.state,
    async ensureRunning() {
      lifecycle.state = "starting";
      await startup;
    },
    async kill() {
      kills += 1;
      rejectStartup!(new Error("Startup cancelled."));
    },
    async shutdown() {},
  };
  let replacements = 0;
  let abortNextStartup: AbortController | undefined;
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create: () => {
      replacements += 1;
      return {
        runtimeId: () => `llm:model:${replacements + 1}`,
        state: () => "idle",
        async ensureRunning() {
          abortNextStartup?.abort();
          abortNextStartup = undefined;
        },
        async kill() {},
        async shutdown() {},
      };
    },
  };
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm: initial }),
    factory,
    { event() {} } as never,
  );

  try {
    const first = await reconciler.admitModel("llm", config.activeLlmModel);
    if (first.kind !== "admitted") throw new Error("Expected admission.");
    expect(lifecycle.state).toBe("starting");
    const firstStartup = first.value.admission.ready.finally(
      first.value.admission.release,
    );

    const switched = await reconciler.admitModel("llm", switchedModel);

    await expect(firstStartup).rejects.toThrow("Startup cancelled.");
    expect(kills).toBe(1);
    expect(switched).toMatchObject({
      kind: "admitted",
      value: { modelId: switchedModel },
    });
    if (switched.kind !== "admitted") throw new Error("Expected admission.");
    await switched.value.admission.ready;
    switched.value.admission.release();

    const abortController = new AbortController();
    abortNextStartup = abortController;
    await expect(
      reconciler.admitModel(
        "llm",
        config.activeLlmModel,
        abortController.signal,
      ),
    ).rejects.toBeInstanceOf(RuntimeRequestAbortedError);

    const recovered = await reconciler.admitModel("llm", switchedModel);
    if (recovered.kind !== "admitted") throw new Error("Expected admission.");
    await recovered.value.admission.ready;
    recovered.value.admission.release();
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not stop a ready runtime while a model switch drains admission", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-runtime-ready-drain-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  const switchedModel = "qwen2.5-coder-7b-instruct-q4_k_m";
  config.selectedLlmModels = [config.activeLlmModel, switchedModel];
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  let state: "idle" | "running" = "idle";
  let kills = 0;
  const initial: RuntimeSupervisor = {
    runtimeId: () => "llm:model:1",
    state: () => state,
    async ensureRunning() {
      state = "running";
    },
    async kill() {
      kills += 1;
    },
    async shutdown() {},
  };
  const replacement: RuntimeSupervisor = {
    runtimeId: () => "llm:model:2",
    state: () => "idle",
    async ensureRunning() {},
    async kill() {},
    async shutdown() {},
  };
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create: () => replacement,
  };
  let markSwitching!: () => void;
  const switching = new Promise<void>((resolve) => {
    markSwitching = resolve;
  });
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm: initial }),
    factory,
    {
      event(event) {
        if (event.eventName === "model.switching") markSwitching();
      },
    } as never,
  );

  try {
    const active = await reconciler.admitModel("llm", config.activeLlmModel);
    if (active.kind !== "admitted") throw new Error("Expected admission.");
    await active.value.admission.ready;
    expect(state).toBe("running");

    const switched = reconciler.admitModel("llm", switchedModel);
    await switching;
    expect(kills).toBe(0);

    active.value.admission.release();
    const replacementAdmission = await switched;
    if (replacementAdmission.kind !== "admitted") {
      throw new Error("Expected replacement admission.");
    }
    replacementAdmission.value.admission.release();
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("cancels an orphaned running runtime after shared admissions settle", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-runtime-cancellation-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  let kills = 0;
  const supervisor: RuntimeSupervisor = {
    runtimeId: () => "llm:test:1",
    state: () => "running",
    async ensureRunning() {},
    async kill() {
      kills += 1;
    },
    async shutdown() {},
  };
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create: () => supervisor,
  };
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm: supervisor }),
    factory,
    { event() {} } as never,
  );

  try {
    const cancelled = await reconciler.admitModel("llm", config.activeLlmModel);
    const completed = await reconciler.admitModel("llm", config.activeLlmModel);
    if (cancelled.kind !== "admitted" || completed.kind !== "admitted") {
      throw new Error("Expected admissions.");
    }

    cancelled.value.admission.cancel();
    expect(kills).toBe(0);

    completed.value.admission.release();
    expect(kills).toBe(1);
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("evicts only running runtimes without admitted requests", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-runtime-eviction-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  config.selectedSttModels = [];
  config.activeSttModel = "";
  config.selectedImageModels = [];
  config.activeImageModel = "";
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  let kills = 0;
  const service: RuntimeSupervisor = {
    runtimeId: () => "llm:test:1",
    state: () => "running",
    async ensureRunning() {},
    async kill() {
      kills += 1;
    },
    async shutdown() {},
  };
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create: () => service,
  };
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm: service }),
    factory,
    { event() {} } as never,
  );

  try {
    const active = await reconciler.admitModel("llm", config.activeLlmModel);
    if (active.kind !== "admitted") throw new Error("Expected admission.");

    await reconciler.evictIdleRuntimes();
    expect(kills).toBe(0);

    active.value.admission.release();
    await reconciler.evictIdleRuntimes();
    expect(kills).toBe(1);

    const reattached = await reconciler.admitModel(
      "llm",
      config.activeLlmModel,
    );
    expect(reattached.kind).toBe("admitted");
    if (reattached.kind === "admitted") reattached.value.admission.release();
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("kills critical runtimes before awaiting active leases and reattaches", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-runtime-critical-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  config.selectedSttModels = [config.activeSttModel];
  config.selectedImageModels = [];
  config.activeImageModel = "";
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  const events: string[] = [];
  let releaseActive!: () => void;
  let resolveStartup!: () => void;
  const startup = new Promise<void>((resolve) => {
    resolveStartup = resolve;
  });
  let sttKills = 0;
  let sttState: "starting" | "idle" = "starting";
  const llm: RuntimeSupervisor = {
    runtimeId: () => "llm:test:1",
    state: () => "running",
    async ensureRunning() {},
    async kill() {
      events.push("llm-kill");
      releaseActive();
    },
    async shutdown() {},
  };
  const stt: RuntimeSupervisor = {
    runtimeId: () => "stt:test:1",
    state: () => sttState,
    async ensureRunning() {
      await startup;
    },
    async kill() {
      sttKills += 1;
      events.push("stt-kill");
      sttState = "idle";
      resolveStartup();
    },
    async shutdown() {},
  };
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create: (modality) => (modality === "llm" ? llm : stt),
  };
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm, stt }),
    factory,
    { event() {} } as never,
  );

  try {
    const active = await reconciler.admitModel("llm", config.activeLlmModel);
    if (active.kind !== "admitted") throw new Error("Expected LLM admission.");
    active.value.admission.markResponseStarted();
    releaseActive = () => {
      events.push("llm-release");
      active.value.admission.release();
    };

    const pending = await reconciler.admitModel("stt", config.activeSttModel);
    if (pending.kind !== "admitted") throw new Error("Expected STT admission.");
    void pending.value.admission.ready.finally(pending.value.admission.release);

    await reconciler.evictAllRuntimes();

    expect(events).toEqual(["llm-kill", "llm-release", "stt-kill"]);
    expect(sttKills).toBe(1);

    const reattached = await reconciler.admitModel(
      "llm",
      config.activeLlmModel,
    );
    expect(reattached.kind).toBe("admitted");
    if (reattached.kind === "admitted") reattached.value.admission.release();
  } finally {
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});
