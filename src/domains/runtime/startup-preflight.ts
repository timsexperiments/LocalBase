import { statfsSync } from "node:fs";
import type { ModelSpec } from "../../catalog";
import type { LocalBaseConfig } from "../../manager";

export function assertModelInstallConsent(
  modelId: string,
  installMissing: boolean,
): void {
  if (installMissing) return;
  throw new Error(
    `Model "${modelId}" is not installed. Install it with "local-base models install ${modelId}" or rerun serve with --install-missing to allow downloads.`,
  );
}

export function assertModelDiskSpace(
  config: LocalBaseConfig,
  model: ModelSpec,
  availableBytes = (() => {
    const stats = statfsSync(config.root);
    return stats.bavail * stats.bsize;
  })(),
): void {
  const directory = {
    llm: config.llmModelsDir,
    stt: config.sttModelsDir,
    tts: config.ttsModelsDir,
    image: config.imageModelsDir,
    video: config.videoModelsDir,
  }[model.kind];
  const requiredBytes = model.artifacts.reduce(
    (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
    0,
  );
  if (availableBytes < requiredBytes) {
    throw new Error(
      `Insufficient disk space to install "${model.modelId}": requires ${(requiredBytes / 1_073_741_824).toFixed(2)} GiB, ${(availableBytes / 1_073_741_824).toFixed(2)} GiB available at ${directory}.`,
    );
  }
}

export async function installMissingModel(
  config: LocalBaseConfig,
  model: ModelSpec | undefined,
  modelId: string,
  consent: boolean,
  install: () => Promise<string>,
): Promise<string> {
  assertModelInstallConsent(modelId, consent);
  if (model) assertModelDiskSpace(config, model);
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

export function assertServePortsAvailable(
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
  },
): void {
  assertPortAvailable(
    input.host ?? config.gatewayHost,
    input.port ?? config.gatewayPort,
  );
  if (input.llm !== false)
    assertPortAvailable(
      input.llmHost ?? config.host,
      input.llmPort ?? config.port,
    );
  if (input.stt !== false)
    assertPortAvailable(
      input.sttHost ?? config.sttHost,
      input.sttPort ?? config.sttPort,
    );
  if (input.image !== false)
    assertPortAvailable(
      input.imageHost ?? "127.0.0.1",
      input.imagePort ?? 8090,
    );
  if (input.video !== false)
    assertPortAvailable(
      input.videoHost ?? "127.0.0.1",
      input.videoPort ?? 8091,
    );
}
