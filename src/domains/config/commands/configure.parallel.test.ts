import { expect, spyOn, test } from "bun:test";
import { Database } from "bun:sqlite";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { migrate } from "drizzle-orm/bun-sqlite/migrator";
import { mkdtempSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { defaultConfig, loadConfig } from "../../../manager";
import * as manager from "../../../manager";
import { runConfigure } from "./configure";
import { runConfigShow } from "./config";
import { DatabaseSession } from "../../../db/client";
import { migrationsFolder } from "../../../db/migration-assets";
import * as schema from "../../../db/schema";
import type { CommandExecution } from "../../app/commands/framework";
import { configureInputSchema } from "../../app/commands/inputs";
import { ensureLocalBaseRootMarker } from "../../../utils/root";
import { createOtelRuntime, OtelRuntimeHolder } from "../../observability/otel";
import { RuntimeConfigController } from "../../runtime/config-snapshot";
import { CliInputError } from "../../app/commands/errors";

const nonInteractiveExecution: CommandExecution = {
  global: { nonInteractive: true, json: false },
  output: { info() {}, error() {}, lifecycle() {} },
};

function makeContext(root: string, gpuVramGb = 16): AppContext {
  const database = new DatabaseSession();
  const config = defaultConfig(root, gpuVramGb);
  const otelConfiguration = {
    enabled: false,
    headers: {},
    tracesHeaders: {},
    logsHeaders: {},
    sampleRatio: 1,
    sampler: "parentbased_traceidratio" as const,
    source: "persistent" as const,
    displayEndpoint: "",
  };
  return {
    otel: new OtelRuntimeHolder(createOtelRuntime(otelConfiguration)),
    otelConfiguration,
    database,
    config,
    defaultMaxTokens: 4096,
    runtimeConfig: new RuntimeConfigController(database, root, config),
    specs: {
      osName: "Test OS",
      ramGb: 32,
      cpuModel: "Test CPU",
      gpuName: "Test GPU",
      gpuVramGb,
      isMac: false,
      isAppleSilicon: false,
    },
    logger: {
      info() {},
      warn() {},
      error() {},
      event() {},
      request() {},
      async drainStream() {},
      async enableFileLogging() {},
      async close() {},
    },
  };
}

async function withTempRoot(
  action: (root: string) => Promise<void>,
): Promise<void> {
  const root = mkdtempSync(join(tmpdir(), "local-base-configure-"));
  ensureLocalBaseRootMarker(root);

  try {
    await action(root);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test("configure input rejects malformed and out-of-range parallel values", () => {
  for (const parallel of ["many", "0", "5", "1.5"]) {
    expect(
      configureInputSchema.safeParse({
        all: false,
        defaults: true,
        parallel,
      }).success,
    ).toBe(false);
  }
});

test("configure omits model selection lines for empty capabilities", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const lines: string[] = [];
    const execution: CommandExecution = {
      ...nonInteractiveExecution,
      output: {
        info: (message) => lines.push(message),
        error() {},
        lifecycle() {},
      },
    };
    try {
      await runConfigure(
        { all: false, defaults: true, createKey: false },
        context,
        execution,
      );
    } finally {
      context.database.close();
    }

    expect(lines.filter((line) => line.startsWith("Selected "))).not.toContain(
      "Selected STT models: ",
    );
    expect(lines.filter((line) => /^Selected .* models: $/.test(line))).toEqual(
      [],
    );
  });
});

test("configure rejects malformed OTLP settings before persistence", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const valid = configureInputSchema.parse({
      all: false,
      defaults: true,
      otelEndpoint: "https://collector.example",
      otelHeaders: "x-label=left%09right",
      createKey: false,
    });

    try {
      await runConfigure(valid, context, nonInteractiveExecution);
      const before = loadConfig(context.database, root);
      expect(before.otelHeaders).toBe("x-label=left%09right");

      for (const unsafe of [
        { otelHeaders: "authorization" },
        { otelHeaders: "authorization=Bearer%0Ainjected" },
        { otelHeaders: "x-control=%00" },
        { otelHeaders: "x-control=%01" },
        { otelHeaders: "x-control=%1F" },
        { otelHeaders: "x-control=%7F" },
        { otelHeaders: "content-type=text/plain" },
        { otelHeaders: "Content-Encoding=gzip" },
        { otelHeaders: "Content-Length=1" },
        { otelHeaders: "host=attacker.example" },
        { otelHeaders: "transfer-encoding=chunked" },
        { otelEndpoint: "https://user:password@collector.example" },
        { otelEndpoint: "https://collector.example?token=secret" },
        { otelEndpoint: "https://collector.example/#secret" },
      ]) {
        expect(
          configureInputSchema.safeParse({
            all: false,
            defaults: true,
            ...unsafe,
          }).success,
        ).toBe(false);
        expect(loadConfig(context.database, root)).toEqual(before);
      }
    } finally {
      context.database.close();
    }
  });
});

test("configure validates TOML parallel overrides and warns on low VRAM", async () => {
  await withTempRoot(async (root) => {
    const configPath = join(root, "local-base.toml");
    await Bun.write(configPath, "parallel = 2\n");
    const warnings: string[] = [];
    const originalWarn = console.warn;
    console.warn = (...values: unknown[]) => warnings.push(values.join(" "));

    const context = makeContext(root, 12);
    try {
      await runConfigure(
        { all: false, defaults: true, configPath, createKey: false },
        context,
        nonInteractiveExecution,
      );
    } finally {
      context.database.close();
      console.warn = originalWarn;
    }

    const database = new DatabaseSession();
    expect(loadConfig(database, root, 12).parallel).toBe(2);
    database.close();
    expect(warnings).toEqual([
      "Warning: Setting parallel slots to 2 on a system with only 12 GB VRAM may cause Out-Of-Memory (OOM) crashes.",
    ]);
  });
});

test("configure allows unrelated writes with an unsupported persisted video selection", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const existing = defaultConfig(root);
    existing.allowExperimental = true;
    existing.selectedVideoModels = ["wan2.2-s2v-14b-fp8"];
    existing.activeVideoModel = "wan2.2-s2v-14b-fp8";
    const target = spyOn(manager, "detectHostVideoTarget").mockReturnValue({
      platform: "linux",
      architecture: "x64",
      accelerator: "nvidia",
    });
    try {
      manager.saveConfig(context.database, existing);
    } finally {
      target.mockRestore();
    }

    try {
      const shown = await runConfigShow({}, context, nonInteractiveExecution);
      expect(shown.data.document).toContain('"wan2.2-s2v-14b-fp8"');
      await runConfigure(
        { all: false, defaults: true, parallel: 2, createKey: false },
        context,
        nonInteractiveExecution,
      );
    } finally {
      context.database.close();
    }

    const database = new DatabaseSession();
    expect(loadConfig(database, root)).toMatchObject({
      allowExperimental: true,
      selectedVideoModels: ["wan2.2-s2v-14b-fp8"],
      parallel: 2,
    });
    database.close();
  });
});

test("configure merges partial TOML memory reserves with persisted defaults", async () => {
  await withTempRoot(async (root) => {
    const configPath = join(root, "local-base.toml");
    await Bun.write(configPath, "[memory.systemReserve]\npercent = 20\n");
    const context = makeContext(root);
    try {
      await runConfigure(
        { all: false, defaults: true, configPath, createKey: false },
        context,
        nonInteractiveExecution,
      );
      expect(loadConfig(context.database, root).memory).toEqual({
        systemReserve: { percent: 20, minimumGb: 8 },
        acceleratorReserve: { percent: 10, minimumGb: 2 },
      });
    } finally {
      context.database.close();
    }
  });
});

test("configure gives the CLI root precedence over a configured root", async () => {
  await withTempRoot(async (baseRoot) => {
    const cliRoot = join(baseRoot, "cli-root");
    const configuredRoot = join(baseRoot, "configured-root");
    const configPath = join(baseRoot, "local-base.toml");
    await Bun.write(configPath, `root = "${configuredRoot}"\n`);

    const context = makeContext(cliRoot);
    try {
      await runConfigure(
        { all: false, defaults: true, configPath, createKey: false },
        context,
        {
          ...nonInteractiveExecution,
          global: { root: cliRoot, nonInteractive: true, json: false },
        },
      );
    } finally {
      context.database.close();
    }

    const database = new DatabaseSession();
    expect(loadConfig(database, cliRoot).root).toBe(realpathSync(cliRoot));
    database.close();
  });
});

test("configure clears the active STT model when selection is intentionally empty", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const output: string[] = [];
    try {
      await runConfigure(
        {
          all: false,
          defaults: true,
          sttModels: [],
          createKey: false,
        },
        context,
        {
          ...nonInteractiveExecution,
          output: {
            ...nonInteractiveExecution.output,
            info: (line) => output.push(line),
          },
        },
      );
    } finally {
      context.database.close();
    }

    const database = new DatabaseSession();
    const config = loadConfig(database, root);
    database.close();
    expect(config.selectedSttModels).toEqual([]);
    expect(config.activeSttModel).toBe("");
    expect(output.some((line) => line.startsWith("Selected STT models:"))).toBe(
      false,
    );
  });
});

test("configure persists and disables the canonical TTS selection", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const modelId = "qwen3-tts-1.7b-base-q4_k_m";
    try {
      await runConfigure(
        {
          all: false,
          defaults: true,
          ttsModels: [modelId],
          activeTts: modelId,
          createKey: false,
        },
        context,
        nonInteractiveExecution,
      );
      expect(loadConfig(context.database, root)).toMatchObject({
        selectedTtsModels: [modelId],
        activeTtsModel: modelId,
      });

      await runConfigure(
        {
          all: false,
          defaults: true,
          ttsModels: [],
          createKey: false,
        },
        context,
        nonInteractiveExecution,
      );
      expect(loadConfig(context.database, root)).toMatchObject({
        selectedTtsModels: [],
        activeTtsModel: "",
      });
    } finally {
      context.database.close();
    }
  });
});

test("configure validates video selections against the detected target", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const target = spyOn(manager, "detectHostVideoTarget").mockReturnValue({
      platform: "linux",
      architecture: "x64",
      accelerator: "nvidia",
    });
    const modelId = "wan2.1-t2v-1.3b-q8_0";
    try {
      await runConfigure(
        {
          all: false,
          defaults: true,
          videoModels: [modelId],
          activeVideo: modelId,
          createKey: false,
        },
        context,
        nonInteractiveExecution,
      );
      expect(loadConfig(context.database, root)).toMatchObject({
        selectedVideoModels: [modelId],
        activeVideoModel: modelId,
      });

      target.mockReturnValue(null);
      await runConfigure(
        {
          all: false,
          defaults: true,
          videoModels: [],
          createKey: false,
        },
        context,
        nonInteractiveExecution,
      );
      await expect(
        runConfigure(
          {
            all: false,
            defaults: true,
            videoModels: [modelId],
            createKey: false,
          },
          context,
          nonInteractiveExecution,
        ),
      ).rejects.toThrow("single NVIDIA GPU");
    } finally {
      target.mockRestore();
      context.database.close();
    }
  });
});

test("configure rejects invalid composed model selections before persistence", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);
    const sqlite = new Database(":memory:");
    const db = drizzle({ client: sqlite, schema });
    migrate(db, { migrationsFolder: migrationsFolder() });
    const getDatabase = spyOn(context.database, "get").mockReturnValue(db);
    try {
      const video = "wan2.1-t2v-1.3b-q8_0";
      const catalogOnlyVideo = "wan2.2-ti2v-5b-q6_k";
      await runConfigure(
        {
          all: false,
          defaults: true,
          createKey: false,
          videoModels: [video],
          activeVideo: video,
        },
        context,
        nonInteractiveExecution,
      );
      const before = loadConfig(context.database, root);
      expect(before.selectedVideoModels).toEqual([video]);
      expect(before.activeVideoModel).toBe(video);
      const llm = "qwen2.5-coder-7b-instruct-q4_k_m";
      const cases = [
        { llmModels: [llm, llm] },
        {
          llmModels: [llm],
          activeLlm: "mistral-nemo-12b-instruct-q4_k_m",
        },
        { llmModels: [llm], activeLlm: "whisper-base-q8_0" },
        { videoModels: [catalogOnlyVideo] },
        { activeVideo: catalogOnlyVideo },
      ];

      for (const values of cases) {
        const result = runConfigure(
          { all: false, defaults: true, createKey: false, ...values },
          context,
          nonInteractiveExecution,
        );
        await expect(result).rejects.toBeInstanceOf(CliInputError);
        if ("videoModels" in values || "activeVideo" in values) {
          await expect(result).rejects.toThrow("catalog-only models");
        }
        expect(loadConfig(context.database, root)).toEqual(before);
      }
    } finally {
      getDatabase.mockRestore();
      sqlite.close();
      context.database.close();
    }
  });
});
