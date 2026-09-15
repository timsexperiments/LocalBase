import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join, relative } from "node:path";
import { byId } from "../../catalog";
import { defaultConfig, type LocalBaseConfig } from "../../manager";
import { compileRuntimeFixture } from "../../test/runtime-fixture";
import {
  resolveImageLaunchPlan,
  resolveLlmLaunchPlan,
  resolveSttLaunchPlan,
  resolveVideoLaunchPlan,
} from "./launch-plan";
import { requireWhisperGpuContract } from "./whisper-gpu";
import {
  sdServerEnvironment,
  buildSdVideoServerArgs,
  startLlamaServerProcess,
  startSdServerProcess,
  startSdVideoServerProcess,
  startWhisperServerProcess,
} from "./launcher";

const roots: string[] = [];
const originalPath = process.env.PATH;
const originalLibraryPath = process.env.LD_LIBRARY_PATH;

test("uses video profile launch options", () => {
  const plan = resolveVideoLaunchPlan({
    runtimeId: "video:test:1",
    root: "/tmp/local-base-video-launch",
    modelsDirectory: "/tmp/local-base-video-launch/models/video",
    modelId: "video-model",
    diffusionModelFile: "diffusion.gguf",
    textEncoderFile: "encoder.gguf",
    vaeFile: "vae.safetensors",
    host: "127.0.0.1",
    port: 8091,
    videoRuntime: {
      mode: "t2v",
      artifacts: {
        diffusionModel: "diffusion.gguf",
        textEncoder: "encoder.gguf",
        vae: "vae.safetensors",
      },
      qualification: {
        maxWidth: 320,
        maxHeight: 320,
        maxFrames: 33,
        generation: { sampler: "euler", steps: 20, cfgScale: 6, seed: 42 },
        launchOptions: { cpuOffload: true, diffusionFlashAttention: true },
      },
      estimatedMemoryDemand: {
        unifiedBytes: 1,
        hostBytes: 1,
        acceleratorBytes: 1,
      },
      supportedPlatforms: ["linux"],
    },
    platform: "linux",
  });

  expect(buildSdVideoServerArgs(plan)).toEqual([
    "--diffusion-model",
    plan.diffusionModelPath,
    "--t5xxl",
    plan.textEncoderPath,
    "--vae",
    plan.vaePath,
    "-M",
    "vid_gen",
    "--offload-to-cpu",
    "--diffusion-fa",
    "--listen-ip",
    "127.0.0.1",
    "--listen-port",
    "8091",
  ]);
});

test("adds the wav2vec2 path only to an S2V launch", () => {
  const plan = resolveVideoLaunchPlan({
    runtimeId: "video:s2v:1",
    root: "/tmp/local-base-video-launch",
    modelsDirectory: "/tmp/local-base-video-launch/models/video",
    modelId: "s2v-model",
    diffusionModelFile: "diffusion.safetensors",
    textEncoderFile: "encoder.safetensors",
    vaeFile: "vae.safetensors",
    host: "127.0.0.1",
    port: 8091,
    videoRuntime: {
      mode: "s2v",
      artifacts: {
        diffusionModel: "diffusion.safetensors",
        textEncoder: "encoder.safetensors",
        vae: "vae.safetensors",
        audioEncoder: "wav2vec2.safetensors",
      },
      qualification: {
        maxWidth: 832,
        maxHeight: 480,
        maxFrames: 81,
        generation: { sampler: "euler", steps: 20, cfgScale: 6, seed: 42 },
        launchOptions: { cpuOffload: true, diffusionFlashAttention: true },
      },
      estimatedMemoryDemand: {
        unifiedBytes: 1,
        hostBytes: 1,
        acceleratorBytes: 1,
      },
      supportedPlatforms: ["linux"],
    },
    platform: "linux",
  });

  expect(plan.mode).toBe("s2v");
  if (plan.mode !== "s2v") throw new Error("Expected an S2V launch plan.");
  expect(buildSdVideoServerArgs(plan)).toContain("--audio-encoder");
  expect(buildSdVideoServerArgs(plan)).toContain(plan.audioEncoderPath);
});

test("rejects an S2V launch with a missing audio encoder before spawning", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-s2v-launch-"));
  roots.push(root);
  const modelsDirectory = join(root, "models", "video");
  mkdirSync(modelsDirectory, { recursive: true });
  for (const filename of [
    "diffusion.safetensors",
    "encoder.safetensors",
    "vae.safetensors",
  ]) {
    await Bun.write(join(modelsDirectory, filename), "fixture");
  }
  const plan = resolveVideoLaunchPlan({
    runtimeId: "video:s2v:1",
    root,
    modelsDirectory,
    modelId: "s2v-model",
    diffusionModelFile: "diffusion.safetensors",
    textEncoderFile: "encoder.safetensors",
    vaeFile: "vae.safetensors",
    host: "127.0.0.1",
    port: 8091,
    videoRuntime: {
      mode: "s2v",
      artifacts: {
        diffusionModel: "diffusion.safetensors",
        textEncoder: "encoder.safetensors",
        vae: "vae.safetensors",
        audioEncoder: "missing-wav2vec2.safetensors",
      },
      qualification: {
        maxWidth: 832,
        maxHeight: 480,
        maxFrames: 81,
        generation: { sampler: "euler", steps: 20, cfgScale: 6, seed: 42 },
        launchOptions: { cpuOffload: true, diffusionFlashAttention: true },
      },
      estimatedMemoryDemand: {
        unifiedBytes: 1,
        hostBytes: 1,
        acceleratorBytes: 1,
      },
      supportedPlatforms: ["linux"],
    },
    platform: "linux",
  });

  await expect(startSdVideoServerProcess(plan)).rejects.toThrow(
    "Configured video artifact does not exist.",
  );
});

describe.serial("Whisper GPU launch contract", () => {
  test("launches with the admitted PCI identity on Linux and unchanged arguments on macOS", async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-whisper-launch-"));
    roots.push(root);
    const config = defaultConfig(root, 12);
    const userBin = join(root, "user-bin");
    const binPath = join(userBin, "whisper-server");
    const argsPath = join(root, "args.json");
    mkdirSync(userBin, { recursive: true });
    mkdirSync(config.sttModelsDir, { recursive: true });
    const modelPath = join(config.sttModelsDir, "model.bin");
    await Bun.write(modelPath, "model placeholder");
    const build = async (capability: string) => {
      const result = await Bun.build({
        entrypoints: [
          join(import.meta.dir, "../../test/whisper-launch-fixture.ts"),
        ],
        target: "bun",
        compile: { outfile: binPath },
        define: {
          __WHISPER_CAPABILITY__: JSON.stringify(capability),
          __WHISPER_ARGS_PATH__: JSON.stringify(argsPath),
        },
      });
      expect(result.success).toBeTrue();
    };
    await build("error: unknown argument: --localbase-capabilities");
    await expect(requireWhisperGpuContract(binPath)).rejects.toThrow(
      "Unsupported Linux Whisper runtime",
    );
    expect(await Bun.file(argsPath).exists()).toBeFalse();
    await build("oversized");
    await expect(requireWhisperGpuContract(binPath)).rejects.toThrow(
      "Unsupported Linux Whisper runtime",
    );
    await build("hang");
    await expect(requireWhisperGpuContract(binPath)).rejects.toThrow(
      "Unsupported Linux Whisper runtime",
    );
    const hungPid = Number(await Bun.file(argsPath).text());
    expect(Number.isSafeInteger(hungPid) && hungPid > 0).toBeTrue();
    expect(() => process.kill(hungPid, 0)).toThrow();
    await build("localbase-whisper-gpu-pci-v1");
    await requireWhisperGpuContract(binPath);
    process.env.PATH = `${userBin}:${originalPath ?? ""}`;
    const child = await startWhisperServerProcess(
      resolveSttLaunchPlan({
        runtimeId: "stt:test:1",
        root,
        modelsDirectory: config.sttModelsDir,
        modelId: "whisper-tiny",
        modelRequirementGb: undefined,
        modelFile: "model.bin",
        host: "127.0.0.1",
        port: 18001,
        artifactBytes: 1,
      }),
      {
        kind: "discrete",
        system: { id: "system", capacityBytes: 1 },
        accelerators: [
          {
            id: "nvidia:GPU-12345678-1234-1234-1234-123456789abc",
            capacityBytes: 1,
            pciBusId: "0000:ab:1f.7",
          },
        ],
      },
    );
    expect(await child.exited).toBe(0);
    expect(await Bun.file(argsPath).json()).toEqual([
      "--model",
      modelPath,
      "--host",
      "127.0.0.1",
      "--port",
      "18001",
      ...(process.platform === "linux"
        ? ["--require-gpu-pci", "0000:ab:1f.7"]
        : []),
    ]);
  }, 15_000);
});

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
  process.env.LD_LIBRARY_PATH = originalLibraryPath;
  for (const root of roots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

test("puts sd-server libraries before inherited Linux runtime libraries", () => {
  const binaryPath = "/opt/localbase/sd/sd-server";
  process.env.LD_LIBRARY_PATH = [
    "/opt/localbase/llama",
    "/opt/localbase/whisper",
  ].join(delimiter);

  expect(sdServerEnvironment(binaryPath, "linux").LD_LIBRARY_PATH).toBe(
    ["/opt/localbase/sd", process.env.LD_LIBRARY_PATH].join(delimiter),
  );
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
      expect(process.exitCode).toBeNull();
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

describe.serial("image runtime launch", () => {
  test("starts a user-managed runtime without a managed bin directory", async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-image-launch-"));
    roots.push(root);
    const config = defaultConfig(root, 12);
    const modelFile = "model.safetensors";
    const modelPath = join(config.imageModelsDir, modelFile);
    const userBinDir = join(root, "user-bin");
    const binPath = join(userBinDir, "sd-server");
    const argsPath = join(userBinDir, "sd-server.args");
    const environmentPath = join(userBinDir, "sd-server.environment");
    mkdirSync(config.imageModelsDir, { recursive: true });
    mkdirSync(userBinDir, { recursive: true });
    await Bun.write(modelPath, "model placeholder");
    process.env.LD_LIBRARY_PATH = [
      join(root, "llama-libraries"),
      join(root, "whisper-libraries"),
    ].join(delimiter);
    await compileRuntimeFixture(
      binPath,
      argsPath,
      undefined,
      false,
      undefined,
      undefined,
      false,
      undefined,
      environmentPath,
    );
    process.env.PATH = `${relative(process.cwd(), userBinDir)}:${originalPath ?? ""}`;

    const nativeProcess = await startSdServerProcess(
      resolveImageLaunchPlan({
        runtimeId: "image:test:1",
        root,
        modelsDirectory: config.imageModelsDir,
        modelId: "dreamshaper-v8",
        modelFile,
        host: "127.0.0.1",
        port: 18002,
        modelRequirementGb: 4,
        artifactBytes: 2 * 1024 ** 3,
      }),
    );
    try {
      await readCapturedArgs(argsPath);
      expect(nativeProcess.exitCode).toBeNull();
      expect(await readCapturedArgs(argsPath)).toEqual([
        "-m",
        modelPath,
        "--listen-ip",
        "127.0.0.1",
        "--listen-port",
        "18002",
      ]);
      expect(await Bun.file(environmentPath).text()).toBe(
        process.platform === "linux"
          ? [userBinDir, process.env.LD_LIBRARY_PATH].join(delimiter)
          : process.env.LD_LIBRARY_PATH,
      );
    } finally {
      nativeProcess.kill();
      expect(await nativeProcess.exited).toBe(0);
    }
  });
});
