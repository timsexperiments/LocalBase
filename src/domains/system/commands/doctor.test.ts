import { expect, test } from "bun:test";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { runDoctor } from "./doctor";
import { DatabaseSession } from "../../../db/client";
import type { CommandExecution } from "../../app/commands/framework";

const execution: CommandExecution = {
  global: { nonInteractive: false, json: false },
  output: { info() {}, error() {}, lifecycle() {} },
};

function makeContext(): AppContext {
  const config = defaultConfig("/tmp/local-base-doctor", 16);
  config.parallel = 2;

  return {
    database: new DatabaseSession(),
    config,
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
      request() {},
      pipeStream() {},
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
  expect("hfToken" in report.configuration).toBe(false);
});
