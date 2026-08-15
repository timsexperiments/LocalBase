import type { HostMemorySnapshot, MemoryTopology } from "../memory-safety";
import type { HostMemoryProvider } from "./host-memory-provider";

const gibibyte = 1024 ** 3;

export type LinuxMemoryFileReader = (path: string) => Promise<string>;

export type LinuxMemoryInfo = Readonly<{
  totalBytes: number;
  availableBytes: number;
}>;

export type LinuxAccelerator = Readonly<{
  id: string;
  totalBytes: number;
  readMemory(): Promise<
    Readonly<{ totalBytes: number; availableBytes: number }> | undefined
  >;
  close(): void;
}>;

export type LinuxHostMemoryProviderOptions = Readonly<{
  readFile?: LinuxMemoryFileReader;
  accelerators?: readonly LinuxAccelerator[];
}>;

export function parseLinuxMemoryInfo(
  text: string,
): LinuxMemoryInfo | undefined {
  const values = new Map<string, number>();
  for (const line of text.split(/\r?\n/)) {
    if (!line.trim()) continue;
    const match = /^(MemTotal|MemAvailable):\s+(\d+)\s+kB\s*$/.exec(line);
    if (!match || values.has(match[1])) {
      if (line.startsWith("MemTotal:") || line.startsWith("MemAvailable:")) {
        return undefined;
      }
      continue;
    }
    const kilobytes = Number(match[2]);
    const bytes = kilobytes * 1024;
    if (!Number.isSafeInteger(bytes)) return undefined;
    values.set(match[1], bytes);
  }

  const totalBytes = values.get("MemTotal");
  const availableBytes = values.get("MemAvailable");
  if (
    totalBytes === undefined ||
    availableBytes === undefined ||
    totalBytes <= 0 ||
    availableBytes < 0 ||
    availableBytes > totalBytes
  ) {
    return undefined;
  }
  return { totalBytes, availableBytes };
}

function unavailablePool(poolId: string) {
  return {
    poolId,
    availability: "unavailable" as const,
    pressure: "unknown" as const,
  };
}

function defaultFileReader(path: string): Promise<string> {
  return Bun.file(path).text();
}

function discoverAmdCardPaths(): string[] {
  try {
    return [
      ...new Bun.Glob("card*/device/mem_info_vram_total").scanSync({
        cwd: "/sys/class/drm",
      }),
    ].map(
      (path) =>
        `/sys/class/drm/${path.replace(/\/device\/mem_info_vram_total$/, "")}`,
    );
  } catch {
    return [];
  }
}

function parseBytes(text: string): number | undefined {
  const value = text.trim();
  if (!/^\d+$/.test(value)) return undefined;
  const bytes = Number(value);
  return Number.isSafeInteger(bytes) && bytes >= 0 ? bytes : undefined;
}

export function calculateAmdAvailableBytes(
  totalBytes: number,
  usedBytes: number,
): number | undefined {
  if (
    !Number.isSafeInteger(totalBytes) ||
    !Number.isSafeInteger(usedBytes) ||
    totalBytes < 0 ||
    usedBytes < 0
  ) {
    return undefined;
  }
  return Math.max(totalBytes - usedBytes, 0);
}

function createAmdAccelerators(
  readFile: LinuxMemoryFileReader,
): readonly LinuxAccelerator[] {
  return discoverAmdCardPaths().map((cardPath) => {
    const totalPath = `${cardPath}/device/mem_info_vram_total`;
    const usedPath = `${cardPath}/device/mem_info_vram_used`;
    return {
      id: `amd:${cardPath.split("/").at(-1)}`,
      totalBytes: 0,
      async readMemory() {
        const [totalText, usedText] = await Promise.all([
          readFile(totalPath),
          readFile(usedPath),
        ]);
        const totalBytes = parseBytes(totalText);
        const usedBytes = parseBytes(usedText);
        if (totalBytes === undefined || usedBytes === undefined) {
          return undefined;
        }
        const availableBytes = calculateAmdAvailableBytes(
          totalBytes,
          usedBytes,
        );
        return availableBytes === undefined
          ? undefined
          : { totalBytes, availableBytes };
      },
      close() {},
    };
  });
}

function createNvmlAccelerators(): readonly LinuxAccelerator[] {
  let closeOnFailure: () => void = () => {};
  try {
    const { dlopen, ptr } = require("bun:ffi") as typeof import("bun:ffi");
    const paths = [
      "libnvidia-ml.so",
      "libnvidia-ml.so.1",
      "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so",
      "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
      "/usr/lib/wsl/lib/libnvidia-ml.so",
      "/usr/lib/wsl/lib/libnvidia-ml.so.1",
    ];
    type NvmlSymbols = {
      nvmlInit_v2(): number;
      nvmlShutdown(): number;
      nvmlDeviceGetCount_v2(count: unknown): number;
      nvmlDeviceGetHandleByIndex_v2(index: number, handle: unknown): number;
      nvmlDeviceGetMemoryInfo(handle: unknown, memory: unknown): number;
      nvmlDeviceGetUUID(handle: unknown, uuid: unknown, length: number): number;
    };
    type NvmlLibrary = { symbols: NvmlSymbols; close(): void };
    let library: NvmlLibrary | undefined;
    for (const path of paths) {
      try {
        library = dlopen(path, {
          nvmlInit_v2: { args: [], returns: "i32" },
          nvmlShutdown: { args: [], returns: "i32" },
          nvmlDeviceGetCount_v2: { args: ["ptr"], returns: "i32" },
          nvmlDeviceGetHandleByIndex_v2: {
            args: ["u32", "ptr"],
            returns: "i32",
          },
          nvmlDeviceGetMemoryInfo: { args: ["ptr", "ptr"], returns: "i32" },
          nvmlDeviceGetUUID: { args: ["ptr", "ptr", "u32"], returns: "i32" },
        }) as unknown as NvmlLibrary;
        break;
      } catch {
        // Try the next optional driver location.
      }
    }
    if (!library) return [];
    if (library.symbols.nvmlInit_v2() !== 0) {
      library.close();
      return [];
    }
    const nvml = library;
    let closed = false;
    const closeNvml = () => {
      if (closed) return;
      closed = true;
      try {
        nvml.symbols.nvmlShutdown();
      } finally {
        nvml.close();
      }
    };
    closeOnFailure = closeNvml;

    const count = new Uint32Array(1);
    if (library.symbols.nvmlDeviceGetCount_v2(ptr(count)) !== 0) {
      closeNvml();
      return [];
    }

    const devices: LinuxAccelerator[] = [];
    for (let index = 0; index < count[0]; index += 1) {
      const handleBuffer = new BigUint64Array(1);
      if (
        library.symbols.nvmlDeviceGetHandleByIndex_v2(
          index,
          ptr(handleBuffer),
        ) !== 0
      ) {
        continue;
      }
      const handle = Number(handleBuffer[0]);
      if (!Number.isSafeInteger(handle) || handle === 0) continue;
      const uuidBuffer = new Uint8Array(96);
      const uuidResult = library.symbols.nvmlDeviceGetUUID(
        handle,
        ptr(uuidBuffer),
        uuidBuffer.byteLength,
      );
      const decode = (buffer: Uint8Array, result: number, fallback: string) => {
        if (result !== 0) return fallback;
        const end = buffer.indexOf(0);
        const value = new TextDecoder()
          .decode(buffer.subarray(0, end < 0 ? buffer.length : end))
          .trim();
        return value || fallback;
      };
      const id = `nvidia:${decode(uuidBuffer, uuidResult, `index-${index}`)}`;
      devices.push({
        id,
        totalBytes: 0,
        async readMemory() {
          const memory = new BigUint64Array(3);
          const result = nvml.symbols.nvmlDeviceGetMemoryInfo(
            handle,
            ptr(memory),
          );
          if (result !== 0) {
            return undefined;
          }
          const totalBytes = Number(memory[0]);
          const availableBytes = Number(memory[1]);
          if (
            !Number.isSafeInteger(totalBytes) ||
            !Number.isSafeInteger(availableBytes)
          ) {
            return undefined;
          }
          return { totalBytes, availableBytes };
        },
        close: closeNvml,
      });
    }
    if (devices.length === 0) closeNvml();
    return devices;
  } catch {
    closeOnFailure();
    return [];
  }
}

function acceleratorPools(accelerators: readonly LinuxAccelerator[]) {
  return accelerators
    .filter((accelerator) => accelerator.totalBytes >= 0)
    .map(({ id, totalBytes }) => ({ id, capacityBytes: totalBytes }));
}

export function createLinuxHostMemoryProvider(
  options: LinuxHostMemoryProviderOptions = {},
): HostMemoryProvider {
  const readFile = options.readFile ?? defaultFileReader;
  const accelerators = options.accelerators ?? [
    ...createNvmlAccelerators(),
    ...createAmdAccelerators(readFile),
  ];
  const memory = { id: "system", capacityBytes: 0 };
  const acceleratorPoolList = acceleratorPools(accelerators);
  const topology: MemoryTopology = {
    kind: "discrete",
    system: memory,
    accelerators: acceleratorPoolList,
  };

  return {
    topology,
    async snapshot(): Promise<HostMemorySnapshot> {
      let system: LinuxMemoryInfo | undefined;
      try {
        system = parseLinuxMemoryInfo(await readFile("/proc/meminfo"));
      } catch {
        system = undefined;
      }
      const pools = [
        system
          ? {
              poolId: "system",
              availability: "available" as const,
              availableBytes: system.availableBytes,
              pressure: "normal" as const,
            }
          : unavailablePool("system"),
      ];
      memory.capacityBytes = system?.totalBytes ?? memory.capacityBytes;
      for (const accelerator of accelerators) {
        let sample: Awaited<ReturnType<LinuxAccelerator["readMemory"]>>;
        try {
          sample = await accelerator.readMemory();
        } catch {
          sample = undefined;
        }
        pools.push(
          sample
            ? {
                poolId: accelerator.id,
                availability: "available" as const,
                availableBytes: sample.availableBytes,
                pressure: "normal" as const,
              }
            : unavailablePool(accelerator.id),
        );
        if (sample) {
          const pool = acceleratorPoolList.find(
            (entry) => entry.id === accelerator.id,
          );
          if (pool) pool.capacityBytes = sample.totalBytes;
        }
      }
      return { capturedAtMs: Date.now(), pools };
    },
    async close() {
      for (const accelerator of accelerators) accelerator.close();
    },
  };
}

export async function detectLinuxGpu(): Promise<
  { name: string; vramGb: number } | undefined
> {
  const provider = createLinuxHostMemoryProvider();
  try {
    const topology = provider.topology;
    const accelerator =
      topology.kind === "discrete" ? topology.accelerators[0] : undefined;
    if (!accelerator) return undefined;
    const snapshot = await provider.snapshot();
    const pool = snapshot.pools.find(
      (entry) => entry.poolId === accelerator.id,
    );
    if (!pool || pool.availability !== "available") return undefined;
    return {
      name: accelerator.id.startsWith("nvidia:") ? "NVIDIA GPU" : "AMD GPU",
      vramGb: Math.round(accelerator.capacityBytes / gibibyte),
    };
  } finally {
    await provider.close();
  }
}
