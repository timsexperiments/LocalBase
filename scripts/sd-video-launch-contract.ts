import type { VideoLaunchPlan } from "../src/domains/runtime/launch-plan";
import { buildSdVideoServerArgs } from "../src/domains/runtime/launcher";
import {
  gibibyte,
  type MemoryTopology,
} from "../src/domains/runtime/memory-safety";

function binaryFromArgs(argv: string[]): string {
  const index = argv.indexOf("--binary");
  const binary = index === -1 ? undefined : argv[index + 1];
  if (!binary) {
    throw new Error("Expected --binary <path>.");
  }
  return binary;
}

const plan: VideoLaunchPlan = {
  runtimeId: "video:launch-contract",
  modality: "video",
  component: "sd-server",
  root: "/tmp/localbase-video-launch-contract",
  modelId: "wan2.1-t2v-1.3b-q8_0",
  diffusionModelPath: "/tmp/localbase-video-launch-contract/diffusion.gguf",
  textEncoderPath: "/tmp/localbase-video-launch-contract/t5xxl.gguf",
  decoder: {
    kind: "vae",
    path: "/tmp/localbase-video-launch-contract/vae.safetensors",
  },
  audioEncoderPath: "/tmp/localbase-video-launch-contract/wav2vec2.safetensors",
  inputBounds: { maxWidth: 320, maxHeight: 320, maxFrames: 33, fps: 16 },
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
  host: "127.0.0.1",
  port: 8091,
  healthUrl: "http://127.0.0.1:8091/",
  memoryDemand: {
    unifiedBytes: 24 * gibibyte,
    hostBytes: 16 * gibibyte,
    acceleratorBytes: 8 * gibibyte,
    confidence: "estimated",
  },
  mode: "s2v",
};

const topology: MemoryTopology = {
  kind: "discrete",
  system: { id: "system", capacityBytes: 32 * gibibyte },
  accelerators: [{ id: "gpu", capacityBytes: 12 * gibibyte }],
};

async function runNative(args: string[]) {
  const child = Bun.spawn(args, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { stdout, stderr, exitCode };
}

const binary = binaryFromArgs(Bun.argv);
const args = buildSdVideoServerArgs(plan, topology);
const accepted = await runNative([binary, ...args, "--help"]);

if (accepted.exitCode !== 0) {
  throw new Error(
    `Pinned sd-server rejected LocalBase video launch arguments.\n${accepted.stdout}${accepted.stderr}`,
  );
}

const rejected = await runNative([
  binary,
  ...args,
  "--localbase-video-launch-contract-invalid",
  "--help",
]);
if (rejected.exitCode === 0) {
  throw new Error("Pinned sd-server did not parse an invalid launch argument.");
}
