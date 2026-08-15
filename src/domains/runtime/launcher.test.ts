import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { byId } from "../../catalog";
import { defaultConfig, type LocalBaseConfig } from "../../manager";
import { compileRuntimeFixture } from "../../test/runtime-fixture";
import { resolveLlmLaunchPlan } from "./launch-plan";
import { startLlamaServerProcess } from "./launcher";

const roots: string[] = [];
const originalPath = process.env.PATH;

async function createLlamaLaunchFixture(
  parallel: LocalBaseConfig["parallel"],
): Promise<{
  argsPath: string;
  config: LocalBaseConfig;
  modelFile: string;
  modelPath: string;
}> {
  const root = mkdtempSync(join(tmpdir(), "local-base-llama-launch-"));
  roots.push(root);
  const config = defaultConfig(root, 9.5);
  config.activeLlmModel = "qwen2.5-coder-7b-instruct-q4_k_m";
  config.parallel = parallel;

  const modelFile = "model.gguf";
  const modelPath = join(config.llmModelsDir, modelFile);
  const userBinDir = join(root, "user-bin");
  const binPath = join(userBinDir, "llama-server");
  const argsPath = join(userBinDir, "llama-server.args");
  mkdirSync(config.llmModelsDir, { recursive: true });
  mkdirSync(userBinDir, { recursive: true });
  await Bun.write(modelPath, "model placeholder");
  await compileRuntimeFixture(binPath, argsPath);
  process.env.PATH = `${userBinDir}:${originalPath ?? ""}`;

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
  if (process.platform === "darwin" && process.arch === "arm64") {
    args.push("--flash-attn", "auto");
  }
  return args;
}

async function readCapturedArgs(argsPath: string): Promise<string[]> {
  const deadline = Date.now() + 2_000;
  while (!(await Bun.file(argsPath).exists())) {
    if (Date.now() >= deadline) {
      throw new Error(`Runtime did not write arguments to ${argsPath}.`);
    }
    await Bun.sleep(10);
  }
  return (await Bun.file(argsPath).text()).trim().split("\n");
}

afterEach(() => {
  process.env.PATH = originalPath;
  for (const root of roots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

describe.serial("llama runtime launch", () => {
  test("passes exact argv to async startup and logs auto allocation", async () => {
    const fixture = await createLlamaLaunchFixture("auto");
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...values: unknown[]) => output.push(values.join(" "));

    try {
      const process = await startLlamaServerProcess(
        resolveLlmLaunchPlan({
          runtimeId: "llm:test:1",
          root: fixture.config.root,
          modelsDirectory: fixture.config.llmModelsDir,
          modelId: fixture.config.activeLlmModel,
          modelFile: fixture.modelFile,
          host: "127.0.0.1",
          port: 18000,
          ctxSize: 8192,
          parallel: fixture.config.parallel,
          modelRequirementGb: byId(fixture.config.activeLlmModel)?.minVramGb,
          artifactBytes: 4 * 1024 ** 3,
          hardware: { memoryGb: 9.5 },
        }),
      );
      await readCapturedArgs(fixture.argsPath);
      process.kill();
      expect(await process.exited).toBe(0);
    } finally {
      console.log = originalLog;
    }

    expect(await readCapturedArgs(fixture.argsPath)).toEqual(
      expectedLlamaArgs(fixture.modelPath, "2"),
    );
    expect(
      output.filter((line) => line.includes("Dynamic Concurrency")),
    ).toEqual([
      "🤖 Dynamic Concurrency: Calculated 2 parallel slots based on 9.5 GB VRAM and context memory constraints. 4096 tokens per slot.",
    ]);
  });
});
