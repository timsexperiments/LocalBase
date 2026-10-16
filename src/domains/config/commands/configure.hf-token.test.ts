import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig, loadConfig, saveConfig } from "../../../manager";
import { ensureLocalBaseRootMarker } from "../../../utils/root";
import type { CommandExecution } from "../../app/commands/framework";
import { createOtelRuntime, OtelRuntimeHolder } from "../../observability/otel";
import { RuntimeConfigController } from "../../runtime/config-snapshot";
import { resolveConfiguredHubToken, runConfigure } from "./configure";

const tomlToken = "hf_tomlTokenAAAAAAAAAAAAAAAA";
const uiToken = "hf_uiTokenBBBBBBBBBBBBBBBBBB";
const flagToken = "hf_flagTokenCCCCCCCCCCCCCCCC";

describe("resolveConfiguredHubToken", () => {
  test("--hf-token always replaces the saved token", () => {
    expect(
      resolveConfiguredHubToken({
        flag: flagToken,
        toml: tomlToken,
        saved: uiToken,
      }),
    ).toEqual({ token: flagToken, keptSaved: false });
    expect(resolveConfiguredHubToken({ flag: "", saved: uiToken })).toEqual({
      token: "",
      keptSaved: false,
    });
  });

  test("a TOML token fills in when no token is saved", () => {
    expect(resolveConfiguredHubToken({ toml: tomlToken, saved: "" })).toEqual({
      token: tomlToken,
      keptSaved: false,
    });
    expect(
      resolveConfiguredHubToken({ toml: tomlToken, saved: tomlToken }),
    ).toEqual({ token: tomlToken, keptSaved: false });
  });

  test("a TOML token never overwrites a different saved token", () => {
    expect(
      resolveConfiguredHubToken({ toml: tomlToken, saved: uiToken }),
    ).toEqual({ token: uiToken, keptSaved: true });
    expect(resolveConfiguredHubToken({ toml: "", saved: uiToken })).toEqual({
      token: uiToken,
      keptSaved: true,
    });
  });

  test("without a flag or TOML token the saved token stays", () => {
    expect(resolveConfiguredHubToken({ saved: uiToken })).toEqual({
      token: uiToken,
      keptSaved: false,
    });
    expect(resolveConfiguredHubToken({ saved: "" })).toEqual({
      token: "",
      keptSaved: false,
    });
  });
});

function makeContext(root: string): AppContext {
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
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

test("configure keeps a token saved in the UI when the TOML token differs", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-configure-token-"));
  ensureLocalBaseRootMarker(root);
  const configPath = join(root, "local-base.toml");
  await Bun.write(configPath, `hfToken = "${tomlToken}"\n`);
  const messages: string[] = [];
  const execution: CommandExecution = {
    global: { nonInteractive: true, json: false },
    output: {
      info: (message) => messages.push(message),
      error: (message) => messages.push(message),
      lifecycle() {},
    },
  };
  const savedToken = () => {
    const database = new DatabaseSession();
    try {
      return loadConfig(database, root, 16).hfToken;
    } finally {
      database.close();
    }
  };
  const configure = async (hfToken?: string) => {
    const context = makeContext(root);
    try {
      await runConfigure(
        {
          all: false,
          defaults: true,
          configPath,
          createKey: false,
          ...(hfToken === undefined ? {} : { hfToken }),
        },
        context,
        execution,
      );
    } finally {
      context.database.close();
    }
  };
  try {
    await configure();
    expect(savedToken()).toBe(tomlToken);

    // The admin UI replaces the token.
    const database = new DatabaseSession();
    saveConfig(database, {
      ...loadConfig(database, root, 16),
      hfToken: uiToken,
    });
    database.close();

    messages.length = 0;
    await configure();
    expect(savedToken()).toBe(uiToken);
    const warning = messages.find((message) => message.startsWith("Warning:"));
    expect(warning).toContain("hfToken in the TOML file was not applied");
    for (const message of messages) {
      expect(message).not.toContain(tomlToken);
      expect(message).not.toContain(uiToken);
    }

    messages.length = 0;
    await configure(flagToken);
    expect(savedToken()).toBe(flagToken);
    expect(messages.some((message) => message.startsWith("Warning:"))).toBe(
      false,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
