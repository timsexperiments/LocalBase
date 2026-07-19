import { expect, test } from "bun:test";
import { defaultConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { runDoctor } from "./doctor";

function makeContext(): AppContext {
  const config = defaultConfig("/tmp/local-base-doctor", 16);
  config.parallel = 2;

  return {
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

function captureOutput(action: () => void): string[] {
  const output: string[] = [];
  const originalLog = console.log;
  console.log = (message?: unknown) => output.push(String(message));

  try {
    action();
  } finally {
    console.log = originalLog;
  }

  return output;
}

test("doctor prints configured parallel slots", () => {
  const output = captureOutput(() => runDoctor([], makeContext()));

  expect(output).toContain("Parallel Slots: 2");
});

test("doctor JSON separates hardware from non-sensitive configuration", () => {
  const context = makeContext();
  context.config.hfToken = "secret";
  const output = captureOutput(() => runDoctor(["--json"], context));
  const report = JSON.parse(output.join("\n"));

  expect(report.gpuVramGb).toBe(16);
  expect(report.hardware).toEqual(context.specs);
  expect(report.configuration.parallel).toBe(2);
  expect(report.configuration.hfToken).toBeUndefined();
});
