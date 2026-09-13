import type { MemoryTopology } from "./memory-safety";

export function whisperGpuArgs(
  platform: NodeJS.Platform,
  topology: MemoryTopology,
): string[] {
  if (platform !== "linux") return [];
  const accelerator =
    topology.kind === "discrete" && topology.accelerators.length === 1
      ? topology.accelerators[0]
      : undefined;
  if (
    !accelerator ||
    !/^nvidia:GPU-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(
      accelerator.id,
    ) ||
    !accelerator.pciBusId ||
    !/^[0-9a-f]{4}:[0-9a-f]{2}:[01][0-9a-f]\.[0-7]$/.test(accelerator.pciBusId)
  ) {
    throw new Error(
      "Linux Whisper requires one monitored NVIDIA GPU with a verified PCI identity. CPU-only fallback and ambiguous GPU selection are unsupported.",
    );
  }
  return ["--require-gpu-pci", accelerator.pciBusId];
}

export async function requireWhisperGpuContract(binary: string): Promise<void> {
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
        stdout.trim() === "localbase-whisper-gpu-pci-v1"
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
    "Unsupported Linux Whisper runtime: --require-gpu-pci is required. Install the LocalBase PCI-capable Whisper release or replace the user-managed whisper-server. CPU-only fallback is disabled.",
  );
}
