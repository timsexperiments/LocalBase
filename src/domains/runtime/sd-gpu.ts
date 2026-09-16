import { linuxNvidiaPci } from "./gpu-pci";
import type { MemoryTopology } from "./memory-safety";

export function sdGpuArgs(
  platform: NodeJS.Platform,
  topology: MemoryTopology,
): string[] {
  const pciBusId = linuxNvidiaPci({
    platform,
    topology,
    runtimeName: "sd-server",
  });
  return pciBusId ? ["--require-gpu-pci", pciBusId] : [];
}

export async function requireSdGpuContract(binary: string): Promise<void> {
  try {
    const child = Bun.spawn([binary, "--localbase-capabilities"], {
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
      timeout: 5_000,
      maxBuffer: 64 * 1024,
      killSignal: "SIGKILL",
    });
    try {
      const [code, stdout] = await Promise.all([
        child.exited,
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
      ]);
      if (
        code === 0 &&
        child.signalCode === null &&
        stdout.trim() === "localbase-sd-gpu-pci-v1"
      )
        return;
    } finally {
      if (child.exitCode === null && child.signalCode === null)
        child.kill("SIGKILL");
      await child.exited;
    }
  } catch {
    // Report one actionable error for old, broken, or unresponsive runtimes.
  }
  throw new Error(
    "Unsupported Linux sd-server runtime: --require-gpu-pci is required. Install the LocalBase PCI-capable sd-server release or replace the user-managed sd-server. CPU-only fallback is disabled.",
  );
}
