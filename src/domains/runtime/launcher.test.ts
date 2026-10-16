import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join, relative } from "node:path";
import { byId, type ModelSpec } from "../../catalog";
import { defaultConfig, type LocalBaseConfig } from "../../manager";
import { compileRuntimeFixture } from "../../test/runtime-fixture";
import {
  resolveImageLaunchPlan,
  resolveLlmLaunchPlan,
  resolveSttLaunchPlan,
  resolveVideoLaunchPlan,
} from "./launch-plan";
import { requireSdGpuContract } from "./sd-gpu";
import { requireWhisperGpuContract } from "./whisper-gpu";
import { gibibyte, type MemoryTopology } from "./memory-safety";
import {
  sdServerEnvironment,
  buildSdImageServerArgs,
  buildSdVideoServerArgs,
  startLlamaServerProcess,
  startSdServerProcess,
  startSdVideoServerProcess,
  startWhisperServerProcess,
} from "./launcher";

function catalogModel(modelId: string): ModelSpec {
  const model = byId(modelId);
  if (!model) throw new Error(`Missing ${modelId} fixture model.`);
  return model;
}

// Catalog video models with their artifacts renamed to the fixture filenames,
// so the derived demand is computed from the real pinned artifact sizes.
function withFixtureFilenames(
  model: ModelSpec,
  filenames: Readonly<Record<string, string>>,
): ModelSpec {
  return {
    ...model,
    artifacts: model.artifacts.map((artifact) => ({
      ...artifact,
      filename: filenames[artifact.filename] ?? artifact.filename,
    })),
  };
}

const wanT2v = withFixtureFilenames(catalogModel("wan2.1-t2v-1.3b-q8_0"), {
  "Wan2.1-T2V-1.3B-Q8_0.gguf": "diffusion.gguf",
  "umt5-xxl-encoder-Q8_0.gguf": "encoder.gguf",
  "wan_2.1_vae.safetensors": "decoder.safetensors",
});
const wanS2v = withFixtureFilenames(catalogModel("wan2.2-s2v-14b-fp8"), {
  "wan2.2_s2v_14B_fp8_scaled.safetensors": "diffusion.safetensors",
  "umt5-xxl-encoder-Q8_0.gguf": "encoder.safetensors",
  "wan_2.1_vae.safetensors": "vae.safetensors",
  "wav2vec2_large_english_fp16.safetensors": "wav2vec2.safetensors",
});

test.each([
  {
    modelId: "flux2-klein-4b-q4_0",
    steps: 4,
    // Measured (memory-measurements.ts): sd.cpp params + VAE-decode compute
    // buffer + accelerator overhead; host is the measured peak RSS.
    memoryDemand: {
      unifiedBytes: 12638224384,
      hostBytes: 5983240192,
      acceleratorBytes: 12638224384,
      confidence: "measured",
    },
  },
  {
    modelId: "z-image-turbo-q4_0",
    steps: 8,
    // Every component resident plus the derived activation, VAE decode and
    // accelerator-overhead terms at the family default resolution.
    memoryDemand: {
      unifiedBytes: 13859679109,
      hostBytes: 7087439872,
      acceleratorBytes: 13859679109,
      confidence: "measured",
    },
  },
])(
  "resolves $modelId artifacts, sampling, and aggregate memory",
  ({ modelId, steps, memoryDemand }) => {
    const spec = byId(modelId);
    if (spec?.imageRuntime?.kind !== "diffusion-qwen3") {
      throw new Error("Missing Qwen image fixture");
    }
    const directory = "/models/image";
    const plan = resolveImageLaunchPlan({
      runtimeId: `image:${modelId}`,
      root: "/localbase",
      modelsDirectory: directory,
      modelId,
      modelFile: spec.imageRuntime.artifacts.diffusionModel,
      host: "127.0.0.1",
      port: 8083,
      model: spec,
      artifactBytes: spec.artifacts.reduce(
        (sum, artifact) => sum + (artifact.expectedSizeBytes ?? 0),
        0,
      ),
      imageRuntime: spec.imageRuntime,
    });
    expect(buildSdImageServerArgs(plan)).toEqual([
      "--diffusion-model",
      join(directory, spec.imageRuntime.artifacts.diffusionModel),
      "--vae",
      join(directory, spec.imageRuntime.artifacts.vae),
      "--llm",
      join(directory, spec.imageRuntime.artifacts.textEncoder),
      "--sampling-method",
      "euler",
      "--steps",
      String(steps),
      "--cfg-scale",
      "1",
      "--diffusion-fa",
      "--listen-ip",
      "127.0.0.1",
      "--listen-port",
      "8083",
    ]);
    expect(plan.memoryDemand).toEqual(memoryDemand);
  },
);

test("rejects a missing image encoder before resolving or spawning a runtime", async () => {
  const spec = byId("z-image-turbo-q4_0");
  if (spec?.imageRuntime?.kind !== "diffusion-qwen3") {
    throw new Error("Missing Qwen image fixture");
  }
  const root = mkdtempSync(join(tmpdir(), "localbase-image-artifacts-"));
  try {
    const plan = resolveImageLaunchPlan({
      runtimeId: "image:test",
      root,
      modelsDirectory: root,
      modelId: spec.modelId,
      modelFile: spec.imageRuntime.artifacts.diffusionModel,
      host: "127.0.0.1",
      port: 8083,
      artifactBytes: 1,
      imageRuntime: spec.imageRuntime,
    });
    await Bun.write(plan.modelPath, "fixture");
    await Bun.write(join(root, spec.imageRuntime.artifacts.vae), "fixture");
    await expect(
      startSdServerProcess(plan, {
        kind: "unified",
        system: { id: "system", capacityBytes: 64 * gibibyte },
      }),
    ).rejects.toThrow("Image artifact not found:");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test.each([
  {
    modelId: "flux1-schnell-q4_0",
    // --clip-on-cpu keeps CLIP-L and T5-XXL off the accelerator.
    expectedMemoryDemand: {
      unifiedBytes: 25991561216,
      hostBytes: 18007343104,
      acceleratorBytes: 14569565061,
      confidence: "measured",
    },
    expectedModelArgs: [
      "--diffusion-model",
      "/models/image/flux1-schnell-q4_0.gguf",
      "--vae",
      "/models/image/flux1-ae.safetensors",
      "--clip_l",
      "/models/image/flux1-clip_l.safetensors",
      "--t5xxl",
      "/models/image/flux1-t5xxl_fp16.safetensors",
      "--clip-on-cpu",
      "--sampling-method",
      "euler",
      "--steps",
      "4",
      "--cfg-scale",
      "1",
    ],
  },
  {
    modelId: "dreamshaper-8-lcm",
    expectedMemoryDemand: {
      unifiedBytes: 4415616451,
      hostBytes: 3006021632,
      acceleratorBytes: 4415616451,
      confidence: "measured",
    },
    expectedModelArgs: [
      "-m",
      "/models/image/DreamShaper8_LCM.safetensors",
      "--sampling-method",
      "lcm",
      "--scheduler",
      "lcm",
      "--steps",
      "4",
      "--cfg-scale",
      "1",
    ],
  },
])(
  "builds the pinned $modelId launch profile",
  ({ modelId, expectedModelArgs, expectedMemoryDemand }) => {
    const spec = byId(modelId);
    if (!spec?.imageRuntime)
      throw new Error(`Missing image profile: ${modelId}`);
    const plan = resolveImageLaunchPlan({
      runtimeId: `image:${modelId}`,
      root: "/localbase",
      modelsDirectory: "/models/image",
      modelId,
      modelFile: spec.imageRuntime.artifacts.diffusionModel,
      host: "127.0.0.1",
      port: 8083,
      model: spec,
      artifactBytes: spec.artifacts.reduce(
        (sum, artifact) => sum + (artifact.expectedSizeBytes ?? 0),
        0,
      ),
      imageRuntime: spec.imageRuntime,
    });

    expect(buildSdImageServerArgs(plan)).toEqual([
      ...expectedModelArgs,
      ...(spec.imageLaunchOptions?.diffusionFlashAttention
        ? ["--diffusion-fa"]
        : []),
      "--listen-ip",
      "127.0.0.1",
      "--listen-port",
      "8083",
    ]);
    expect(plan.memoryDemand).toEqual(expectedMemoryDemand);
  },
);

test.each([true, false])(
  "passes --diffusion-fa only when the catalog enables it (%p)",
  (diffusionFlashAttention) => {
    const spec = byId("dreamshaper-v8");
    if (!spec) throw new Error("Missing SD1.5 fixture");
    const plan = resolveImageLaunchPlan({
      runtimeId: "image:dreamshaper-v8",
      root: "/localbase",
      modelsDirectory: "/models/image",
      modelId: spec.modelId,
      modelFile: "DreamShaper_8_pruned.safetensors",
      host: "127.0.0.1",
      port: 8083,
      model: { ...spec, imageLaunchOptions: { diffusionFlashAttention } },
      artifactBytes: 1,
    });
    expect(plan.launchOptions).toEqual({ diffusionFlashAttention });
    expect(buildSdImageServerArgs(plan).includes("--diffusion-fa")).toBe(
      diffusionFlashAttention,
    );
  },
);

test("passes --prediction v for v-prediction checkpoints", () => {
  const spec = byId("dreamshaper-v8");
  if (!spec) throw new Error("Missing SD1.5 fixture");
  const plan = resolveImageLaunchPlan({
    runtimeId: "image:sd-two-one",
    root: "/localbase",
    modelsDirectory: "/models/image",
    modelId: "sd-two-one",
    modelFile: "v2-1_768-ema-pruned.safetensors",
    host: "127.0.0.1",
    port: 8083,
    model: {
      ...spec,
      imageLaunchOptions: { diffusionFlashAttention: false, prediction: "v" },
    },
    artifactBytes: 1,
  });
  expect(plan.launchOptions).toEqual({
    diffusionFlashAttention: false,
    prediction: "v",
  });
  const args = buildSdImageServerArgs(plan);
  expect(
    args.slice(args.indexOf("--prediction"), args.indexOf("--prediction") + 2),
  ).toEqual(["--prediction", "v"]);
  expect(
    buildSdImageServerArgs({
      ...plan,
      launchOptions: { diffusionFlashAttention: false },
    }),
  ).not.toContain("--prediction");
});

test("image plans without a catalog spec launch without --diffusion-fa", () => {
  const plan = resolveImageLaunchPlan({
    runtimeId: "image:custom",
    root: "/localbase",
    modelsDirectory: "/models/image",
    modelId: "custom",
    modelFile: "custom.safetensors",
    host: "127.0.0.1",
    port: 8083,
    artifactBytes: 1,
  });
  expect(buildSdImageServerArgs(plan)).not.toContain("--diffusion-fa");
});

const roots: string[] = [];
const originalPath = process.env.PATH;
const originalLibraryPath = process.env.LD_LIBRARY_PATH;
const unifiedTopology = {
  kind: "unified",
  system: { id: "system", capacityBytes: 64 * gibibyte },
} satisfies MemoryTopology;
const discreteTopology = {
  kind: "discrete",
  system: { id: "system", capacityBytes: 96 * gibibyte },
  accelerators: [
    { id: "gpu", capacityBytes: 12 * gibibyte, pciBusId: "0000:01:00.0" },
  ],
} satisfies MemoryTopology;
const nvidiaTopology: MemoryTopology = {
  kind: "discrete",
  system: { id: "system", capacityBytes: 1 },
  accelerators: [
    {
      id: "nvidia:GPU-12345678-1234-1234-1234-123456789abc",
      capacityBytes: 1,
      pciBusId: "0000:ab:1f.7",
    },
  ],
};

async function compileSdLaunchFixture(input: {
  argsPath: string;
  binPath: string;
  capability: string;
  environmentPath?: string;
}): Promise<void> {
  const result = await Bun.build({
    entrypoints: [join(import.meta.dir, "../../test/sd-launch-fixture.ts")],
    target: "bun",
    compile: { outfile: input.binPath },
    define: {
      __SD_CAPABILITY__: JSON.stringify(input.capability),
      __SD_ARGS_PATH__: JSON.stringify(input.argsPath),
      __SD_ENVIRONMENT_PATH__: JSON.stringify(input.environmentPath ?? ""),
    },
  });
  expect(result.success).toBeTrue();
}

test.each(["vae", "tae"] as const)(
  "uses %s video profile launch options",
  (kind) => {
    const plan = resolveVideoLaunchPlan({
      runtimeId: "video:test:1",
      root: "/tmp/local-base-video-launch",
      modelsDirectory: "/tmp/local-base-video-launch/models/video",
      modelId: "video-model",
      diffusionModelFile: "diffusion.gguf",
      textEncoderFile: "encoder.gguf",
      host: "127.0.0.1",
      port: 8091,
      model: wanT2v,
      videoRuntime: {
        mode: "t2v",
        artifacts: {
          diffusionModel: "diffusion.gguf",
          textEncoder: "encoder.gguf",
          decoder: { kind, artifactFilename: "decoder.safetensors" },
        },
        qualification: {
          jobDeadlineMs: 10 * 60 * 1_000,
          maxWidth: 320,
          maxHeight: 320,
          maxFrames: 33,
          fps: 16,
          generation: {
            sampler: "euler",
            scheduler: "discrete",
            steps: 20,
            cfgScale: 6,
            flowShift: 3,
            seed: 42,
          },
          launchOptions: {
            cpuOffload: true,
            diffusionFlashAttention: true,
            vaeConvDirect: kind === "tae",
          },
        },
        supportedTargets: [
          { platform: "linux", architecture: "x64", accelerator: "nvidia" },
        ],
      },
      target: { platform: "linux", architecture: "x64", accelerator: "nvidia" },
    });

    const unifiedArgs = buildSdVideoServerArgs(plan, unifiedTopology);
    expect(unifiedArgs).toEqual([
      "--diffusion-model",
      plan.diffusionModelPath,
      "--t5xxl",
      plan.textEncoderPath,
      kind === "vae" ? "--vae" : "--tae",
      plan.decoder.path,
      "--offload-to-cpu",
      "--diffusion-fa",
      ...(kind === "tae" ? ["--vae-conv-direct"] : []),
      "--listen-ip",
      "127.0.0.1",
      "--listen-port",
      "8091",
    ]);
    expect(buildSdVideoServerArgs(plan, discreteTopology)).toEqual([
      ...unifiedArgs,
      "--max-vram",
      // The text-encoder stage dominates with CPU offload for both decoders.
      "6.5",
    ]);
  },
);

test("adds the wav2vec2 path only to an S2V launch", () => {
  const plan = resolveVideoLaunchPlan({
    runtimeId: "video:s2v:1",
    root: "/tmp/local-base-video-launch",
    modelsDirectory: "/tmp/local-base-video-launch/models/video",
    modelId: "s2v-model",
    diffusionModelFile: "diffusion.safetensors",
    textEncoderFile: "encoder.safetensors",
    host: "127.0.0.1",
    port: 8091,
    model: wanS2v,
    videoRuntime: {
      mode: "s2v",
      artifacts: {
        diffusionModel: "diffusion.safetensors",
        textEncoder: "encoder.safetensors",
        decoder: { kind: "vae", artifactFilename: "vae.safetensors" },
        audioEncoder: "wav2vec2.safetensors",
      },
      qualification: {
        jobDeadlineMs: 10 * 60 * 1_000,
        maxWidth: 832,
        maxHeight: 480,
        maxFrames: 81,
        fps: 16,
        generation: {
          sampler: "euler",
          scheduler: "discrete",
          steps: 20,
          cfgScale: 6,
          flowShift: 3,
          seed: 42,
        },
        launchOptions: {
          cpuOffload: true,
          diffusionFlashAttention: true,
          vaeConvDirect: false,
        },
      },
      supportedTargets: [
        { platform: "linux", architecture: "x64", accelerator: "nvidia" },
      ],
    },
    target: { platform: "linux", architecture: "x64", accelerator: "nvidia" },
  });

  expect(plan.mode).toBe("s2v");
  if (plan.mode !== "s2v") throw new Error("Expected an S2V launch plan.");
  expect(buildSdVideoServerArgs(plan, discreteTopology)).toContain(
    "--audio-encoder",
  );
  expect(buildSdVideoServerArgs(plan, discreteTopology)).toContain(
    plan.audioEncoderPath,
  );
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
    host: "127.0.0.1",
    port: 8091,
    model: wanS2v,
    videoRuntime: {
      mode: "s2v",
      artifacts: {
        diffusionModel: "diffusion.safetensors",
        textEncoder: "encoder.safetensors",
        decoder: { kind: "vae", artifactFilename: "vae.safetensors" },
        audioEncoder: "missing-wav2vec2.safetensors",
      },
      qualification: {
        jobDeadlineMs: 10 * 60 * 1_000,
        maxWidth: 832,
        maxHeight: 480,
        maxFrames: 81,
        fps: 16,
        generation: {
          sampler: "euler",
          scheduler: "discrete",
          steps: 20,
          cfgScale: 6,
          flowShift: 3,
          seed: 42,
        },
        launchOptions: {
          cpuOffload: true,
          diffusionFlashAttention: true,
          vaeConvDirect: false,
        },
      },
      supportedTargets: [
        { platform: "linux", architecture: "x64", accelerator: "nvidia" },
      ],
    },
    target: { platform: "linux", architecture: "x64", accelerator: "nvidia" },
  });

  await expect(
    startSdVideoServerProcess(plan, discreteTopology),
  ).rejects.toThrow("Configured video artifact does not exist.");
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

describe.serial("sd-server GPU launch contract", () => {
  test("rejects an sd-server without the PCI capability before spawn", async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-sd-contract-"));
    roots.push(root);
    const binPath = join(root, "sd-server");
    const argsPath = join(root, "args.json");
    const build = (capability: string) =>
      compileSdLaunchFixture({
        binPath,
        argsPath,
        capability,
      });

    await build("unknown argument");
    await expect(requireSdGpuContract(binPath)).rejects.toThrow(
      "Unsupported Linux sd-server runtime",
    );
    expect(await Bun.file(argsPath).exists()).toBeFalse();
    await build("oversized");
    await expect(requireSdGpuContract(binPath)).rejects.toThrow(
      "Unsupported Linux sd-server runtime",
    );
    await build("localbase-sd-gpu-pci-v1");
    await expect(requireSdGpuContract(binPath)).resolves.toBeUndefined();
  }, 15_000);

  test("passes the admitted PCI identity to a video sd-server on Linux", async () => {
    const root = mkdtempSync(join(tmpdir(), "local-base-video-gpu-launch-"));
    roots.push(root);
    const config = defaultConfig(root, 12);
    const userBinDir = join(root, "user-bin");
    const binPath = join(userBinDir, "sd-server");
    const argsPath = join(userBinDir, "sd-server.args");
    mkdirSync(config.videoModelsDir, { recursive: true });
    mkdirSync(userBinDir, { recursive: true });
    for (const filename of [
      "diffusion.gguf",
      "encoder.gguf",
      "vae.safetensors",
    ]) {
      await Bun.write(
        join(config.videoModelsDir, filename),
        "model placeholder",
      );
    }
    await compileSdLaunchFixture({
      binPath,
      argsPath,
      capability: "localbase-sd-gpu-pci-v1",
    });
    process.env.PATH = `${relative(process.cwd(), userBinDir)}:${originalPath ?? ""}`;

    const nativeProcess = await startSdVideoServerProcess(
      resolveVideoLaunchPlan({
        runtimeId: "video:test:1",
        root,
        modelsDirectory: config.videoModelsDir,
        modelId: "video-model",
        diffusionModelFile: "diffusion.gguf",
        textEncoderFile: "encoder.gguf",
        host: "127.0.0.1",
        port: 18003,
        model: wanT2v,
        videoRuntime: {
          mode: "t2v",
          artifacts: {
            diffusionModel: "diffusion.gguf",
            textEncoder: "encoder.gguf",
            decoder: { kind: "vae", artifactFilename: "vae.safetensors" },
          },
          qualification: {
            jobDeadlineMs: 10 * 60 * 1_000,
            maxWidth: 320,
            maxHeight: 320,
            maxFrames: 33,
            fps: 16,
            generation: {
              sampler: "euler",
              scheduler: "discrete",
              steps: 20,
              cfgScale: 6,
              flowShift: 3,
              seed: 42,
            },
            launchOptions: {
              cpuOffload: true,
              diffusionFlashAttention: true,
              vaeConvDirect: false,
            },
          },
          supportedTargets: [
            { platform: "linux", architecture: "x64", accelerator: "nvidia" },
          ],
        },
        target: {
          platform: "linux",
          architecture: "x64",
          accelerator: "nvidia",
        },
      }),
      nvidiaTopology,
    );
    try {
      expect(await readCapturedArgs(argsPath)).toEqual([
        "--diffusion-model",
        join(config.videoModelsDir, "diffusion.gguf"),
        "--t5xxl",
        join(config.videoModelsDir, "encoder.gguf"),
        "--vae",
        join(config.videoModelsDir, "vae.safetensors"),
        "--offload-to-cpu",
        "--diffusion-fa",
        "--listen-ip",
        "127.0.0.1",
        "--listen-port",
        "18003",
        "--max-vram",
        "6.5",
        ...(process.platform === "linux"
          ? ["--require-gpu-pci", "0000:ab:1f.7"]
          : []),
      ]);
    } finally {
      nativeProcess.kill();
      await nativeProcess.exited;
    }
  });
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
  await compileRuntimeFixture(binPath, { argsPath });
  process.env.PATH = `${userBinDir}:${originalPath ?? ""}`;

  return { argsPath, config, modelFile, modelPath };
}

function expectedLlamaArgs(
  modelPath: string,
  parallel: string,
  cacheType = "f16",
): string[] {
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
    "--no-kv-unified",
    "--jinja",
    "--embeddings",
    "--flash-attn",
    "on",
    "--cache-type-k",
    cacheType,
    "--cache-type-v",
    cacheType,
    "--cache-ram",
    "2048",
    "--cache-reuse",
    "256",
  ];
  return args;
}

function expectedEmbeddingArgs(modelPath: string, parallel: string): string[] {
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
    "--no-kv-unified",
    "--embedding",
    "--pooling",
    "last",
  ];
  if (process.platform === "darwin" && process.arch === "arm64") {
    args.push("--flash-attn", "auto");
  }
  args.push(
    "--cache-type-k",
    "f16",
    "--cache-type-v",
    "f16",
    "--cache-ram",
    "0",
  );
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
          model: byId(fixture.config.activeLlmModel),
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
      expectedLlamaArgs(fixture.modelPath, "4"),
    );
    expect(
      output.filter((line) => line.includes("Dynamic Concurrency")),
    ).toEqual([
      "🤖 Dynamic Concurrency: Calculated 4 parallel slots based on 9.5 GB VRAM and context memory constraints. 2048 tokens per slot.",
    ]);
  });

  test("quantizes the KV cache only for q8_0-compatible geometry", async () => {
    const fixture = await createLlamaLaunchFixture(2);
    const geometry = (q8Compatible: boolean) => ({
      architecture: "llama",
      blockCount: 4,
      fullKvHeads: 4,
      swaKvHeads: 0,
      slidingWindow: null,
      keyLength: q8Compatible ? 128 : 80,
      valueLength: q8Compatible ? 128 : 80,
      swaKeyLength: q8Compatible ? 128 : 80,
      swaValueLength: q8Compatible ? 128 : 80,
      q8Compatible,
      recurrentBytesPerSlot: 0,
      contextLength: null,
    });
    for (const compatible of [true, false]) {
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
          parallel: 2,
          artifactBytes: 1024 ** 3,
          hardware: { memoryGb: 64 },
          kvGeometry: geometry(compatible),
        }),
      );
      await readCapturedArgs(fixture.argsPath);
      process.kill();
      await process.exited;
      expect(await readCapturedArgs(fixture.argsPath)).toEqual(
        expectedLlamaArgs(fixture.modelPath, "2", compatible ? "q8_0" : "f16"),
      );
      rmSync(fixture.argsPath, { force: true });
    }
  });

  test("uses the embedding-only pooling profile", async () => {
    const fixture = await createLlamaLaunchFixture(1);
    const process = await startLlamaServerProcess(
      resolveLlmLaunchPlan({
        runtimeId: "llm:embedding:1",
        root: fixture.config.root,
        modelsDirectory: fixture.config.llmModelsDir,
        modelId: "qwen3-embedding-0.6b-q8_0",
        modelFile: fixture.modelFile,
        host: "127.0.0.1",
        port: 18000,
        ctxSize: 8192,
        parallel: 1,
        artifactBytes: 639150592,
        hardware: { memoryGb: 9.5 },
        embedding: {
          capability: "embedding-only",
          pooling: "last",
          dimensions: { minimum: 1024, maximum: 1024 },
        },
      }),
    );
    await readCapturedArgs(fixture.argsPath);
    process.kill();
    expect(await process.exited).toBe(0);

    expect(await readCapturedArgs(fixture.argsPath)).toEqual(
      expectedEmbeddingArgs(fixture.modelPath, "1"),
    );
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
    await compileSdLaunchFixture({
      binPath,
      argsPath,
      capability: "localbase-sd-gpu-pci-v1",
      environmentPath,
    });
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
        artifactBytes: 2 * 1024 ** 3,
      }),
      nvidiaTopology,
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
        ...(process.platform === "linux"
          ? ["--require-gpu-pci", "0000:ab:1f.7"]
          : []),
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
