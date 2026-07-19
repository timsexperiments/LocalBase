import { expect, test, describe, beforeAll, afterAll } from "bun:test";
import { defaultConfig, startLlamaServerProcess } from "../../manager";
import { join } from "node:path";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";

const TEST_ROOT = join(import.meta.dirname, "../../../test-parallel-root");

describe("Configurable Parallel Request Slots & Auto Scaling", () => {
  beforeAll(() => {
    rmSync(TEST_ROOT, { recursive: true, force: true });
    mkdirSync(TEST_ROOT, { recursive: true });

    // Create dummy mock binary
    const binDir = join(TEST_ROOT, "bin");
    mkdirSync(binDir, { recursive: true });
    writeFileSync(
      join(binDir, "llama-server"),
      '#!/bin/sh\necho "args: $@"\nexit 0\n',
      { mode: 0o755 },
    );
  });

  afterAll(() => {
    rmSync(TEST_ROOT, { recursive: true, force: true });
  });

  test("defaultConfig initializes parallel to 'auto'", () => {
    const config = defaultConfig(TEST_ROOT);
    expect(config.parallel).toBe("auto");
  });

  test("startLlamaServerProcess reads static parallel slots count", async () => {
    const config = defaultConfig(TEST_ROOT);
    config.parallel = 4;

    config.root = TEST_ROOT;
    config.llmModelsDir = join(TEST_ROOT, "models", "llm");
    mkdirSync(config.llmModelsDir, { recursive: true });
    writeFileSync(join(config.llmModelsDir, "dummy-model.gguf"), "dummy model");

    // Spy on Bun.spawn
    const originalSpawn = Bun.spawn;
    let passedArgs: string[] = [];
    // @ts-ignore
    Bun.spawn = (args: string[], options: any) => {
      passedArgs = args;
      return originalSpawn(args, options);
    };

    try {
      const process = await startLlamaServerProcess(
        config,
        "dummy-model.gguf",
        "127.0.0.1",
        18005,
        2048,
      );
      process.kill();
    } finally {
      Bun.spawn = originalSpawn;
    }

    expect(passedArgs).toContain("--parallel");
    expect(passedArgs[passedArgs.indexOf("--parallel") + 1]).toBe("4");
  });

  test("startLlamaServerProcess handles auto parallel slots count calculation", async () => {
    const config = defaultConfig(TEST_ROOT);
    config.parallel = "auto";
    config.activeLlmModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";

    config.root = TEST_ROOT;
    config.llmModelsDir = join(TEST_ROOT, "models", "llm");
    mkdirSync(config.llmModelsDir, { recursive: true });
    writeFileSync(
      join(config.llmModelsDir, "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf"),
      "dummy model",
    );

    // Spy on Bun.spawn
    const originalSpawn = Bun.spawn;
    let passedArgs: string[] = [];
    // @ts-ignore
    Bun.spawn = (args: string[], options: any) => {
      passedArgs = args;
      return originalSpawn(args, options);
    };

    try {
      const process = await startLlamaServerProcess(
        config,
        "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf",
        "127.0.0.1",
        18006,
        2048,
      );
      process.kill();
    } finally {
      Bun.spawn = originalSpawn;
    }

    expect(passedArgs).toContain("--parallel");
    const slots = parseInt(
      passedArgs[passedArgs.indexOf("--parallel") + 1],
      10,
    );
    expect(slots).toBeGreaterThanOrEqual(1);
    expect(slots).toBeLessThanOrEqual(4);
  });
});
