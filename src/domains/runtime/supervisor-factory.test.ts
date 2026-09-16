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

test("rejects an unsupported video target before installation or launch", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-video-target-"));
  let snapshots = 0;
  const memorySafety = new MemorySafetyController(
    {
      topology: {
        kind: "unified",
        system: { id: "system", capacityBytes: 32 * gibibyte },
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
