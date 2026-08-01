import { expect, test } from "bun:test";
import { mkdtempSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { defaultConfig, loadConfig } from "../../../manager";
import { runConfigure } from "./configure";
import { DatabaseSession } from "../../../db/client";
import type { CommandExecution } from "../../app/commands/framework";
import { configureInputSchema } from "../../app/commands/inputs";
import { ensureLocalBaseRootMarker } from "../../../utils/root";
import { createOtelRuntime } from "../../observability/otel";

const nonInteractiveExecution: CommandExecution = {
  global: { nonInteractive: true, json: false },
  output: { info() {}, error() {}, lifecycle() {} },
};

function makeContext(root: string, gpuVramGb = 16): AppContext {
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
    otel: createOtelRuntime(otelConfiguration),
    otelConfiguration,
    database: new DatabaseSession(),
    config: defaultConfig(root, gpuVramGb),
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
      pipeStream() {},
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
    try {
      await runConfigure(
        {
          all: false,
          defaults: true,
          sttModels: [],
          createKey: false,
        },
        context,
        nonInteractiveExecution,
      );
    } finally {
      context.database.close();
    }

    const database = new DatabaseSession();
    const config = loadConfig(database, root);
    database.close();
    expect(config.selectedSttModels).toEqual([]);
    expect(config.activeSttModel).toBe("");
  });
});
