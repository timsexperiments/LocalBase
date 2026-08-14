import { expect, test } from "bun:test";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { runDoctor } from "./doctor";
import { DatabaseSession } from "../../../db/client";
import type { CommandExecution } from "../../app/commands/framework";
import { createOtelRuntime, OtelRuntimeHolder } from "../../observability/otel";
import { RuntimeConfigController } from "../../runtime/config-snapshot";

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
      pipeStream() {},
      async enableFileLogging() {},
      async close() {},
    },
  };
}

test("doctor prints configured parallel slots and returns a safe report", () => {
  const output: string[] = [];
  runDoctor({}, makeContext(), {
    ...execution,
    output: {
      info: (message) => output.push(message),
      error() {},
      lifecycle() {},
    },
  });

  expect(output).toContain("Parallel Slots: 2");
});

test("doctor result separates hardware from non-sensitive configuration", () => {
  const context = makeContext();
  context.config.hfToken = "secret";
  const report = runDoctor({}, context, execution).data;

  expect(report.hardware.gpuVramGb).toBe(16);
  expect(report.hardware).toEqual(context.specs);
  expect(report.configuration.parallel).toBe(2);
  expect(report.configuration.memory).toEqual(context.config.memory);
  expect("hfToken" in report.configuration).toBe(false);
});

test("doctor reports effective environment observability without secrets", () => {
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
  const report = runDoctor({}, context, {
    ...execution,
    output: {
      info: (message) => output.push(message),
      error() {},
      lifecycle() {},
    },
  }).data;
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
