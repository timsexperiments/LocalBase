import { expect, test } from "bun:test";
import {
  calculateAmdAvailableBytes,
  createLinuxHostMemoryProvider,
  parseLinuxMemoryInfo,
  parseNvmlPciInfo,
  type LinuxAccelerator,
} from "./linux-memory-provider";

const gibibyte = 1024 ** 3;

function pciInfo(
  busId: string,
  domain: number,
  bus: number,
  device: number,
): Uint8Array {
  const info = new Uint8Array(68);
  const fields = new DataView(info.buffer);
  fields.setUint32(16, domain, true);
  fields.setUint32(20, bus, true);
  fields.setUint32(24, device, true);
  fields.setUint32(28, 0x278010de, true);
  info.set(new TextEncoder().encode(busId), 36);
  return info;
}

test("normalizes the NVML PCI struct to exact GGML identity without losing domain bits", () => {
  expect(parseNvmlPciInfo(pciInfo("00000000:AB:1F.7", 0, 0xab, 0x1f))).toBe(
    "0000:ab:1f.7",
  );
  expect(parseNvmlPciInfo(pciInfo("0000ABCD:EF:01.0", 0xabcd, 0xef, 1))).toBe(
    "abcd:ef:01.0",
  );
  expect(parseNvmlPciInfo(pciInfo("ffff:ff:00.0", 0xffff, 0xff, 0))).toBe(
    "ffff:ff:00.0",
  );
  for (const value of [
    "00010000:AB:1F.7",
    "000000:AB:1F.7",
    "0000:AB:20.0",
    "0000:AB:1F.8",
    " 0000:AB:1F.7",
    "0000:AB:1F.7 ",
    "0000:AB:1G.7",
    "",
  ]) {
    expect(parseNvmlPciInfo(pciInfo(value, 0, 0xab, 0x1f))).toBeUndefined();
  }
  for (const fields of [
    [1, 0xab, 0x1f],
    [0, 0xac, 0x1f],
    [0, 0xab, 0x1e],
  ]) {
    expect(
      parseNvmlPciInfo(
        pciInfo("00000000:AB:1F.7", fields[0]!, fields[1]!, fields[2]!),
      ),
    ).toBeUndefined();
  }
  const wrongVendor = pciInfo("00000000:AB:1F.7", 0, 0xab, 0x1f);
  new DataView(wrongVendor.buffer).setUint32(28, 0x8086, true);
  expect(parseNvmlPciInfo(wrongVendor)).toBeUndefined();
  expect(parseNvmlPciInfo(new Uint8Array(67))).toBeUndefined();
  expect(parseNvmlPciInfo(new Uint8Array(68).fill(65))).toBeUndefined();
});

test("retains a measured pool without PCI identity and preserves verified identity on that same pool", async () => {
  const provider = createLinuxHostMemoryProvider({
    readFile: async () => "MemTotal: 32768 kB\nMemAvailable: 16384 kB\n",
    accelerators: [
      {
        id: "nvidia:uuid-a",
        pciBusId: "0000:ab:1f.7",
        totalBytes: 0,
        readMemory: async () => ({
          totalBytes: 12 * gibibyte,
          availableBytes: 10 * gibibyte,
        }),
        close() {},
      },
      {
        id: "nvidia:uuid-b",
        totalBytes: 0,
        readMemory: async () => ({
          totalBytes: 24 * gibibyte,
          availableBytes: 20 * gibibyte,
        }),
        close() {},
      },
    ],
  });
  const snapshot = await provider.snapshot();
  if (provider.topology.kind !== "discrete")
    throw new Error("Expected discrete topology");
  expect(provider.topology.accelerators).toEqual([
    {
      id: "nvidia:uuid-a",
      pciBusId: "0000:ab:1f.7",
      capacityBytes: 12 * gibibyte,
    },
    { id: "nvidia:uuid-b", capacityBytes: 24 * gibibyte },
  ]);
  expect(snapshot.pools[1]).toMatchObject({
    poolId: "nvidia:uuid-a",
    availableBytes: 10 * gibibyte,
  });
  expect(snapshot.pools[2]).toMatchObject({
    poolId: "nvidia:uuid-b",
    availableBytes: 20 * gibibyte,
  });
  await provider.close();
});

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
