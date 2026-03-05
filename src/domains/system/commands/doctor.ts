import { detectSpecs } from "../../../system";

export function runDoctor(args: string[]): number {
  const specs = detectSpecs();
  if (args.includes("--json")) {
    console.log(JSON.stringify(specs, null, 2));
    return 0;
  }

  console.log(`OS: ${specs.osName}`);
  console.log(`CPU: ${specs.cpuModel}`);
  console.log(`RAM: ${specs.ramGb} GB`);
  console.log(`GPU: ${specs.gpuName}`);
  console.log(`GPU VRAM: ${specs.gpuVramGb} GB`);
  if (specs.gpuVramGb >= 12 && specs.ramGb >= 64) {
    console.log("Status: ✅ Good fit for multi-model coding setup on quantized 7B/14B models + local STT.");
  } else {
    console.log("Status: ⚠️ Usable, but reduce model size/context or add memory.");
  }
  return 0;
}
