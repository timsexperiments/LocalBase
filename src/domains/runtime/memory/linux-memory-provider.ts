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
  pciBusId?: string;
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

/** Converts NVML's padded domain to GGML's PCI format without truncation. */
export function parseNvmlPciInfo(info: Uint8Array): string | undefined {
  if (info.byteLength !== 68) return undefined;
  const busId = info.subarray(36, 68);
  const end = busId.indexOf(0);
  if (end < 0) return undefined;
  const match =
    /^(?:0000)?([0-9a-f]{4}):([0-9a-f]{2}):([01][0-9a-f])\.([0-7])$/i.exec(
      new TextDecoder().decode(busId.subarray(0, end)),
    );
  if (!match) return undefined;
  const [, domain, bus, device, fn] = match;
  const fields = new DataView(info.buffer, info.byteOffset, info.byteLength);
  if (
    fields.getUint32(16, true) !== Number.parseInt(domain!, 16) ||
    fields.getUint32(20, true) !== Number.parseInt(bus!, 16) ||
    fields.getUint32(24, true) !== Number.parseInt(device!, 16) ||
    (fields.getUint32(28, true) & 0xffff) !== 0x10de
  )
    return undefined;
  return `${domain}:${bus}:${device}.${fn}`.toLowerCase();
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
    type NvmlPciLibrary = {
      symbols: {
        nvmlDeviceGetPciInfo_v3(handle: unknown, pci: unknown): number;
      };
      close(): void;
    };
    let library: NvmlLibrary | undefined;
    let pciLibrary: NvmlPciLibrary | undefined;
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
        try {
          pciLibrary = dlopen(path, {
            nvmlDeviceGetPciInfo_v3: { args: ["ptr", "ptr"], returns: "i32" },
          }) as unknown as NvmlPciLibrary;
        } catch {
          // Keep memory measurement when PCI discovery is unsupported.
        }
        break;
      } catch {
        // Try the next optional driver location.
      }
    }
    if (!library) return [];
    if (library.symbols.nvmlInit_v2() !== 0) {
      pciLibrary?.close();
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
        pciLibrary?.close();
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
      // nvmlPciInfo_t: 16-byte legacy ID, five uint32 fields, 32-byte bus ID.
      // The UUID, PCI address, and memory sample all use this same handle.
      const pciInfo = new Uint8Array(68);
      const pciBusId =
        count[0] === 1 &&
        pciLibrary?.symbols.nvmlDeviceGetPciInfo_v3(handle, ptr(pciInfo)) === 0
          ? parseNvmlPciInfo(pciInfo)
          : undefined;
      devices.push({
        id,
        ...(pciBusId ? { pciBusId } : {}),
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
    .map(({ id, totalBytes, pciBusId }) => ({
      id,
      capacityBytes: totalBytes,
      ...(pciBusId ? { pciBusId } : {}),
    }));
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
