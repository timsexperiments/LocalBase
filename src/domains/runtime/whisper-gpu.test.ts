import { expect, test } from "bun:test";
import { MemorySafetyController } from "./memory-controller";
import { createLinuxHostMemoryProvider } from "./memory/linux-memory-provider";
import type { MemoryTopology } from "./memory-safety";
import { whisperGpuArgs } from "./whisper-gpu";

const id = "nvidia:GPU-12345678-1234-1234-1234-123456789abc";
const system = { id: "system", capacityBytes: 32 * 1024 ** 3 };
const accelerator = {
  id,
  capacityBytes: 12 * 1024 ** 3,
  pciBusId: "0000:ab:1f.7",
};

test("Whisper PCI identity belongs to the exact memory pool admitted by the controller", async () => {
  const provider = createLinuxHostMemoryProvider({
    readFile: async () => "MemTotal: 33554432 kB\nMemAvailable: 16777216 kB\n",
    accelerators: [
      {
        id,
        pciBusId: accelerator.pciBusId,
        totalBytes: accelerator.capacityBytes,
        readMemory: async () => ({
          totalBytes: accelerator.capacityBytes,
          availableBytes: 8 * 1024 ** 3,
        }),
        close() {},
      },
    ],
  });
  const controller = new MemorySafetyController(provider, {
    systemReserve: { minimumGb: 0, percent: 0 },
    acceleratorReserve: { minimumGb: 0, percent: 0 },
  });
  const reservation = await controller.reserve({
    runtimeId: "stt:test",
    demand: {
      hostBytes: 1,
      acceleratorBytes: 1,
      unifiedBytes: 1,
      confidence: "estimated",
    },
  });
  expect(controller.topology).toBe(provider.topology);
  expect(whisperGpuArgs("linux", controller.topology)).toEqual([
    "--require-gpu-pci",
    accelerator.pciBusId,
  ]);
  expect((await provider.snapshot()).pools[1]?.poolId).toBe(id);
  reservation.release();
  await provider.close();
});

test("refuses missing, non-NVIDIA, noncanonical, and ambiguous identities", () => {
  const topologies: MemoryTopology[] = [
    { kind: "unified", system },
    ...[
      [],
      [
        accelerator,
        {
          ...accelerator,
          id: "nvidia:GPU-22345678-1234-1234-1234-123456789abc",
        },
      ],
      [{ id, capacityBytes: 1 }],
      [{ ...accelerator, id: "nvidia:index-0" }],
      [{ ...accelerator, id: "amd:card0" }],
      [{ ...accelerator, pciBusId: "00000000:AB:1F.7" }],
      [{ ...accelerator, pciBusId: "0000:ab:20.0" }],
      [{ ...accelerator, pciBusId: "0000:ab:1f.8" }],
    ].map((accelerators): MemoryTopology => ({
      kind: "discrete",
      system,
      accelerators,
    })),
  ];
  for (const topology of topologies) {
    expect(() => whisperGpuArgs("linux", topology)).toThrow(
      "one monitored NVIDIA GPU",
    );
    expect(whisperGpuArgs("darwin", topology)).toEqual([]);
  }
});
