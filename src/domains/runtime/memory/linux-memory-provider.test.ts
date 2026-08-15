import { expect, test } from "bun:test";
import {
  calculateAmdAvailableBytes,
  createLinuxHostMemoryProvider,
  parseLinuxMemoryInfo,
  type LinuxAccelerator,
} from "./linux-memory-provider";

const gibibyte = 1024 ** 3;

test("parses authoritative Linux memory fields", () => {
  expect(
    parseLinuxMemoryInfo(
      "MemTotal:       16777216 kB\nMemAvailable:    8388608 kB\n",
    ),
  ).toEqual({
    totalBytes: 16 * gibibyte,
    availableBytes: 8 * gibibyte,
  });
});

test("rejects missing, malformed, duplicate, and out-of-range fields", () => {
  expect(parseLinuxMemoryInfo("MemTotal: 100 kB\n")).toBeUndefined();
  expect(
    parseLinuxMemoryInfo("MemTotal: 100 MB\nMemAvailable: 50 kB\n"),
  ).toBeUndefined();
  expect(
    parseLinuxMemoryInfo(
      "MemTotal: 100 kB\nMemTotal: 100 kB\nMemAvailable: 50 kB\n",
    ),
  ).toBeUndefined();
  expect(
    parseLinuxMemoryInfo("MemTotal: 100 kB\nMemAvailable: 101 kB\n"),
  ).toBeUndefined();
});

test("reports an unavailable system and accelerator without throwing", async () => {
  const accelerator: LinuxAccelerator = {
    id: "nvidia:gpu-1",
    totalBytes: 8 * gibibyte,
    async readMemory() {
      return undefined;
    },
    close() {},
  };
  const provider = createLinuxHostMemoryProvider({
    readFile: async () => {
      throw new Error("fixture unavailable");
    },
    accelerators: [accelerator],
  });

  expect(provider.topology).toEqual({
    kind: "discrete",
    system: { id: "system", capacityBytes: 0 },
    accelerators: [
      { id: accelerator.id, capacityBytes: accelerator.totalBytes },
    ],
  });
  expect((await provider.snapshot()).pools).toEqual([
    { poolId: "system", availability: "unavailable", pressure: "unknown" },
    {
      poolId: accelerator.id,
      availability: "unavailable",
      pressure: "unknown",
    },
  ]);
  await provider.close();
});

test("preserves distinct accelerator identities and snapshots", async () => {
  const accelerators: LinuxAccelerator[] = [
    {
      id: "nvidia:uuid-a",
      totalBytes: 16 * gibibyte,
      async readMemory() {
        return { totalBytes: 16 * gibibyte, availableBytes: 12 * gibibyte };
      },
      close() {},
    },
    {
      id: "nvidia:uuid-b",
      totalBytes: 24 * gibibyte,
      async readMemory() {
        return { totalBytes: 24 * gibibyte, availableBytes: 20 * gibibyte };
      },
      close() {},
    },
  ];
  const provider = createLinuxHostMemoryProvider({
    readFile: async () => "MemTotal: 32768 kB\nMemAvailable: 16384 kB\n",
    accelerators,
  });

  expect(provider.topology.kind).toBe("discrete");
  if (provider.topology.kind === "discrete") {
    expect(provider.topology.accelerators.map(({ id }) => id)).toEqual([
      "nvidia:uuid-a",
      "nvidia:uuid-b",
    ]);
  }
  expect((await provider.snapshot()).pools).toEqual([
    {
      poolId: "system",
      availability: "available",
      availableBytes: 16 * 1024 * 1024,
      pressure: "normal",
    },
    {
      poolId: "nvidia:uuid-a",
      availability: "available",
      availableBytes: 12 * gibibyte,
      pressure: "normal",
    },
    {
      poolId: "nvidia:uuid-b",
      availability: "available",
      availableBytes: 20 * gibibyte,
      pressure: "normal",
    },
  ]);
});

test("updates unknown topology capacities from the first valid sample", async () => {
  const provider = createLinuxHostMemoryProvider({
    readFile: async () => "MemTotal: 32768 kB\nMemAvailable: 16384 kB\n",
    accelerators: [
      {
        id: "nvidia:uuid",
        totalBytes: 0,
        async readMemory() {
          return {
            totalBytes: 12 * gibibyte,
            availableBytes: 10 * gibibyte,
          };
        },
        close() {},
      },
    ],
  });

  expect(provider.topology.system.capacityBytes).toBe(0);
  if (provider.topology.kind === "discrete") {
    expect(provider.topology.accelerators[0]?.capacityBytes).toBe(0);
  }
  await provider.snapshot();
  expect(provider.topology.system.capacityBytes).toBe(32 * 1024 * 1024);
  if (provider.topology.kind === "discrete") {
    expect(provider.topology.accelerators[0]?.capacityBytes).toBe(
      12 * gibibyte,
    );
  }
});

test("subtracts AMD VRAM usage without producing negative availability", () => {
  expect(calculateAmdAvailableBytes(10, 4)).toBe(6);
  expect(calculateAmdAvailableBytes(4, 10)).toBe(0);
  expect(calculateAmdAvailableBytes(-1, 0)).toBeUndefined();
});
