import type { AppContext } from "../../../context";

export function runDoctor(args: string[], ctx: AppContext): number {
  const specs = ctx.specs;
  if (args.includes("--json")) {
    console.log(JSON.stringify(specs, null, 2));
    return 0;
  }

  console.log(`OS: ${specs.osName}`);
  console.log(`CPU: ${specs.cpuModel}`);
  console.log(`RAM: ${specs.ramGb} GB`);
  console.log(`GPU: ${specs.gpuName}`);
  if (specs.isAppleSilicon) {
    console.log(`GPU VRAM: ${specs.gpuVramGb} GB (Unified Memory)`);
  } else {
    console.log(`GPU VRAM: ${specs.gpuVramGb} GB (Discrete Memory)`);
  }

  if (specs.isAppleSilicon) {
    if (specs.ramGb >= 64) {
      console.log("Status: ✅ Excellent fit for large developer-grade models (up to 32B/70B) using unified memory.");
    } else if (specs.ramGb >= 32) {
      console.log("Status: ✅ Good fit for medium coding models (up to 14B/32B) using unified memory.");
    } else if (specs.ramGb >= 16) {
      console.log("Status: ⚠️ Good for entry coding models (1.5B/7B), but larger models may slow down.");
    } else {
      console.log("Status: ❌ Low memory. Limit to small models (1.5B) and small contexts.");
    }
  } else {
    if (specs.gpuVramGb >= 16) {
      console.log("Status: ✅ Excellent fit for multi-model coding setup (7B/14B models + local STT).");
    } else if (specs.gpuVramGb >= 12) {
      console.log("Status: ✅ Good fit for 7B models with plenty of context headroom.");
    } else if (specs.gpuVramGb >= 8) {
      console.log("Status: ⚠️ Tight fit for 7B models (warning: context growth may slow down), or perfect for 3B/1.5B models.");
    } else {
      console.log("Status: ❌ Low VRAM. Focus on 1.5B/3B models or CPU offloading.");
    }
  }
  return 0;
}

