import {
  allocateParallelSlots,
  type ParallelAllocation,
} from "../config/parallel";
import { ensureBinary } from "../../manager/binaries";
import type {
  ImageLaunchPlan,
  LlmLaunchPlan,
  SttLaunchPlan,
} from "./launch-plan";

export type LlamaServerArgs = {
  args: string[];
  parallel: ParallelAllocation;
};

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
  const parallel = allocateParallelSlots({
    parallel: plan.parallel,
    memoryGb: plan.hardware.memoryGb,
    modelRequirementGb: plan.modelRequirementGb,
    ctxSize: plan.ctxSize,
  });
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
    String(parallel.slots),
    "--jinja",
    "--embeddings",
  ];

  if (process.platform === "darwin" && process.arch === "arm64") {
    args.push("--flash-attn", "auto");
  }

  return { args, parallel };
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
): Promise<Bun.Subprocess> {
  if (!(await Bun.file(plan.modelPath).exists())) {
    throw new Error(`STT model file not found: ${plan.modelPath}`);
  }

  const binPath = await ensureBinary({ root: plan.root }, plan.component);
  return Bun.spawn(
    [
      binPath,
      "--model",
      plan.modelPath,
      "--host",
      plan.host,
      "--port",
      String(plan.port),
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
): Promise<Bun.Subprocess> {
  if (!(await Bun.file(plan.modelPath).exists())) {
    throw new Error(`Model file not found: ${plan.modelPath}`);
  }

  const binPath = await ensureBinary({ root: plan.root }, plan.component);
  return Bun.spawn(
    [
      binPath,
      "-m",
      plan.modelPath,
      "--listen-ip",
      plan.host,
      "--listen-port",
      String(plan.port),
    ],
    {
      stdout: "pipe",
      stderr: "pipe",
      stdin: "inherit",
      cwd: plan.workingDirectory,
    },
  );
}
