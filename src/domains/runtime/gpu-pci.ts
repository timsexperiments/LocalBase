import type { MemoryTopology } from "./memory-safety";

export function linuxNvidiaPci(input: {
  platform: NodeJS.Platform;
  topology: MemoryTopology;
  runtimeName: string;
}): string | undefined {
  if (input.platform !== "linux") return undefined;
  const accelerator =
    input.topology.kind === "discrete" &&
    input.topology.accelerators.length === 1
      ? input.topology.accelerators[0]
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
      `Linux ${input.runtimeName} requires one monitored NVIDIA GPU with a verified PCI identity. CPU-only fallback and ambiguous GPU selection are unsupported.`,
    );
  }
  return accelerator.pciBusId;
}
