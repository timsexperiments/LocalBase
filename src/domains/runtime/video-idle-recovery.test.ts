import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { DatabaseSession } from "../../db/client";
import { defaultConfig, saveConfig } from "../../manager";
import { createLogger } from "../observability/logging";
import { createOtelRuntime, OtelRuntimeHolder } from "../observability/otel";
import { admitVideoWithIdleRecovery } from "./commands/serve";
import { defaultMemorySafetyConfig, gibibyte } from "./memory-safety";
import { MemorySafetyController } from "./memory-controller";
import { RuntimeConfigController } from "./config-snapshot";
import { RuntimeReconciler } from "./runtime-reconciler";
import { createRuntimeSupervisorFactory } from "./supervisor-factory";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";

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
