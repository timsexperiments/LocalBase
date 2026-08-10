import { cpus, platform as osPlatform, totalmem } from "node:os";
import { z } from "zod";
import { detectLinuxGpu } from "./domains/runtime/memory/linux-memory-provider";
import { detectAppleSilicon } from "./domains/runtime/memory/macos-memory-provider";

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

export async function detectSpecs(): Promise<HostSpecs> {
  const platform = osPlatform();
  const isMac = platform === "darwin";
  const isAppleSilicon = isMac && detectAppleSilicon();

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

    const detectedGpu = await detectLinuxGpu();
    if (detectedGpu) {
      gpuName = detectedGpu.name;
      gpuVramGb = detectedGpu.vramGb;
    } else {
      gpuName = "CPU / Integrated Graphics";
    }
  } else {
    osName = platform === "win32" ? "Windows" : platform;
    ramGb = totalRamGb();
    cpuModel = firstCpuModel("Unknown CPU");
    gpuName = "Unavailable";
  }

  return { osName, ramGb, cpuModel, gpuName, gpuVramGb, isMac, isAppleSilicon };
}
