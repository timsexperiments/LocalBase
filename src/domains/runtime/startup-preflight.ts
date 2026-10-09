import { existsSync, statSync, statfsSync } from "node:fs";
import { dirname, join } from "node:path";
import type { ModelSpec } from "../../catalog";
import type { LocalBaseConfig } from "../../manager";
import { backendBindHost } from "./launch-plan";

export class ModelInstallConsentError extends Error {
  constructor(modelId: string) {
    super(
      `Model "${modelId}" is not installed. Install it with "local-base models install ${modelId}" or restart serve with --install-missing.`,
    );
    this.name = "ModelInstallConsentError";
  }
}

export function assertModelInstallConsent(
  modelId: string,
  installMissing: boolean,
): void {
  if (installMissing) return;
  throw new ModelInstallConsentError(modelId);
}

export function assertModelDiskSpace(
  config: LocalBaseConfig,
  model: ModelSpec,
  availableBytes?: number,
): void {
  const directory = {
    llm: config.llmModelsDir,
    stt: config.sttModelsDir,
    tts: config.ttsModelsDir,
    image: config.imageModelsDir,
    video: config.videoModelsDir,
  }[model.kind];
  let destination = directory;
  while (!existsSync(destination)) destination = dirname(destination);
  const stats =
    availableBytes === undefined ? statfsSync(destination) : undefined;
  const available = availableBytes ?? stats!.bavail * stats!.bsize;
  const requiredBytes = model.artifacts.reduce((total, artifact) => {
    try {
      if (
        statSync(join(directory, artifact.filename)).size ===
        artifact.expectedSizeBytes
      )
        return total;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    const partial = join(directory, `${artifact.filename}.partial`);
    let remaining = artifact.expectedSizeBytes ?? 0;
    try {
      remaining = Math.max(0, remaining - statSync(partial).size);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    // The installer renames a truncated final artifact to .partial and resumes it.
    try {
      const finalSize = statSync(join(directory, artifact.filename)).size;
      if (finalSize < (artifact.expectedSizeBytes ?? 0))
        remaining = Math.min(
          remaining,
          (artifact.expectedSizeBytes ?? 0) - finalSize,
        );
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    return total + remaining;
  }, 0);
  if (available < requiredBytes) {
    throw new Error(
      `Insufficient disk space to install "${model.modelId}": requires ${(requiredBytes / 1_073_741_824).toFixed(2)} GiB, ${(available / 1_073_741_824).toFixed(2)} GiB available at ${directory}.`,
    );
  }
}

export async function installMissingModel(
  config: LocalBaseConfig,
  model: ModelSpec | undefined,
  modelId: string,
  consent: boolean,
  install: () => Promise<string>,
  availableBytes?: number,
): Promise<string> {
  assertModelInstallConsent(modelId, consent);
  if (model) assertModelDiskSpace(config, model, availableBytes);
  return await install();
}

export function assertPortAvailable(host: string, port: number): void {
  let probe: ReturnType<typeof Bun.serve>;
  try {
    probe = Bun.serve({ hostname: host, port, fetch: () => new Response() });
  } catch (error) {
    if (
      error instanceof Error &&
      "code" in error &&
      error.code === "EADDRINUSE"
    ) {
      throw new Error(
        `Port ${port} is already in use on ${host}. Choose another port with --port (gateway) or the matching --llm-port, --stt-port, --image-port, or --video-port option.`,
      );
    }
    throw error;
  }
  probe.stop(true);
}

export async function assertServePortsAvailable(
  config: LocalBaseConfig,
  input: {
    host?: string;
    port?: number;
    llmHost?: string;
    llmPort?: number;
    sttHost?: string;
    sttPort?: number;
    imageHost?: string;
    imagePort?: number;
    videoHost?: string;
    videoPort?: number;
    llm?: boolean;
    stt?: boolean;
    image?: boolean;
    video?: boolean;
    backendPortPreflight?: boolean;
  },
): Promise<void> {
  const bindings = [
    {
      name: "gateway",
      host: input.host ?? config.gatewayHost,
      port: input.port ?? config.gatewayPort,
    },
  ];
  if (input.backendPortPreflight !== false && input.llm !== false)
    bindings.push({
      name: "LLM",
      host: backendBindHost(input.llmHost ?? config.host),
      port: input.llmPort ?? config.port,
    });
  if (input.backendPortPreflight !== false && input.stt !== false)
    bindings.push({
      name: "STT",
      host: backendBindHost(input.sttHost ?? config.sttHost),
      port: input.sttPort ?? config.sttPort,
    });
  if (input.backendPortPreflight !== false && input.image !== false)
    bindings.push({
      name: "image",
      host: backendBindHost(input.imageHost ?? "127.0.0.1"),
      port: input.imagePort ?? 8090,
    });
  if (input.backendPortPreflight !== false && input.video !== false)
    bindings.push({
      name: "video",
      host: backendBindHost(input.videoHost ?? "127.0.0.1"),
      port: input.videoPort ?? 8091,
    });
  const probes: ReturnType<typeof Bun.serve>[] = [];
  try {
    for (const binding of bindings) {
      let probe: ReturnType<typeof Bun.serve>;
      try {
        probe = Bun.serve({
          hostname: binding.host,
          port: binding.port,
          fetch: () => new Response(),
        });
      } catch (error) {
        if (
          error instanceof Error &&
          "code" in error &&
          error.code === "EADDRINUSE"
        )
          throw new Error(
            `Port ${binding.port} is already in use on ${binding.host}. Choose another port with --port (gateway) or the matching --llm-port, --stt-port, --image-port, or --video-port option.`,
          );
        throw error;
      }
      probes.push(probe);
    }
  } finally {
    for (const probe of probes) probe.stop(true);
  }
}
