import { createLinuxHostMemoryProvider } from "../domains/runtime/memory/linux-memory-provider";

// Only the test build substitutes this provider. Production CLI builds have no
// identity override, including when memory-limit checks are bypassed.
export function createHostMemoryProvider() {
  return createLinuxHostMemoryProvider({
    readFile: async () => "MemTotal: 67108864 kB\nMemAvailable: 33554432 kB\n",
    accelerators: [
      {
        id: "nvidia:GPU-12345678-1234-1234-1234-123456789abc",
        pciBusId: "0000:ab:1f.7",
        totalBytes: 32 * 1024 ** 3,
        async readMemory() {
          return { totalBytes: 32 * 1024 ** 3, availableBytes: 24 * 1024 ** 3 };
        },
        close() {},
      },
    ],
  });
}
