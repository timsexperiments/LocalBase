import { delimiter, dirname, resolve } from "node:path";
import type { ParallelAllocation } from "../config/parallel";
import { ensureBinary } from "../../manager/binaries";
import type { MemoryTopology } from "./memory-safety";
import { requireSdGpuContract, sdGpuArgs } from "./sd-gpu";
import { requireWhisperGpuContract, whisperGpuArgs } from "./whisper-gpu";
import type {
  ImageLaunchPlan,
  LlmLaunchPlan,
  SttLaunchPlan,
  VideoLaunchPlan,
} from "./launch-plan";

export type LlamaServerArgs = {
  args: string[];
  parallel: ParallelAllocation;
};

export function sdServerEnvironment(
  binaryPath: string,
  platform = process.platform,
): NodeJS.ProcessEnv {
  if (platform !== "linux") return process.env;
  return {
    ...process.env,
    LD_LIBRARY_PATH: [dirname(binaryPath), process.env.LD_LIBRARY_PATH]
      .filter((path): path is string => Boolean(path))
      .join(delimiter),
  };
}

function logAutoParallel(
  parallel: ParallelAllocation,
  hardware: LlmLaunchPlan["hardware"],
): void {
  console.log(
    `🤖 Dynamic Concurrency: Calculated ${parallel.slots} parallel slots based on ${hardware.memoryGb} GB VRAM and context memory constraints. ${parallel.contextPerSlot} tokens per slot.`,
  );
}

/** Builds the resolved llama-server arguments for one launch plan. */
export function buildLlamaServerArgs(
  plan: Pick<
    LlmLaunchPlan,
    | "modelPath"
    | "host"
    | "port"
    | "ctxSize"
    | "parallel"
    | "modelRequirementGb"
    | "hardware"
  >,
): LlamaServerArgs {
  const args = [
    "-m",
    plan.modelPath,
    "--host",
    plan.host,
    "--port",
    String(plan.port),
    "-c",
    String(plan.ctxSize),
    "--parallel",
    String(plan.parallel.slots),
    "--jinja",
    "--embeddings",
  ];

  if (process.platform === "darwin" && process.arch === "arm64") {
    args.push("--flash-attn", "auto");
  }

  return { args, parallel: plan.parallel };
}

export async function startLlamaServerProcess(
  plan: LlmLaunchPlan,
): Promise<Bun.Subprocess> {
  if (!(await Bun.file(plan.modelPath).exists())) {
    throw new Error(`Model file not found: ${plan.modelPath}`);
  }

  const binPath = await ensureBinary({ root: plan.root }, plan.component);
  const launch = buildLlamaServerArgs(plan);
  if (launch.parallel.isAuto) logAutoParallel(launch.parallel, plan.hardware);

  return Bun.spawn([binPath, ...launch.args], {
    stdout: "pipe",
    stderr: "pipe",
    stdin: "inherit",
  });
}

export async function startWhisperServerProcess(
  plan: SttLaunchPlan,
  topology: MemoryTopology,
): Promise<Bun.Subprocess> {
  if (!(await Bun.file(plan.modelPath).exists())) {
    throw new Error(`STT model file not found: ${plan.modelPath}`);
  }

  const gpuArgs = whisperGpuArgs(process.platform, topology);
  const binPath = await ensureBinary({ root: plan.root }, plan.component);
  if (process.platform === "linux") await requireWhisperGpuContract(binPath);
  return Bun.spawn(
    [
      binPath,
      "--model",
      plan.modelPath,
      "--host",
      plan.host,
      "--port",
      String(plan.port),
      ...gpuArgs,
    ],
    {
      stdout: "pipe",
      stderr: "pipe",
      stdin: "inherit",
    },
  );
}

export async function startSdServerProcess(
  plan: ImageLaunchPlan,
  topology: MemoryTopology,
): Promise<Bun.Subprocess> {
  if (!(await Bun.file(plan.modelPath).exists())) {
    throw new Error(`Model file not found: ${plan.modelPath}`);
  }

  const binPath = resolve(
    await ensureBinary({ root: plan.root }, plan.component),
  );
  const gpuArgs = sdGpuArgs(process.platform, topology);
  if (process.platform === "linux") await requireSdGpuContract(binPath);
  return Bun.spawn(
    [
      binPath,
      "-m",
      plan.modelPath,
      "--listen-ip",
      plan.host,
      "--listen-port",
      String(plan.port),
      ...gpuArgs,
    ],
    {
      stdout: "pipe",
      stderr: "pipe",
      stdin: "inherit",
      cwd: dirname(binPath),
      env: sdServerEnvironment(binPath),
    },
  );
}

export async function startSdVideoServerProcess(
  plan: VideoLaunchPlan,
  topology: MemoryTopology,
): Promise<Bun.Subprocess> {
  for (const path of [
    plan.diffusionModelPath,
    plan.textEncoderPath,
    plan.vaePath,
    ...(plan.mode === "s2v" ? [plan.audioEncoderPath] : []),
  ]) {
    if (!(await Bun.file(path).exists())) {
      throw new Error("Configured video artifact does not exist.");
    }
  }
  const binPath = resolve(
    await ensureBinary({ root: plan.root }, plan.component),
  );
  const gpuArgs = sdGpuArgs(process.platform, topology);
  if (process.platform === "linux") await requireSdGpuContract(binPath);
  const args = buildSdVideoServerArgs(plan);
  return Bun.spawn([binPath, ...args, ...gpuArgs], {
    stdout: "pipe",
    stderr: "pipe",
    stdin: "inherit",
    cwd: dirname(binPath),
    env: sdServerEnvironment(binPath),
  });
}

/** Builds the pinned sd-server `vid_gen` argv without touching the filesystem. */
export function buildSdVideoServerArgs(plan: VideoLaunchPlan): string[] {
  return [
    "--diffusion-model",
    plan.diffusionModelPath,
    "--t5xxl",
    plan.textEncoderPath,
    "--vae",
    plan.vaePath,
    ...(plan.mode === "s2v" ? ["--audio-encoder", plan.audioEncoderPath] : []),
    ...(plan.launchOptions.cpuOffload ? ["--offload-to-cpu"] : []),
    ...(plan.launchOptions.diffusionFlashAttention ? ["--diffusion-fa"] : []),
    "--listen-ip",
    plan.host,
    "--listen-port",
    String(plan.port),
  ];
}
