import { totalmem } from "node:os";
import type {
  HostMemorySnapshot,
  MemoryPool,
  MemoryTopology,
} from "../memory-safety";
import {
  createLinuxHostMemoryProvider,
  type LinuxHostMemoryProviderOptions,
} from "./linux-memory-provider";
import {
  createMacOsHostMemoryProvider,
  type MacOsHostMemoryProviderOptions,
} from "./macos-memory-provider";

export interface HostMemoryProvider {
  readonly topology: MemoryTopology;
  snapshot(): Promise<HostMemorySnapshot>;
  close(): Promise<void>;
}

export type HostMemoryProviderOptions = Readonly<{
  platform?: NodeJS.Platform;
  linux?: LinuxHostMemoryProviderOptions;
  macos?: MacOsHostMemoryProviderOptions;
}>;

function fallbackTopology(): MemoryTopology {
  let capacityBytes = 0;
  try {
    capacityBytes = totalmem();
  } catch {
    // An unavailable capacity is represented by an unavailable snapshot.
  }
  const system: MemoryPool = { id: "system", capacityBytes };
  return { kind: "discrete", system, accelerators: [] };
}

function unavailableProvider(topology: MemoryTopology): HostMemoryProvider {
  return {
    topology,
    async snapshot() {
      return {
        capturedAtMs: Date.now(),
        pools: [
          {
            poolId: topology.system.id,
            availability: "unavailable",
            pressure: "unknown",
          },
          ...(topology.kind === "discrete"
            ? topology.accelerators.map((pool) => ({
                poolId: pool.id,
                availability: "unavailable" as const,
                pressure: "unknown" as const,
              }))
            : []),
        ],
      };
    },
    async close() {},
  };
}

export function createHostMemoryProvider(
  options: HostMemoryProviderOptions = {},
): HostMemoryProvider {
  const platform = options.platform ?? process.platform;
  try {
    if (platform === "darwin") {
      return createMacOsHostMemoryProvider(options.macos);
    }
    if (platform === "linux") {
      return createLinuxHostMemoryProvider(options.linux);
    }
  } catch {
    return unavailableProvider(fallbackTopology());
  }
  return unavailableProvider(fallbackTopology());
}

export type { HostMemorySnapshot, MemoryTopology } from "../memory-safety";
