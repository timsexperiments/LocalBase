import { expect, test } from "bun:test";
import {
  createMacOsHostMemoryProvider,
  parseMachAvailableBytes,
  type MachMemoryReader,
} from "./macos-memory-provider";

function machFixture({
  free,
  inactive,
  speculative,
}: {
  free: number;
  inactive: number;
  speculative: number;
}): Uint8Array {
  const buffer = new Uint8Array(96);
  const view = new DataView(buffer.buffer);
  view.setUint32(0, free, true);
  view.setUint32(8, inactive, true);
  view.setUint32(92, speculative, true);
  return buffer;
}

test("parses the required Mach VM counters and page size", () => {
  expect(
    parseMachAvailableBytes(
      machFixture({ free: 2, inactive: 3, speculative: 1 }),
      4096,
    ),
  ).toBe(6 * 4096);
});

test("rejects short or invalid Mach samples", () => {
  expect(parseMachAvailableBytes(new Uint8Array(95), 4096)).toBeUndefined();
  expect(parseMachAvailableBytes(new Uint8Array(96), 0)).toBeUndefined();
});

test("uses unified topology on Apple Silicon", async () => {
  const reader: MachMemoryReader = {
    read: () => ({
      buffer: machFixture({ free: 1, inactive: 2, speculative: 0 }),
      pageSize: 4096,
    }),
    close() {},
  };
  const provider = createMacOsHostMemoryProvider({
    memoryReader: reader,
    totalMemoryBytes: 32 * 1024 ** 3,
    appleSilicon: true,
  });

  expect(provider.topology).toEqual({
    kind: "unified",
    system: { id: "system", capacityBytes: 32 * 1024 ** 3 },
  });
  expect((await provider.snapshot()).pools).toEqual([
    {
      poolId: "system",
      availability: "available",
      availableBytes: 3 * 4096,
      pressure: "normal",
    },
  ]);
});

test("represents Intel macOS as host-only discrete memory", () => {
  const provider = createMacOsHostMemoryProvider({
    memoryReader: { read: () => undefined, close() {} },
    totalMemoryBytes: 16 * 1024 ** 3,
    appleSilicon: false,
  });
  expect(provider.topology).toEqual({
    kind: "discrete",
    system: { id: "system", capacityBytes: 16 * 1024 ** 3 },
    accelerators: [],
  });
});

test("reads fresh Mach counters and fails closed instead of reusing a prior sample", async () => {
  let readCount = 0;
  const provider = createMacOsHostMemoryProvider({
    memoryReader: {
      read() {
        readCount += 1;
        return readCount <= 2
          ? {
              buffer: machFixture({
                free: 3 - readCount,
                inactive: 0,
                speculative: 0,
              }),
              pageSize: 4096,
            }
          : undefined;
      },
      close() {},
    },
    totalMemoryBytes: 16 * 1024 ** 3,
    appleSilicon: true,
  });
  for (const availableBytes of [8192, 4096]) {
    expect((await provider.snapshot()).pools).toEqual([
      {
        poolId: "system",
        availability: "available",
        availableBytes,
        pressure: "normal",
      },
    ]);
  }
  expect((await provider.snapshot()).pools).toEqual([
    { poolId: "system", availability: "unavailable", pressure: "unknown" },
  ]);
  expect(readCount).toBe(3);
});
