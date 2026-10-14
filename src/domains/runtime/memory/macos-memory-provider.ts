import { totalmem } from "node:os";
import type {
  HostMemorySnapshot,
  MemoryPool,
  MemoryTopology,
} from "../memory-safety";
import type { HostMemoryProvider } from "./host-memory-provider";

const HOST_VM_INFO64 = 4;
const MINIMUM_VM_STATISTICS_BYTES = 96;
const FREE_COUNT_OFFSET = 0;
const INACTIVE_COUNT_OFFSET = 8;

export type MachMemorySample = Readonly<{
  buffer: Uint8Array;
  pageSize: number;
}>;

export type MachMemoryReader = Readonly<{
  read(): MachMemorySample | undefined;
  close(): void;
}>;

export type MacOsHostMemoryProviderOptions = Readonly<{
  memoryReader?: MachMemoryReader;
  totalMemoryBytes?: number;
  appleSilicon?: boolean;
  /**
   * Explicit unified GPU working-set cap in bytes. Takes precedence over the
   * `LOCALBASE_GPU_WORKING_SET_GB` environment variable, the Metal probe and
   * the heuristic. Only consulted on Apple Silicon.
   */
  gpuWorkingSetBytes?: number;
  /** Replaces the Metal probe (tests); return null when unavailable. */
  metalWorkingSetProbe?: () => number | null;
  /** Environment used for the override variable (defaults to process.env). */
  env?: Readonly<Record<string, string | undefined>>;
}>;

const GIB = 1024 ** 3;
export const GPU_WORKING_SET_ENV = "LOCALBASE_GPU_WORKING_SET_GB";

/**
 * Conservative fallback for Metal's `recommendedMaxWorkingSetSize` when the
 * probe is unavailable: the commonly cited default GPU wired limit is about two
 * thirds of unified memory on machines with at most 36 GiB and about three
 * quarters above that (tunable via `sysctl iogpu.wired_limit_mb`). Real
 * devices may report more (a 64 GiB M-series reports ~0.81x), so the probe wins
 * whenever it is available.
 */
export function appleGpuWorkingSetHeuristic(totalBytes: number): number {
  if (!Number.isFinite(totalBytes) || totalBytes <= 0) return 0;
  return Math.floor(
    totalBytes <= 36 * GIB ? (totalBytes * 2) / 3 : (totalBytes * 3) / 4,
  );
}

let cachedMetalProbe: number | null | undefined;

/**
 * Reads `MTLDevice.recommendedMaxWorkingSetSize` for the default Metal device
 * once per process via Bun FFI. Returns null when Metal or FFI is unavailable.
 */
export function probeMetalRecommendedWorkingSet(): number | null {
  if (cachedMetalProbe !== undefined) return cachedMetalProbe;
  cachedMetalProbe = null;
  try {
    const { dlopen } = require("bun:ffi") as typeof import("bun:ffi");
    const metal = dlopen("/System/Library/Frameworks/Metal.framework/Metal", {
      MTLCreateSystemDefaultDevice: { args: [], returns: "ptr" },
    });
    const objc = dlopen("/usr/lib/libobjc.A.dylib", {
      sel_registerName: { args: ["cstring"], returns: "ptr" },
      objc_msgSend: { args: ["ptr", "ptr"], returns: "u64" },
    });
    const device = metal.symbols.MTLCreateSystemDefaultDevice();
    if (device) {
      const selector = objc.symbols.sel_registerName(
        cstring("recommendedMaxWorkingSetSize"),
      );
      if (selector) {
        const value = Number(objc.symbols.objc_msgSend(device, selector));
        if (Number.isSafeInteger(value) && value > 0) cachedMetalProbe = value;
      }
    }
  } catch {
    cachedMetalProbe = null;
  }
  return cachedMetalProbe;
}

function positiveBytes(value: number | null | undefined): number | undefined {
  return value !== null &&
    value !== undefined &&
    Number.isFinite(value) &&
    value > 0
    ? Math.floor(value)
    : undefined;
}

/** Resolution order: option, env override (GiB), Metal probe, heuristic. */
export function resolveGpuWorkingSetBytes(
  options: MacOsHostMemoryProviderOptions,
  totalBytes: number,
): number | undefined {
  const explicit = positiveBytes(options.gpuWorkingSetBytes);
  if (explicit !== undefined) return explicit;
  const raw = (options.env ?? process.env)[GPU_WORKING_SET_ENV]?.trim();
  if (raw) {
    const gb = Number(raw);
    const fromEnv = positiveBytes(Number.isFinite(gb) ? gb * GIB : undefined);
    if (fromEnv !== undefined) return fromEnv;
  }
  const probed = positiveBytes(
    (options.metalWorkingSetProbe ?? probeMetalRecommendedWorkingSet)(),
  );
  if (probed !== undefined) return probed;
  return positiveBytes(appleGpuWorkingSetHeuristic(totalBytes));
}

export function parseMachAvailableBytes(
  buffer: Uint8Array,
  pageSize: number,
): number | undefined {
  if (!Number.isSafeInteger(pageSize) || pageSize <= 0) return undefined;
  if (buffer.byteLength < MINIMUM_VM_STATISTICS_BYTES) return undefined;

  const view = new DataView(
    buffer.buffer,
    buffer.byteOffset,
    buffer.byteLength,
  );
  // XNU already includes speculative pages in free_count.
  const freePages = view.getUint32(FREE_COUNT_OFFSET, true);
  const inactivePages = view.getUint32(INACTIVE_COUNT_OFFSET, true);
  const pages = freePages + inactivePages;
  const bytes = pages * pageSize;
  return Number.isSafeInteger(bytes) ? bytes : undefined;
}

function unavailablePool(poolId: string) {
  return {
    poolId,
    availability: "unavailable" as const,
    pressure: "unknown" as const,
  };
}

function totalMemory(): number {
  try {
    const value = totalmem();
    return Number.isSafeInteger(value) && value > 0 ? value : 0;
  } catch {
    return 0;
  }
}

function cstring(value: string): Buffer {
  return Buffer.from(`${value}\0`);
}

function createMachMemoryReader(): MachMemoryReader | undefined {
  let closeLibrary: () => void = () => {};
  try {
    const { dlopen, ptr } = require("bun:ffi") as typeof import("bun:ffi");
    const library = dlopen("/usr/lib/libSystem.B.dylib", {
      mach_host_self: { args: [], returns: "u32" },
      host_page_size: { args: ["u32", "ptr"], returns: "i32" },
      host_statistics64: {
        args: ["u32", "i32", "ptr", "ptr"],
        returns: "i32",
      },
    });
    closeLibrary = () => library.close();
    const host = library.symbols.mach_host_self();
    const pageSizeBuffer = new Uint32Array(1);
    const statisticsBuffer = new Uint8Array(1024);
    const countBuffer = new Uint32Array([statisticsBuffer.byteLength / 4]);

    let closed = false;
    return {
      read() {
        if (closed) return undefined;
        const pageResult = library.symbols.host_page_size(
          host,
          ptr(pageSizeBuffer),
        );
        if (pageResult !== 0) return undefined;

        countBuffer[0] = statisticsBuffer.byteLength / 4;
        const statisticsResult = library.symbols.host_statistics64(
          host,
          HOST_VM_INFO64,
          ptr(statisticsBuffer),
          ptr(countBuffer),
        );
        if (statisticsResult !== 0) return undefined;

        const count = countBuffer[0];
        if (!Number.isSafeInteger(count) || count < 24 || count > 256) {
          return undefined;
        }
        return {
          buffer: statisticsBuffer.subarray(0, count * 4),
          pageSize: pageSizeBuffer[0],
        };
      },
      close() {
        if (closed) return;
        closed = true;
        library.close();
      },
    };
  } catch {
    try {
      closeLibrary();
    } catch {
      // Ignore cleanup failures while returning an unavailable provider.
    }
    return undefined;
  }
}

export function detectAppleSilicon(): boolean {
  try {
    const { dlopen, ptr } = require("bun:ffi") as typeof import("bun:ffi");
    const library = dlopen("/usr/lib/libSystem.B.dylib", {
      sysctlbyname: {
        args: ["cstring", "ptr", "ptr", "ptr", "usize"],
        returns: "i32",
      },
    });
    const value = new Uint32Array(1);
    const size = new BigUint64Array([4n]);
    try {
      const result = library.symbols.sysctlbyname(
        cstring("hw.optional.arm64"),
        ptr(value),
        ptr(size),
        0,
        0,
      );
      return result === 0 ? value[0] === 1 : process.arch === "arm64";
    } finally {
      library.close();
    }
  } catch {
    return process.arch === "arm64";
  }
}

export function createMacOsHostMemoryProvider(
  options: MacOsHostMemoryProviderOptions = {},
): HostMemoryProvider {
  const appleSilicon = options.appleSilicon ?? detectAppleSilicon();
  const capacityBytes = options.totalMemoryBytes ?? totalMemory();
  const system: MemoryPool = { id: "system", capacityBytes };
  const gpuWorkingSetBytes = appleSilicon
    ? resolveGpuWorkingSetBytes(options, capacityBytes)
    : undefined;
  const topology: MemoryTopology = appleSilicon
    ? {
        kind: "unified",
        system,
        ...(gpuWorkingSetBytes !== undefined ? { gpuWorkingSetBytes } : {}),
      }
    : { kind: "discrete", system, accelerators: [] };
  const reader = options.memoryReader ?? createMachMemoryReader();

  return {
    topology,
    async snapshot(): Promise<HostMemorySnapshot> {
      const sample = reader?.read();
      const availableBytes = sample
        ? parseMachAvailableBytes(sample.buffer, sample.pageSize)
        : undefined;
      return {
        capturedAtMs: Date.now(),
        pools:
          availableBytes === undefined
            ? [unavailablePool(system.id)]
            : [
                {
                  poolId: system.id,
                  availability: "available",
                  availableBytes,
                  pressure: "normal",
                },
              ],
      };
    },
    async close() {
      reader?.close();
    },
  };
}
