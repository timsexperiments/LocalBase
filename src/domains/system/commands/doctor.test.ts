import { expect, test } from "bun:test";
import { appendFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { runDoctor } from "./doctor";
import { DatabaseSession } from "../../../db/client";
import type { CommandExecution } from "../../app/commands/framework";
import { createOtelRuntime, OtelRuntimeHolder } from "../../observability/otel";
import { RuntimeConfigController } from "../../runtime/config-snapshot";
import {
  prepareServiceStderrLog,
  serviceStderrPath,
} from "../../observability/service-stderr";

const execution: CommandExecution = {
  global: { nonInteractive: false, json: false },
  output: { info() {}, error() {}, lifecycle() {} },
};

function makeContext(): AppContext {
  const config = defaultConfig("/tmp/local-base-doctor", 16);
  config.parallel = 2;
  const database = new DatabaseSession();

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
    runtimeConfig: new RuntimeConfigController(database, config.root, config),
    specs: {
      osName: "Test OS",
      ramGb: 32,
      cpuModel: "Test CPU",
      gpuName: "Test GPU",
      gpuVramGb: 16,
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

test("doctor prints configured parallel slots and returns a safe report", async () => {
  const output: string[] = [];
  await runDoctor({}, makeContext(), {
    ...execution,
    output: {
      info: (message) => output.push(message),
      error() {},
      lifecycle() {},
    },
  });

  expect(output).toContain("Parallel Slots: 2");
});

test("doctor result separates hardware from non-sensitive configuration", async () => {
  const context = makeContext();
  context.config.hfToken = "secret";
  const report = (await runDoctor({}, context, execution)).data;

  expect(report.hardware.gpuVramGb).toBe(16);
  expect(report.hardware).toEqual(context.specs);
  expect(report.configuration.parallel).toBe(2);
  expect(report.configuration.memory).toEqual(context.config.memory);
  expect("hfToken" in report.configuration).toBe(false);
});

test("doctor reports effective environment observability without secrets", async () => {
  const context = makeContext();
  context.config.otelEndpoint = "https://persisted.example";
  context.config.otelHeaders = "authorization=Bearer%20persisted-secret";
  context.otelConfiguration = {
    ...context.otelConfiguration,
    enabled: true,
    source: "environment",
    sampler: "always_off",
    sampleRatio: 1,
    displayEndpoint: "https://override.example/",
    tracesEndpoint: "https://override.example/v1/traces",
    logsEndpoint: "https://override.example/v1/logs",
  };
  const output: string[] = [];
  const report = (
    await runDoctor({}, context, {
      ...execution,
      output: {
        info: (message) => output.push(message),
        error() {},
        lifecycle() {},
      },
    })
  ).data;
  const serialized = JSON.stringify(report);

  expect(output).toContain(
    "OpenTelemetry: enabled via environment (https://override.example/)",
  );
  expect(report.configuration.observability).toEqual({
    enabled: true,
    source: "environment",
    endpoint: "https://override.example/",
    persistedEndpoint: "https://persisted.example/",
    sampler: "always_off",
    sampleRatio: 1,
  });
  expect(serialized).not.toContain("persisted-secret");
  expect(serialized).not.toContain("authorization");
});

test("doctor surfaces captured service stderr by size and path only", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-doctor-stderr-"));
  try {
    const context = makeContext();
    context.config.root = root;
    const quiet = await runDoctor({}, context, execution);
    expect(quiet.data).not.toHaveProperty("serviceStderr");

    await prepareServiceStderrLog(root);
    appendFileSync(serviceStderrPath(root), "panic: prompt=doctor-secret\n");
    const output: string[] = [];
    const report = (
      await runDoctor({}, context, {
        ...execution,
        output: {
          info: (message) => output.push(message),
          error() {},
          lifecycle() {},
        },
      })
    ).data;

    expect(report.serviceStderr).toEqual({
      path: serviceStderrPath(root),
      bytes: 28,
      modifiedAt: expect.any(String),
      archiveBytes: 0,
    });
    const line = output.find((message) =>
      message.startsWith("Service stderr:"),
    );
    expect(line).toContain(`28 bytes captured at ${serviceStderrPath(root)}`);
    expect(output.join("\n")).not.toContain("doctor-secret");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
