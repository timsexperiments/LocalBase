import { cpus, platform as osPlatform, totalmem } from "node:os";
import { z } from "zod";

export type HostSpecs = {
  osName: string;
  ramGb: number;
  cpuModel: string;
  gpuName: string;
  gpuVramGb: number;
  isMac: boolean;
  isAppleSilicon: boolean;
};

const nonEmptyStringSchema = z.string().trim().min(1);
const positiveNumberSchema = z.coerce.number().finite().positive();
const cpuInfoSchema = z.object({ model: nonEmptyStringSchema });

const gpuDetectionSchema = z.object({
  name: nonEmptyStringSchema,
  vramGb: z.number().finite().nonnegative(),
});

function parsePositiveNumber(value: unknown): number | undefined {
  const parsed = positiveNumberSchema.safeParse(value);
  return parsed.success ? parsed.data : undefined;
}

function bytesToGb(value: unknown): number {
  const bytes = parsePositiveNumber(value);
  return bytes === undefined ? 0 : Math.round(bytes / 1024 / 1024 / 1024);
}

function totalRamGb(): number {
  try {
    return bytesToGb(totalmem());
  } catch {
    return 0;
  }
}

function firstCpuModel(fallback: string): string {
  try {
    const parsed = cpuInfoSchema.safeParse(cpus()[0]);
    return parsed.success ? parsed.data.model : fallback;
  } catch {
    return fallback;
  }
}

export function deriveAppleGpuName(cpuModel: string): string {
  const parsed = nonEmptyStringSchema.safeParse(cpuModel);
  if (!parsed.success) return "Apple Silicon GPU";

  const chip = parsed.data.match(/\bApple\s+M\d+(?:\s+(?:Pro|Max|Ultra))?\b/i);
  return chip ? `${chip[0]} GPU` : "Apple Silicon GPU";
}

function parsePrettyName(osRelease: string): string | undefined {
  const line = osRelease
    .split(/\r?\n/)
    .find((entry) => entry.startsWith("PRETTY_NAME="));
  if (!line) return undefined;

  let value = line.slice("PRETTY_NAME=".length).trim();
  if (!value) return undefined;
  if (
    (value.startsWith('"') && value.endsWith('"')) ||
    (value.startsWith("'") && value.endsWith("'"))
  ) {
    value = value.slice(1, -1);
  }
  const parsed = nonEmptyStringSchema.safeParse(value);
  return parsed.success ? parsed.data : undefined;
}

type NvmlSymbols = {
  nvmlInit_v2(): number;
  nvmlShutdown(): number;
  nvmlDeviceGetCount_v2(count: unknown): number;
  nvmlDeviceGetHandleByIndex_v2(index: number, handle: unknown): number;
  nvmlDeviceGetMemoryInfo(handle: unknown, memory: unknown): number;
  nvmlDeviceGetName(handle: unknown, name: unknown, length: number): number;
};

type NvmlLibrary = { symbols: NvmlSymbols };

function tryNvmlFfi(): { name: string; vramGb: number } | null {
  try {
    const { dlopen, ptr } = require("bun:ffi") as typeof import("bun:ffi");
    const nvmlLibPaths = [
      "libnvidia-ml.so",
      "libnvidia-ml.so.1",
      "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so",
      "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
      "/usr/lib/wsl/lib/libnvidia-ml.so",
      "/usr/lib/wsl/lib/libnvidia-ml.so.1",
    ];

    let nvml: NvmlLibrary | null = null;
    for (const path of nvmlLibPaths) {
      try {
        nvml = dlopen(path, {
          nvmlInit_v2: {
            args: [],
            returns: "i32",
          },
          nvmlShutdown: {
            args: [],
            returns: "i32",
          },
          nvmlDeviceGetCount_v2: {
            args: ["ptr"],
            returns: "i32",
          },
          nvmlDeviceGetHandleByIndex_v2: {
            args: ["u32", "ptr"],
            returns: "i32",
          },
          nvmlDeviceGetMemoryInfo: {
            args: ["ptr", "ptr"],
            returns: "i32",
          },
          nvmlDeviceGetName: {
            args: ["ptr", "ptr", "u32"],
            returns: "i32",
          },
        }) as unknown as NvmlLibrary;
        break;
      } catch {
        // try next path
      }
    }

    if (!nvml) return null;

    const initRes = nvml.symbols.nvmlInit_v2();
    if (initRes !== 0) return null;

    try {
      const countBuf = new Uint32Array(1);
      const countRes = nvml.symbols.nvmlDeviceGetCount_v2(ptr(countBuf));
      if (countRes !== 0 || countBuf[0] === 0) return null;

      const handleBuf = new BigUint64Array(1);
      const handleRes = nvml.symbols.nvmlDeviceGetHandleByIndex_v2(
        0,
        ptr(handleBuf),
      );
      if (handleRes !== 0) return null;

      const handle = ptr(handleBuf);

      const nameBuf = new Uint8Array(64);
      const nameRes = nvml.symbols.nvmlDeviceGetName(handle, ptr(nameBuf), 64);
      let name = "NVIDIA GPU";
      if (nameRes === 0) {
        const end = nameBuf.indexOf(0);
        const parsedName = nonEmptyStringSchema.safeParse(
          new TextDecoder()
            .decode(nameBuf.subarray(0, end > 0 ? end : undefined))
            .trim(),
        );
        if (parsedName.success) name = parsedName.data;
      }

      const memBuf = new BigUint64Array(3);
      const memRes = nvml.symbols.nvmlDeviceGetMemoryInfo(handle, ptr(memBuf));
      const vramGb = memRes === 0 ? bytesToGb(memBuf[0]) : 0;

      return gpuDetectionSchema.parse({ name, vramGb });
    } finally {
      nvml.symbols.nvmlShutdown();
    }
  } catch {
    return null;
  }
}

/** Reads the standard AMD VRAM sysfs node available on Linux DRM devices. */
async function tryAmdLinux(): Promise<{
  name: string;
  vramGb: number;
} | null> {
  try {
    const vramFile = "/sys/class/drm/card0/device/mem_info_vram_total";
    const vramGb = bytesToGb(await Bun.file(vramFile).text());
    if (vramGb > 0) {
      return gpuDetectionSchema.parse({ name: "AMD GPU", vramGb });
    }
  } catch {
    // ignore
  }
  return null;
}

export async function detectSpecs(): Promise<HostSpecs> {
  const platform = osPlatform();
  const isMac = platform === "darwin";
  const isAppleSilicon = isMac && process.arch === "arm64";

  let osName = "Unknown";
  let ramGb = 0;
  let cpuModel = "Unknown";
  let gpuName = "Unavailable";
  let gpuVramGb = 0;

  if (isMac) {
    osName = "macOS";
    ramGb = totalRamGb();
    cpuModel = firstCpuModel(isAppleSilicon ? "Apple Silicon" : "Unknown CPU");

    if (isAppleSilicon) {
      gpuName = deriveAppleGpuName(cpuModel);
      gpuVramGb = ramGb;
    } else {
      gpuName = "Intel Integrated Graphics";
    }
  } else if (platform === "linux") {
    // Linux exposes hardware details through procfs and DRM sysfs.
    try {
      osName =
        parsePrettyName(await Bun.file("/etc/os-release").text()) ||
        "Unknown Linux";
    } catch {
      osName = "Unknown Linux";
    }

    let ramKb = 0;
    try {
      const memInfo = (await Bun.file("/proc/meminfo").text()).split("\n");
      const memLine = memInfo.find((line) => line.startsWith("MemTotal:"));
      if (memLine) {
        ramKb = parsePositiveNumber(memLine.split(/\s+/)[1]) ?? 0;
      }
    } catch {}
    ramGb = ramKb > 0 ? Math.round(ramKb / 1024 / 1024) : totalRamGb();

    cpuModel = "Unknown";
    try {
      const cpuInfo = (await Bun.file("/proc/cpuinfo").text()).split("\n");
      const line = cpuInfo.find((l) => l.startsWith("model name"));
      if (line) {
        const parsed = nonEmptyStringSchema.safeParse(line.split(":", 2)[1]);
        if (parsed.success) cpuModel = parsed.data;
      }
    } catch {}
    if (cpuModel === "Unknown") {
      cpuModel = firstCpuModel("Unknown CPU");
    }

    // Prefer direct NVML FFI so detection does not depend on host utilities.
    let detectedGpu = false;
    const nvmlGpu = tryNvmlFfi();
    if (nvmlGpu) {
      gpuName = nvmlGpu.name;
      gpuVramGb = nvmlGpu.vramGb;
      detectedGpu = true;
    }

    // Fallback 1: AMD Linux sysfs.
    if (!detectedGpu) {
      const amdGpu = await tryAmdLinux();
      if (amdGpu) {
        gpuName = amdGpu.name;
        gpuVramGb = amdGpu.vramGb;
        detectedGpu = true;
      }
    }

    // Fallback 2: integrated GPU or CPU-only.
    if (!detectedGpu) {
      gpuName = "CPU / Integrated Graphics";
      gpuVramGb = 0;
    }
  } else {
    osName = platform === "win32" ? "Windows" : platform;
    ramGb = totalRamGb();
    cpuModel = firstCpuModel("Unknown CPU");
    gpuName = "Unavailable";
  }

  return { osName, ramGb, cpuModel, gpuName, gpuVramGb, isMac, isAppleSilicon };
}
