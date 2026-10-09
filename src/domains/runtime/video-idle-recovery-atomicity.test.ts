import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CATALOG } from "../../catalog";
import { DatabaseSession } from "../../db/client";
import { defaultConfig, saveConfig } from "../../manager";
import { useSupportedVideoHost } from "../../test/video-host";
import { RuntimeConfigController } from "./config-snapshot";
import { RuntimeReconciler } from "./runtime-reconciler";
import type { RuntimeSupervisorFactory } from "./supervisor-factory";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";
import type { RuntimeModality } from "./modality";

useSupportedVideoHost();

function setup() {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-atomicity-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  const otherLlm = CATALOG.find(
    (model) => model.kind === "llm" && model.modelId !== config.activeLlmModel,
  )?.modelId;
  const videoModel = CATALOG.find((model) => model.kind === "video")?.modelId;
  if (!otherLlm || !videoModel)
    throw new Error("Expected alternate LLM and video models.");
  config.selectedLlmModels = [config.activeLlmModel, otherLlm];
  config.selectedVideoModels = [videoModel];
  config.activeVideoModel = videoModel;
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  const kills: string[] = [];
  const projectionStarted = Promise.withResolvers<void>();
  const releaseProjection = Promise.withResolvers<void>();
  const llmPreflightStarted = Promise.withResolvers<void>();
  const releaseLlmPreflight = Promise.withResolvers<void>();
  let holdProjection = false;
  let holdLlmPreflight = false;

  function supervisor(
    modality: RuntimeModality,
    modelId: string,
  ): RuntimeSupervisor {
    const runtimeId = `${modality}:${modelId}`;
    return {
      kind: "server",
      runtimeId: () => runtimeId,
      state: () => "running",
      async ensureRunning() {},
      async kill() {
        kills.push(runtimeId);
      },
      async shutdown() {},
    };
  }

  const registry = new SupervisorRegistry({
    llm: supervisor("llm", config.activeLlmModel),
    stt: supervisor("stt", config.activeSttModel),
  });
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create(modality, snapshot) {
      const modelId =
        modality === "video"
          ? snapshot.config.activeVideoModel
          : modality === "llm"
            ? snapshot.config.activeLlmModel
            : snapshot.config.activeSttModel;
      return {
        ...supervisor(modality, modelId),
        async preflight() {
          if (modality === "video" && holdProjection) {
            projectionStarted.resolve();
            await releaseProjection.promise;
          }
          if (modality === "llm" && holdLlmPreflight) {
            llmPreflightStarted.resolve();
            await releaseLlmPreflight.promise;
          }
          return undefined;
        },
      };
    },
  };
  const reconciler = new RuntimeReconciler(controller, {}, registry, factory, {
    event() {},
  });
  return {
    config,
    controller,
    otherLlm,
    videoModel,
    reconciler,
    kills,
    projectionStarted: projectionStarted.promise,
    llmPreflightStarted: llmPreflightStarted.promise,
    holdProjection: () => (holdProjection = true),
    holdLlmPreflight: () => (holdLlmPreflight = true),
    releaseProjection: () => releaseProjection.resolve(),
    releaseLlmPreflight: () => releaseLlmPreflight.resolve(),
    cleanup() {
      releaseProjection.resolve();
      releaseLlmPreflight.resolve();
      reconciler.closeQueues();
      database.close();
      rmSync(root, { recursive: true, force: true });
    },
  };
}

test("failed activation cannot reopen a recovery-claimed peer to active requests", async () => {
  const f = setup();
  f.holdProjection();
  f.holdLlmPreflight();
  try {
    const recovery = f.reconciler.recoverWithIdleEviction(
      "video",
      f.videoModel,
    );
    await f.projectionStarted;
    const switching = f.reconciler.admitModel("llm", f.otherLlm);
    await f.llmPreflightStarted;
    await f.controller.update((config) => {
      config.selectedLlmModels = config.selectedLlmModels.filter(
        (modelId) => modelId !== f.otherLlm,
      );
    });
    f.releaseLlmPreflight();
    expect((await switching).kind).toBe("model-not-found");

    const originalRequest = await f.reconciler.admitModel(
      "llm",
      f.config.activeLlmModel,
    );
    expect(originalRequest.kind).toBe("admitted");
    if (originalRequest.kind !== "admitted")
      throw new Error("Expected the original model request admission.");
    await originalRequest.value.admission.ready;
    originalRequest.value.admission.markResponseStarted();

    f.releaseProjection();
    expect(await recovery).toBe(false);
    expect(f.kills).toEqual([]);
    expect(originalRequest.value.admission.supervisor.state()).toBe("running");
    originalRequest.value.admission.release();
  } finally {
    f.cleanup();
  }
});

test("aborting while waiting for final stop locks releases claims without killing", async () => {
  const f = setup();
  f.holdProjection();
  f.holdLlmPreflight();
  const abort = new AbortController();
  try {
    const recovery = f.reconciler.recoverWithIdleEviction(
      "video",
      f.videoModel,
      abort.signal,
    );
    const outcome = recovery.then(
      () => "resolved",
      (error: unknown) => (error instanceof Error ? error.name : String(error)),
    );
    await f.projectionStarted;
    const switching = f.reconciler.admitModel("llm", f.otherLlm);
    await f.llmPreflightStarted;
    f.releaseProjection();
    await new Promise((resolve) => setTimeout(resolve, 0));
    abort.abort();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(f.reconciler.lifecycleSnapshot().stt.admission).toMatchObject({
      accepting: true,
      activeCount: 0,
    });
    expect(f.kills).toEqual([]);
    expect(await outcome).toBe("RuntimeRequestAbortedError");

    f.releaseLlmPreflight();
    const switched = await switching;
    if (switched.kind === "admitted") switched.value.admission.release();
    expect(f.kills).toEqual([]);
    expect(f.reconciler.lifecycleSnapshot().llm.admission).toMatchObject({
      accepting: true,
      activeCount: 0,
    });
  } finally {
    f.cleanup();
  }
});
