import { afterEach, describe, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { arch, platform, tmpdir } from "node:os";
import { join } from "node:path";
import {
  defaultConfig,
  launchLlamaServer,
  loadConfig,
  managedRuntimeUnavailableError,
  platformSupportTier,
  saveConfig,
  startLlamaServerProcess,
  type LocalBaseConfig,
} from "./manager";

const testRoots: string[] = [];

function createLegacyConfigRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "local-base-parallel-"));
  testRoots.push(root);
  const db = new Database(join(root, "local-base.db"));
  db.exec(`
    create table config (
      id text primary key,
      root text not null,
      llm_models_dir text not null,
      stt_models_dir text not null,
      image_models_dir text,
      runtime_backend text not null,
      stt_backend text not null,
      host text not null,
      port integer not null,
      ctx_size integer not null,
      stt_host text not null,
      stt_port integer not null,
      startup_on_boot integer not null,
      selected_llm_models text not null,
      selected_stt_models text not null,
      selected_image_models text,
      active_llm_model text not null,
      active_stt_model text not null,
      active_image_model text,
      system_prompt text,
      hf_token text
    );
  `);
  db.prepare(
    `insert into config values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    "default",
    root,
    join(root, "models", "llm"),
    join(root, "models", "stt"),
    join(root, "models", "image"),
    "llama.cpp",
    "whisper.cpp",
    "127.0.0.1",
    18000,
    8192,
    "127.0.0.1",
    18080,
    0,
    '["qwen2.5-coder-7b-instruct-q4_k_m"]',
    '["whisper-base-q8_0"]',
    "[]",
    "qwen2.5-coder-7b-instruct-q4_k_m",
    "whisper-base-q8_0",
    "",
    "",
    "",
  );
  db.close();
  return root;
}

async function createLlamaLaunchFixture(
  parallel: LocalBaseConfig["parallel"],
): Promise<{
  argsPath: string;
  config: LocalBaseConfig;
  modelFile: string;
  modelPath: string;
}> {
  const root = mkdtempSync(join(tmpdir(), "local-base-llama-launch-"));
  testRoots.push(root);

  const config = defaultConfig(root, 9.5);
  config.activeLlmModel = "qwen2.5-coder-7b-instruct-q4_k_m";
  config.parallel = parallel;

  const modelFile = "model.gguf";
  const modelPath = join(config.llmModelsDir, modelFile);
  const binDir = join(root, "bin");
  const binPath = join(binDir, "llama-server");
  const argsPath = join(binDir, "llama-server.args");
  mkdirSync(config.llmModelsDir, { recursive: true });
  mkdirSync(binDir, { recursive: true });
  await Bun.write(modelPath, "model placeholder");
  await Bun.write(
    binPath,
    `#!/bin/sh
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
printf '%s\\n' "$@" > "$script_dir/llama-server.args"
`,
  );
  chmodSync(binPath, 0o755);

  return { argsPath, config, modelFile, modelPath };
}

function expectedLlamaArgs(modelPath: string, parallel: string): string[] {
  const args = [
    "-m",
    modelPath,
    "--host",
    "127.0.0.1",
    "--port",
    "18000",
    "-c",
    "8192",
    "--parallel",
    parallel,
    "--jinja",
    "--embeddings",
  ];
  if (platform() === "darwin" && arch() === "arm64") {
    args.push("--flash-attn", "auto");
  }
  return args;
}

function readCapturedArgs(argsPath: string): string[] {
  return readFileSync(argsPath, "utf8").trim().split("\n");
}

afterEach(() => {
  for (const root of testRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

describe("parallel configuration persistence", () => {
  test("migrates legacy SQLite config and round-trips auto and manual values", () => {
    const root = createLegacyConfigRoot();
    const config = loadConfig(root);

    expect(config.parallel).toBe("auto");

    config.parallel = 4;
    saveConfig(config);
    expect(loadConfig(root).parallel).toBe(4);

    config.parallel = "auto";
    saveConfig(config);
    expect(loadConfig(root).parallel).toBe("auto");
  });
});

describe("platform support tiers", () => {
  test("classifies managed, CLI-only, and unsupported targets", () => {
    expect(platformSupportTier({ os: "darwin", cpu: "arm64" })).toBe("managed");
    expect(platformSupportTier({ os: "linux", cpu: "x64" })).toBe("managed");
    expect(platformSupportTier({ os: "darwin", cpu: "x64" })).toBe("cli-only");
    expect(platformSupportTier({ os: "linux", cpu: "arm64" })).toBe("cli-only");
    expect(platformSupportTier({ os: "win32", cpu: "x64" })).toBe(
      "unsupported",
    );
  });

  test("explains how to provide missing CLI-only runtimes", () => {
    expect(
      managedRuntimeUnavailableError(
        "whisper-server",
        { os: "darwin", cpu: "x64" },
        "/tmp/local-base/bin",
      ).message,
    ).toBe(
      "LocalBase CLI-only compatibility on macOS x64 does not include a managed whisper-server runtime. Place a compatible whisper-server executable in /tmp/local-base/bin/whisper-server or on PATH.",
    );
    expect(
      managedRuntimeUnavailableError(
        "sd-server",
        { os: "linux", cpu: "arm64" },
        "/tmp/local-base/bin",
      ).message,
    ).toContain("/tmp/local-base/bin/sd-server or on PATH");
    expect(
      managedRuntimeUnavailableError(
        "sd-server",
        { os: "win32", cpu: "x64" },
        "/tmp/local-base/bin",
      ).message,
    ).toContain("/tmp/local-base/bin/sd-server or on PATH");
  });
});

describe("llama server argument construction", () => {
  test("passes exact argv to async startup and logs auto allocation", async () => {
    const fixture = await createLlamaLaunchFixture("auto");
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...values: unknown[]) => output.push(values.join(" "));

    try {
      const process = await startLlamaServerProcess(
        fixture.config,
        fixture.modelFile,
        "127.0.0.1",
        18000,
        8192,
        { memoryGb: 9.5 },
      );
      expect(await process.exited).toBe(0);
    } finally {
      console.log = originalLog;
    }

    expect(readCapturedArgs(fixture.argsPath)).toEqual(
      expectedLlamaArgs(fixture.modelPath, "2"),
    );
    expect(
      output.filter((line) => line.includes("Dynamic Concurrency")),
    ).toEqual([
      "🤖 Dynamic Concurrency: Calculated 2 parallel slots based on 9.5 GB VRAM and context memory constraints. 4096 tokens per slot.",
    ]);
  });

  test("passes exact argv to synchronous startup", async () => {
    const fixture = await createLlamaLaunchFixture(3);

    expect(
      await launchLlamaServer(
        fixture.config,
        fixture.modelFile,
        "127.0.0.1",
        18000,
        8192,
      ),
    ).toBe(0);
    expect(readCapturedArgs(fixture.argsPath)).toEqual(
      expectedLlamaArgs(fixture.modelPath, "3"),
    );
  });
});
