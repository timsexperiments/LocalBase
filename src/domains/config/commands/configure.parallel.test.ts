import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { defaultConfig, loadConfig } from "../../../manager";
import { runConfigure } from "./configure";

function makeContext(root: string, gpuVramGb = 16): AppContext {
  return {
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
      request() {},
      pipeStream() {},
    },
  };
}

async function withTempRoot(
  action: (root: string) => Promise<void>,
): Promise<void> {
  const root = mkdtempSync(join(tmpdir(), "local-base-configure-"));

  try {
    await action(root);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test("configure rejects malformed and out-of-range parallel values", async () => {
  await withTempRoot(async (root) => {
    const context = makeContext(root);

    for (const parallel of ["many", "0", "5", "1.5"]) {
      await expect(
        runConfigure(
          ["--defaults", "--parallel", parallel, "--create-key", "false"],
          context,
        ),
      ).rejects.toThrow(/parallel/i);
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

    try {
      await runConfigure(
        ["--defaults", "--config", configPath, "--create-key", "false"],
        makeContext(root, 12),
      );
    } finally {
      console.warn = originalWarn;
    }

    expect(loadConfig(root, 12).parallel).toBe(2);
    expect(warnings).toEqual([
      "Warning: Setting parallel slots to 2 on a system with only 12 GB VRAM may cause Out-Of-Memory (OOM) crashes.",
    ]);
  });
});
