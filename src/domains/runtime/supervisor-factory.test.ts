import { expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createLogger } from "../observability/logging";
import { createOtelRuntime, OtelRuntimeHolder } from "../observability/otel";
import { defaultConfig, saveConfig } from "../../manager";
import { DatabaseSession } from "../../db/client";
import { RuntimeConfigController } from "./config-snapshot";
import { MemorySafetyController } from "./memory-controller";
import { defaultMemorySafetyConfig, gibibyte } from "./memory-safety";
import { createRuntimeSupervisorFactory } from "./supervisor-factory";
import { RuntimeReconciler } from "./runtime-reconciler";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";
import { SpeechSupervisor } from "./speech-supervisor";
import { generateSpeechWithIdleRecovery } from "./commands/serve";
import { projectVideoJob } from "./video/gateway-contract";

test("rejects an unsupported video topology before installation or launch", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-target-"));
  let snapshots = 0;
  const memorySafety = new MemorySafetyController(
    {
      topology: {
        kind: "discrete",
        system: { id: "system", capacityBytes: 32 * gibibyte },
        accelerators: [],
      },
      async snapshot() {
        snapshots += 1;
        return {
          capturedAtMs: Date.now(),
          pools: [
            {
              poolId: "system",
              availability: "available",
              availableBytes: 32 * gibibyte,
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
  const config = defaultConfig(root);
  config.selectedVideoModels = ["wan2.1-t2v-1.3b-q8_0"];
  config.activeVideoModel = "wan2.1-t2v-1.3b-q8_0";
  const factory = createRuntimeSupervisorFactory(
    {
      logger: createLogger(),
      otel,
      specs: {
        osName: "test",
        ramGb: 32,
        cpuModel: "test",
        gpuName: "test",
        gpuVramGb: 32,
        isMac: false,
        isAppleSilicon: false,
      },
    },
    {},
    { memorySafety },
  );

  try {
    let failure: unknown;
    try {
      await factory.create("video", { revision: 0, config }).ensureRunning();
    } catch (error) {
      failure = error;
    }
    if (!(failure instanceof Error))
      throw new Error("Expected video startup to fail with an Error.");
    const projected = projectVideoJob({
      id: "00000000-0000-4000-8000-000000000000",
      createdAtMs: 1,
      terminalAtMs: 2,
      state: "failed",
      failure,
    });
    expect(projected).toMatchObject({
      error: {
        code: "model_hardware_ineligible",
        message: expect.stringContaining("single NVIDIA GPU"),
      },
    });
    expect(existsSync(config.videoModelsDir)).toBe(false);
    expect(snapshots).toBe(0);
  } finally {
    await otel.shutdown();
    rmSync(root, { recursive: true, force: true });
  }
});

test("recovers a real speech generation rejection by evicting an idle peer", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-tts-idle-recovery-"));
  const db = new DatabaseSession();
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
              availability: "available" as const,
              availableBytes: freeBytes,
              pressure: "normal" as const,
            },
          ],
        };
      },
      async close() {},
    },
    defaultMemorySafetyConfig(),
  );
  const demand = {
    unifiedBytes: 8 * gibibyte,
    hostBytes: 8 * gibibyte,
    acceleratorBytes: 8 * gibibyte,
    confidence: "estimated" as const,
  };
  const resident = await memorySafety.reserve({
    runtimeId: "llm:idle",
    demand: { ...demand, unifiedBytes: 20 * gibibyte },
  });
  resident.materialize();
  freeBytes = 10 * gibibyte;
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
  config.selectedTtsModels = ["qwen3-tts-1.7b-base-q4_k_m"];
  config.activeTtsModel = config.selectedTtsModels[0]!;
  saveConfig(db, config);
  const controller = new RuntimeConfigController(db, root, config);
  const logger = createLogger();
  const factory = createRuntimeSupervisorFactory(
    {
      logger,
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
    { memorySafety, host: { platform: "darwin", arch: "arm64" } },
  );
  let kills = 0;
  const llm: RuntimeSupervisor = {
    kind: "server",
    runtimeId: () => "llm:idle",
    state: () => (kills ? "idle" : "running"),
    async ensureRunning() {},
    async kill() {
      kills++;
      resident.release();
      freeBytes += 20 * gibibyte;
    },
    async shutdown() {},
  };
  const speech = new SpeechSupervisor({
    runtimeId: "tts:test",
    root,
    modelId: config.activeTtsModel,
    logger,
    memorySafety,
    async prepare() {
      return {
        binaryPath: "/unused",
        modelPath: "/unused",
        projectorPath: "/unused",
        speakerFiles: { harbor: "/unused", willow: "/unused" },
      };
    },
  });
  const reconciler = new RuntimeReconciler(
    controller,
    {},
    new SupervisorRegistry({ llm, tts: speech }),
    factory,
    { event() {} },
  );
  let attempts = 0;
  try {
    expect(
      await memorySafety.checkAdmission(
        { demand },
        { releasingRuntimeIds: ["llm:idle"] },
      ),
    ).toBeUndefined();
    const admitted = await reconciler.admitModel("tts", config.activeTtsModel);
    if (admitted.kind !== "admitted") throw new Error("Expected admission");
    try {
      await admitted.value.admission.ready;
      await generateSpeechWithIdleRecovery(
        reconciler,
        config.activeTtsModel,
        new AbortController().signal,
        async () => {
          attempts++;
          if (attempts === 1) return speech.generateSpeech({ text: "hello" });
          return new Uint8Array([1]);
        },
      ).catch(() => {});
      expect({ attempts, kills }).toEqual({ attempts: 2, kills: 1 });
    } finally {
      admitted.value.admission.release();
    }
  } finally {
    reconciler.closeQueues();
    resident.release();
    db.close();
    await otel.shutdown();
    rmSync(root, { recursive: true, force: true });
  }
});

test("preflights catalog capacity demand when LLM artifacts are missing", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-missing-llm-preflight-"));
  const memorySafety = new MemorySafetyController(
    {
      topology: {
        kind: "unified",
        system: { id: "system", capacityBytes: 32 * gibibyte },
      },
      async snapshot() {
        return {
          capturedAtMs: Date.now(),
          pools: [
            {
              poolId: "system",
              availability: "available",
              availableBytes: 32 * gibibyte,
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
  const config = defaultConfig(root, 32);
  config.selectedLlmModels = ["qwen3-coder-next-q4_k_m"];
  config.activeLlmModel = "qwen3-coder-next-q4_k_m";
  const factory = createRuntimeSupervisorFactory(
    {
      logger: createLogger(),
      otel,
      specs: {
        osName: "test",
        ramGb: 32,
        cpuModel: "test",
        gpuName: "test",
        gpuVramGb: 32,
        isMac: false,
        isAppleSilicon: false,
      },
    },
    {},
    { memorySafety },
  );

  try {
    const supervisor = factory.create("llm", { revision: 0, config });
    if (!supervisor.preflight) throw new Error("Expected preflight support.");
    const rejection = await supervisor.preflight(["llm:current"]);
    expect(rejection).toMatchObject({
      decision: {
        kind: "rejected",
        reason: "system-memory",
      },
      capacity: {
        capacityBytes: 32 * gibibyte,
      },
    });
    // All four shards are resident, so demand must cover their sum, not only
    // the first shard.
    expect(rejection?.capacity?.requiredBytes).toBeGreaterThan(45 * gibibyte);
    await expect(
      Bun.file(
        join(
          config.llmModelsDir,
          "Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
        ),
      ).exists(),
    ).resolves.toBe(false);
  } finally {
    await otel.shutdown();
    rmSync(root, { recursive: true, force: true });
  }
});

test.each([
  ["stt", "whisper-large-v3-turbo"],
  ["image", "flux1-schnell-q4_0"],
  ["image", "sdxl-base-1.0"],
  ["video", "wan2.1-t2v-1.3b-q8_0"],
] as const)(
  "preflights catalog capacity demand for missing %s artifacts",
  async (modality, modelId) => {
    const root = mkdtempSync(
      join(tmpdir(), `localbase-missing-${modality}-preflight-`),
    );
    const memorySafety = new MemorySafetyController(
      {
        topology: {
          kind: "unified",
          system: { id: "system", capacityBytes: 4 * gibibyte },
        },
        async snapshot() {
          return {
            capturedAtMs: Date.now(),
            pools: [
              {
                poolId: "system",
                availability: "available",
                availableBytes: 4 * gibibyte,
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
    const config = defaultConfig(root, 4);
    if (modality === "stt") {
      config.selectedSttModels = [modelId];
      config.activeSttModel = modelId;
    } else if (modality === "image") {
      config.selectedImageModels = [modelId];
      config.activeImageModel = modelId;
    } else {
      config.selectedVideoModels = [modelId];
      config.activeVideoModel = modelId;
    }
    const factory = createRuntimeSupervisorFactory(
      {
        logger: createLogger(),
        otel,
        specs: {
          osName: "test",
          ramGb: 4,
          cpuModel: "test",
          gpuName: "test",
          gpuVramGb: 4,
          isMac: false,
          isAppleSilicon: false,
        },
      },
      {},
      { memorySafety, host: { platform: "darwin", arch: "arm64" } },
    );

    try {
      const supervisor = factory.create(modality, { revision: 0, config });
      if (!supervisor.preflight) throw new Error("Expected preflight support.");
      const rejection = await supervisor.preflight([`${modality}:current`]);
      expect(rejection).toMatchObject({
        decision: { kind: "rejected", reason: "system-memory" },
        capacity: { capacityBytes: 4 * gibibyte },
      });
    } finally {
      await otel.shutdown();
      rmSync(root, { recursive: true, force: true });
    }
  },
);
