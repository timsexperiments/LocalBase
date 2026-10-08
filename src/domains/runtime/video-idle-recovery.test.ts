import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { CATALOG } from "../../catalog";
import { DatabaseSession } from "../../db/client";
import { defaultConfig, saveConfig } from "../../manager";
import { createLogger } from "../observability/logging";
import { createOtelRuntime, OtelRuntimeHolder } from "../observability/otel";
import { admitVideoWithIdleRecovery } from "./commands/serve";
import { defaultMemorySafetyConfig, gibibyte } from "./memory-safety";
import {
  MemorySafetyController,
  RuntimeMemoryAdmissionError,
} from "./memory-controller";
import { RuntimeConfigController } from "./config-snapshot";
import { RuntimeReconciler } from "./runtime-reconciler";
import type { RuntimeSupervisorFactory } from "./supervisor-factory";
import { createRuntimeSupervisorFactory } from "./supervisor-factory";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";

function insufficientMemory(): RuntimeMemoryAdmissionError {
  return new RuntimeMemoryAdmissionError({
    kind: "rejected",
    reason: "system-memory",
    poolId: "system",
  });
}

function testSupervisor(
  runtimeId: string,
  state: () => "running" | "idle",
  kill: () => Promise<void>,
  shutdown: () => Promise<void> = async () => {},
): RuntimeSupervisor {
  return {
    kind: "server",
    runtimeId: () => runtimeId,
    state,
    async ensureRunning() {},
    kill,
    shutdown,
  };
}

test("video recovery preserves idle peers when releasing them cannot admit the request", async () => {
  const root = mkdtempSync("/tmp/localbase-video-idle-recovery-");
  const database = new DatabaseSession();
  let freeBytes = 48 * gibibyte;
  const memorySafety = new MemorySafetyController(
    {
      topology: {
        kind: "unified",
        system: { id: "system", capacityBytes: 48 * gibibyte },
      },
      async snapshot() {
        return {
          capturedAtMs: Date.now(),
          pools: [
            {
              poolId: "system",
              availability: "available",
              availableBytes: freeBytes,
              pressure: "normal",
            },
          ],
        };
      },
      async close() {},
    },
    defaultMemorySafetyConfig(),
  );
  const otel = new OtelRuntimeHolder(
    createOtelRuntime({
      enabled: false,
      headers: {},
      tracesHeaders: {},
      logsHeaders: {},
      sampleRatio: 1,
      sampler: "always_on",
      source: "persistent",
      displayEndpoint: "disabled",
    }),
  );
  const config = defaultConfig(root, 48);
  const factory = createRuntimeSupervisorFactory(
    {
      logger: createLogger(),
      otel,
      specs: {
        osName: "test",
        ramGb: 48,
        cpuModel: "test",
        gpuName: "test",
        gpuVramGb: 48,
        isMac: true,
        isAppleSilicon: true,
      },
    },
    {},
    { memorySafety },
  );
  const resident = async (runtimeId: string, gb: number) => {
    const reservation = await memorySafety.reserve({
      runtimeId,
      demand: {
        unifiedBytes: gb * gibibyte,
        hostBytes: gb * gibibyte,
        acceleratorBytes: gb * gibibyte,
        confidence: "estimated",
      },
    });
    reservation.materialize();
    freeBytes -= gb * gibibyte;
    return () => {
      reservation.release();
      freeBytes += gb * gibibyte;
    };
  };

  try {
    config.selectedSttModels = [config.activeSttModel];
    config.selectedVideoModels = [
      "wan2.1-t2v-1.3b-q8_0",
      "fastwan2.2-ti2v-5b-q6_k",
    ];
    config.activeVideoModel = config.selectedVideoModels[0]!;
    saveConfig(database, config);
    const controller = new RuntimeConfigController(database, root, config);
    const releaseLlm = await resident("llm:busy", 20);
    const releaseStt = await resident("stt:idle", 1);
    let sttKills = 0;
    const llm: RuntimeSupervisor = {
      kind: "server",
      runtimeId: () => "llm:busy",
      state: () => "running",
      async ensureRunning() {},
      async kill() {
        throw new Error("The active LLM must remain running.");
      },
      async shutdown() {},
    };
    const stt: RuntimeSupervisor = {
      kind: "server",
      runtimeId: () => "stt:idle",
      state: () => "running",
      async ensureRunning() {},
      async kill() {
        sttKills += 1;
        releaseStt();
      },
      async shutdown() {},
    };
    const video = factory.create("video", controller.read());
    const reconciler = new RuntimeReconciler(
      controller,
      {},
      new SupervisorRegistry({ llm, stt, video }),
      factory,
      { event() {} },
    );
    const busy = await reconciler.admitModel("llm", config.activeLlmModel);
    if (busy.kind !== "admitted")
      throw new Error("Expected the busy LLM lease.");

    try {
      const afterIdleRelease = await memorySafety.checkAdmission(
        {
          demand: {
            unifiedBytes: 24 * gibibyte,
            hostBytes: 24 * gibibyte,
            acceleratorBytes: 24 * gibibyte,
            confidence: "estimated",
          },
        },
        { releasingRuntimeIds: ["stt:idle"] },
      );
      expect(afterIdleRelease?.decision.reason).toBe("system-memory");
      expect(afterIdleRelease?.capacity).toBeUndefined();
      const result = await admitVideoWithIdleRecovery(
        reconciler,
        config.selectedVideoModels[1]!,
        new AbortController().signal,
      );

      expect(result.kind).toBe("insufficient-memory");
      expect(sttKills).toBe(0);
    } finally {
      busy.value.admission.release();
      releaseLlm();
      releaseStt();
      reconciler.closeQueues();
    }
  } finally {
    database.close();
    await otel.shutdown();
    rmSync(root, { recursive: true, force: true });
  }
});

test("video switch projection credits the old video generation and admits after peer recovery", async () => {
  const root = mkdtempSync("/tmp/localbase-video-switch-recovery-");
  const database = new DatabaseSession();
  const config = defaultConfig(root, 48);
  const videoModels = CATALOG.filter((model) => model.kind === "video");
  const previousVideoModel = videoModels[0]?.modelId;
  const targetVideoModel = videoModels[1]?.modelId;
  if (!previousVideoModel || !targetVideoModel)
    throw new Error("Expected two catalog video models.");
  config.activeVideoModel = previousVideoModel;
  config.selectedVideoModels = [previousVideoModel, targetVideoModel];
  config.selectedLlmModels = [config.activeLlmModel];
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  const released = new Set<string>();
  const projectedReleases: string[][] = [];
  const oldVideo = testSupervisor(
    "video:old",
    () => "running",
    async () => {},
    async () => {
      released.add("video:old");
    },
  );
  const llm = testSupervisor(
    "llm:idle",
    () => "running",
    async () => {
      released.add("llm:idle");
    },
  );
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create(modality, snapshot) {
      const modelId =
        modality === "video"
          ? snapshot.config.activeVideoModel
          : snapshot.config.activeLlmModel;
      const runtimeId = `${modality}:${modelId}`;
      return {
        ...testSupervisor(
          runtimeId,
          () => "idle",
          async () => {},
        ),
        ...(modality === "video"
          ? {
              async preflight(releasingRuntimeIds: readonly string[]) {
                projectedReleases.push([...releasingRuntimeIds]);
                const releasesOldVideo =
                  releasingRuntimeIds.includes("video:old");
                const releasesIdleLlm =
                  releasingRuntimeIds.includes("llm:idle");
                return releasesOldVideo &&
                  (releasesIdleLlm || released.has("llm:idle"))
                  ? undefined
                  : insufficientMemory();
              },
            }
          : {}),
      };
    },
  };
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ video: oldVideo, llm }),
    factory,
    { event() {} },
  );

  try {
    const result = await admitVideoWithIdleRecovery(
      reconciler,
      targetVideoModel,
      new AbortController().signal,
    );
    expect(result.kind).toBe("admitted");
    if (result.kind !== "admitted")
      throw new Error("Expected video admission.");
    await result.value.admission.ready;
    result.value.admission.release();
    expect(released).toEqual(new Set(["llm:idle", "video:old"]));
    expect(projectedReleases).toContainEqual(["llm:idle", "video:old"]);
  } finally {
    reconciler.closeQueues();
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("video recovery claims idle peers before projecting them", async () => {
  const root = mkdtempSync("/tmp/localbase-video-recovery-claim-");
  const database = new DatabaseSession();
  const config = defaultConfig(root, 48);
  const videoModel = CATALOG.find((model) => model.kind === "video")?.modelId;
  if (!videoModel) throw new Error("Expected a catalog video model.");
  config.activeVideoModel = videoModel;
  config.selectedVideoModels = [videoModel];
  config.selectedLlmModels = [config.activeLlmModel];
  config.selectedSttModels = [config.activeSttModel];
  saveConfig(database, config);
  const controller = new RuntimeConfigController(database, root, config);
  const projectionStarted = Promise.withResolvers<void>();
  const releaseProjection = Promise.withResolvers<void>();
  let llmKills = 0;
  let sttKills = 0;
  const llm = testSupervisor(
    "llm:idle",
    () => "running",
    async () => {
      llmKills += 1;
    },
  );
  const stt = testSupervisor(
    "stt:idle",
    () => "running",
    async () => {
      sttKills += 1;
    },
  );
  const factory: RuntimeSupervisorFactory = {
    baseUrl: () => "http://127.0.0.1:1",
    create(modality, snapshot) {
      const modelId =
        modality === "video"
          ? snapshot.config.activeVideoModel
          : snapshot.config.activeLlmModel;
      return {
        ...testSupervisor(
          `${modality}:${modelId}`,
          () => "idle",
          async () => {},
        ),
        ...(modality === "video"
          ? {
              async preflight() {
                projectionStarted.resolve();
                await releaseProjection.promise;
                return undefined;
              },
            }
          : {}),
      };
    },
  };
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm, stt }),
    factory,
    { event() {} },
  );

  try {
    const recovery = reconciler.recoverWithIdleEviction("video", videoModel);
    await projectionStarted.promise;
    const racedAdmission = await reconciler.admitModel(
      "llm",
      config.activeLlmModel,
    );
    expect(racedAdmission.kind).toBe("unavailable");
    releaseProjection.resolve();
    await expect(recovery).resolves.toBe(true);
    expect({ llmKills, sttKills }).toEqual({ llmKills: 1, sttKills: 1 });
  } finally {
    releaseProjection.resolve();
    reconciler.closeQueues();
    database.close();
    rmSync(root, { recursive: true, force: true });
  }
});
