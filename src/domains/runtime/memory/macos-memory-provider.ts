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
const SPECULATIVE_COUNT_OFFSET = 92;

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
}>;

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
  const freePages = view.getUint32(FREE_COUNT_OFFSET, true);
  const inactivePages = view.getUint32(INACTIVE_COUNT_OFFSET, true);
  const speculativePages = view.getUint32(SPECULATIVE_COUNT_OFFSET, true);
  const pages = freePages + inactivePages + speculativePages;
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
  const topology: MemoryTopology = appleSilicon
    ? { kind: "unified", system }
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
