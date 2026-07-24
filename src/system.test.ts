import { expect, test } from "bun:test";
import { platform as osPlatform } from "node:os";
import { deriveAppleGpuName, detectSpecs } from "./system";

test("derives stable Apple GPU labels from supported chip names", () => {
  expect(deriveAppleGpuName("Apple M4 Max")).toBe("Apple M4 Max GPU");
  expect(deriveAppleGpuName("Apple M3 Pro")).toBe("Apple M3 Pro GPU");
  expect(deriveAppleGpuName("unknown CPU")).toBe("Apple Silicon GPU");
});

test("detects a finite host specification set", async () => {
  const specs = await detectSpecs();

  expect(specs.osName.length).toBeGreaterThan(0);
  expect(specs.cpuModel.length).toBeGreaterThan(0);
  expect(specs.gpuName.length).toBeGreaterThan(0);
  expect(Number.isFinite(specs.ramGb)).toBe(true);
  expect(Number.isFinite(specs.gpuVramGb)).toBe(true);
  expect(specs.ramGb).toBeGreaterThanOrEqual(0);
  expect(specs.gpuVramGb).toBeGreaterThanOrEqual(0);
  expect(specs.isMac).toBe(osPlatform() === "darwin");

  if (specs.isAppleSilicon) {
    expect(specs.osName).toBe("macOS");
    expect(specs.gpuName).toContain("GPU");
    expect(specs.gpuVramGb).toBe(specs.ramGb);
  }
});
