import { expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createLogger } from "../observability/logging";
import { createOtelRuntime, OtelRuntimeHolder } from "../observability/otel";
import { defaultConfig } from "../../manager";
import { MemorySafetyController } from "./memory-controller";
import { defaultMemorySafetyConfig, gibibyte } from "./memory-safety";
import { createRuntimeSupervisorFactory } from "./supervisor-factory";

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
    await expect(
      factory.create("video", { revision: 0, config }).ensureRunning(),
    ).rejects.toThrow(/Video runtime target is not supported|does not support/);
    expect(existsSync(config.videoModelsDir)).toBe(false);
    expect(snapshots).toBe(0);
  } finally {
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
