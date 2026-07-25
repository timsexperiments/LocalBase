import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { defaultConfig, loadConfig } from "../../../manager";
import { runConfigure } from "./configure";
import { DatabaseSession } from "../../../db/client";
import type { CommandExecution } from "../../app/commands/framework";
import { configureInputSchema } from "../../app/commands/inputs";

const nonInteractiveExecution: CommandExecution = {
  global: { nonInteractive: true },
  output: { info() {}, error() {} },
};

function makeContext(root: string, gpuVramGb = 16): AppContext {
  return {
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
          global: { root: cliRoot, nonInteractive: true },
        },
      );
    } finally {
      context.database.close();
    }

    const database = new DatabaseSession();
    expect(loadConfig(database, cliRoot).root).toBe(cliRoot);
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
