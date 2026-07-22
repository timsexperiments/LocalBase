import { cpus, totalmem } from "node:os";
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

const commandResultSchema = z.object({
  exitCode: z.number().int(),
  stdout: z.instanceof(Uint8Array),
});

const gpuDetectionSchema = z.object({
  name: z.string().min(1),
  vramGb: z.number().finite().nonnegative(),
});

const nvidiaSmiOutputSchema = z.object({
  name: z.string().min(1),
  memoryMb: z.number().finite().positive(),
});

function run(cmd: string, args: string[]): string {
  const result = commandResultSchema.parse(
    Bun.spawnSync([cmd, ...args], {
      stdout: "pipe",
      stderr: "ignore",
      timeout: 3000,
      killSignal: "SIGKILL",
    }),
  );
  if (result.exitCode !== 0) return "";
  return new TextDecoder().decode(result.stdout).trim();
}

function parsePrettyName(osRelease: string): string | undefined {
  const line = osRelease
    .split(/\r?\n/)
    .find((entry) => entry.startsWith("PRETTY_NAME="));
  if (!line) return undefined;

  const value = line.slice("PRETTY_NAME=".length).trim();
  if (!value) return undefined;
  if (
    (value.startsWith('"') && value.endsWith('"')) ||
    (value.startsWith("'") && value.endsWith("'"))
  ) {
    return value.slice(1, -1);
  }
  return value;
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
        name = new TextDecoder()
          .decode(nameBuf.subarray(0, end > 0 ? end : undefined))
          .trim();
      }

      const memBuf = new BigUint64Array(3);
      const memRes = nvml.symbols.nvmlDeviceGetMemoryInfo(handle, ptr(memBuf));
      let vramGb = 0;
      if (memRes === 0) {
        vramGb = Math.round(Number(memBuf[0]) / 1024 / 1024 / 1024);
      }

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
    const bytes = Number((await Bun.file(vramFile).text()).trim());
    if (Number.isFinite(bytes) && bytes > 0) {
      const vramGb = Math.round(bytes / 1024 / 1024 / 1024);
      return gpuDetectionSchema.parse({ name: "AMD GPU", vramGb });
    }
  } catch {
    // ignore
  }
  return null;
}

export async function detectSpecs(): Promise<HostSpecs> {
  const platform = process.platform;
  const isMac = platform === "darwin";
  const isAppleSilicon = isMac && process.arch === "arm64";

  let osName = "Unknown";
  let ramGb = 0;
  let cpuModel = "Unknown";
  let gpuName = "Unavailable";
  let gpuVramGb = 0;

  if (isMac) {
    const version = run("sw_vers", ["-productVersion"]) || "Unknown";
    osName = `macOS ${version}`;

    try {
      const memBytesStr = run("sysctl", ["-n", "hw.memsize"]);
      const memBytes = Number(memBytesStr);
      if (Number.isFinite(memBytes) && memBytes > 0) {
        ramGb = Math.round(memBytes / 1024 / 1024 / 1024);
      } else {
        ramGb = Math.round(totalmem() / 1024 / 1024 / 1024);
      }
    } catch {
      ramGb = Math.round(totalmem() / 1024 / 1024 / 1024);
    }

    cpuModel =
      run("sysctl", ["-n", "machdep.cpu.brand_string"]) ||
      cpus()[0]?.model ||
      "Apple Silicon";

    if (isAppleSilicon) {
      const profilerOut = run("system_profiler", ["SPDisplaysDataType"]);
      const match = profilerOut.match(/Chipset Model:\s*(.+)/);
      gpuName = match ? match[1].trim() : cpuModel;
      gpuVramGb = ramGb; // Unified memory is shared and fully accessible
    } else {
      const profilerOut = run("system_profiler", ["SPDisplaysDataType"]);
      const match = profilerOut.match(/Chipset Model:\s*(.+)/);
      gpuName = match ? match[1].trim() : "Intel Integrated Graphics";
      const vramMatch = profilerOut.match(/VRAM \(Total\):\s*(.+)/);
      if (vramMatch) {
        const vramStr = vramMatch[1].trim();
        const val = parseFloat(vramStr);
        if (!isNaN(val)) {
          gpuVramGb = vramStr.toLowerCase().includes("mb")
            ? Math.round(val / 1024)
            : Math.round(val);
        }
      }
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
        ramKb = Number(memLine.split(/\s+/)[1]);
      }
    } catch {}
    if (ramKb > 0) {
      ramGb = Math.round(ramKb / 1024 / 1024);
    } else {
      ramGb = Math.round(totalmem() / 1024 / 1024 / 1024);
    }

    cpuModel = "Unknown";
    try {
      const cpuInfo = (await Bun.file("/proc/cpuinfo").text()).split("\n");
      const line = cpuInfo.find((l) => l.startsWith("model name"));
      if (line) {
        cpuModel = line.split(":", 2)[1]?.trim() ?? cpuModel;
      }
    } catch {}
    if (cpuModel === "Unknown") {
      cpuModel = cpus()[0]?.model ?? "Unknown CPU";
    }

    // Prefer nvidia-smi when it is installed and exits successfully.
    let detectedGpu = false;
    if (Bun.which("nvidia-smi")) {
      const raw = run("nvidia-smi", [
        "--query-gpu=name,memory.total",
        "--format=csv,noheader,nounits",
      ]);
      const first = raw.split("\n")[0] ?? "";
      if (first.includes(",")) {
        const [name, memMbRaw] = first.split(",", 2).map((item) => item.trim());
        const parsed = nvidiaSmiOutputSchema.safeParse({
          name,
          memoryMb: Number(memMbRaw),
        });
        if (parsed.success) {
          gpuName = parsed.data.name;
          detectedGpu = true;
          gpuVramGb = Math.round(parsed.data.memoryMb / 1024);
        }
      }
    }

    // Fallback 1: Try NVML FFI directly
    if (!detectedGpu) {
      const nvmlGpu = tryNvmlFfi();
      if (nvmlGpu) {
        gpuName = nvmlGpu.name;
        gpuVramGb = nvmlGpu.vramGb;
        detectedGpu = true;
      }
    }

    // Fallback 2: AMD Linux sysfs.
    if (!detectedGpu) {
      const amdGpu = await tryAmdLinux();
      if (amdGpu) {
        gpuName = amdGpu.name;
        gpuVramGb = amdGpu.vramGb;
        detectedGpu = true;
      }
    }

    // Fallback 3: integrated GPU or CPU-only.
    if (!detectedGpu) {
      gpuName = "CPU / Integrated Graphics";
      gpuVramGb = 0;
    }
  } else {
    osName = platform === "win32" ? "Windows" : platform;
    ramGb = Math.round(totalmem() / 1024 / 1024 / 1024);
    cpuModel = cpus()[0]?.model ?? "Unknown CPU";
    gpuName = "Unavailable";
  }

  return { osName, ramGb, cpuModel, gpuName, gpuVramGb, isMac, isAppleSilicon };
}
