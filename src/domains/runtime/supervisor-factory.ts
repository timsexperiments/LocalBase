import { basename, join } from "node:path";
import {
  byId,
  calculateMaxSafeContextSize,
  primaryArtifact,
  resolveCatalogInstallation,
} from "../../catalog";
import type { AppContext } from "../../context";
import {
  ensureBinary,
  installModel,
  type LocalBaseConfig,
} from "../../manager";
import type { ServeInput } from "../app/commands/inputs";
import type { RuntimeConfigSnapshot } from "./config-snapshot";
import {
  resolveImageLaunchPlan,
  resolveLlmLaunchPlan,
  resolveSttLaunchPlan,
} from "./launch-plan";
import {
  startLlamaServerProcess,
  startSdServerProcess,
  startWhisperServerProcess,
} from "./launcher";
import type { RuntimeModality } from "./modality";
import type { MemorySafetyController } from "./memory-controller";
import { SpeechSupervisor, type SpeechPreparation } from "./speech-supervisor";
import { ManagedService } from "./supervisor";
import type { RuntimeSupervisor } from "./supervisor-registry";

type ServerRuntimeModality = Exclude<RuntimeModality, "tts">;

export type RuntimeLaunchOverrides = Readonly<{
  llmHost?: string;
  llmPort?: number;
  sttHost?: string;
  sttPort?: number;
  imageHost?: string;
  imagePort?: number;
  ctxSize?: number;
  llmModelFile?: string;
  sttModelFile?: string;
  imageModelFile?: string;
  ttsModelFile?: string;
}>;

export type RuntimeSupervisorFactory = Readonly<{
  create: (
    modality: RuntimeModality,
    snapshot: RuntimeConfigSnapshot,
  ) => RuntimeSupervisor;
  baseUrl: (
    modality: ServerRuntimeModality,
    snapshot: RuntimeConfigSnapshot,
  ) => string;
}>;

export type RuntimeSupervisorFactoryDependencies = Readonly<{
  memorySafety: MemorySafetyController;
}>;

function endpoint(host: string, port: number): string {
  const urlHost =
    host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  return `http://${urlHost}:${port}`;
}

function llmHost(
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeLaunchOverrides,
): string {
  return overrides.llmHost ?? config.host;
}

function llmPort(
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeLaunchOverrides,
): number {
  return overrides.llmPort ?? config.port;
}

function sttHost(
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeLaunchOverrides,
): string {
  return overrides.sttHost ?? config.sttHost;
}

function sttPort(
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeLaunchOverrides,
): number {
  return overrides.sttPort ?? config.sttPort;
}

function imageHost(overrides: RuntimeLaunchOverrides): string {
  return overrides.imageHost ?? "127.0.0.1";
}

function imagePort(overrides: RuntimeLaunchOverrides): number {
  return overrides.imagePort ?? 8090;
}

function component(
  modality: RuntimeModality,
): "llama-server" | "whisper-server" | "llama-tts" | "sd-server" {
  if (modality === "llm") return "llama-server";
  if (modality === "stt") return "whisper-server";
  if (modality === "tts") return "llama-tts";
  return "sd-server";
}

function activeModel(
  modality: RuntimeModality,
  config: RuntimeConfigSnapshot["config"],
): string {
  if (modality === "llm") return config.activeLlmModel;
  if (modality === "stt") return config.activeSttModel;
  if (modality === "tts") return config.activeTtsModel;
  return config.activeImageModel;
}

function configuredModelFile(
  config: LocalBaseConfig,
  modelId: string,
  modality: RuntimeModality,
): Promise<string> {
  const directory =
    modality === "stt"
      ? config.sttModelsDir
      : modality === "tts"
        ? config.ttsModelsDir
        : config.imageModelsDir;
  const spec = byId(modelId);
  const filename = spec ? primaryArtifact(spec).filename : undefined;
  if (filename) {
    return Bun.file(join(directory, filename))
      .exists()
      .then((exists) => (exists ? filename : ""));
  }
  const fallback =
    modality === "stt" || modality === "tts"
      ? `${modelId}.gguf`
      : `${modelId}.safetensors`;
  return Bun.file(join(directory, fallback))
    .exists()
    .then((exists) => (exists ? fallback : ""));
}

async function artifactBytes(
  modelId: string,
  directory: string,
  modelFile: string,
): Promise<number> {
  const spec = byId(modelId);
  const expectedSizeBytes = spec
    ? primaryArtifact(spec).expectedSizeBytes
    : undefined;
  if (expectedSizeBytes !== undefined) return expectedSizeBytes;
  return (await Bun.file(join(directory, modelFile)).stat()).size;
}

function createLoggerEvent(
  ctx: AppContext,
  modality: RuntimeModality,
  eventName: "model.installing" | "model.installed" | "model.install-failed",
  modelId: string,
  reason?: "incomplete" | "missing",
  error?: unknown,
): void {
  ctx.logger.event({
    severity: eventName === "model.install-failed" ? "error" : "info",
    eventName,
    category: "runtime",
    component: component(modality),
    runtime: modality,
    message:
      eventName === "model.installing"
        ? "Installing a selected model."
        : eventName === "model.installed"
          ? "Selected model installation completed."
          : "Selected model installation failed.",
    ...(error
      ? {
          error: {
            type: error instanceof Error ? error.name : "Error",
            message: error instanceof Error ? error.message : String(error),
          },
        }
      : {}),
    attributes: { model_id: modelId, ...(reason ? { reason } : {}) },
  });
}

async function installSelectedModel(
  ctx: AppContext,
  config: LocalBaseConfig,
  modality: RuntimeModality,
  modelId: string,
  reason: "incomplete" | "missing",
): Promise<string> {
  createLoggerEvent(ctx, modality, "model.installing", modelId, reason);
  try {
    const installed = await installModel(config, modelId);
    createLoggerEvent(ctx, modality, "model.installed", modelId);
    return installed;
  } catch (error) {
    createLoggerEvent(
      ctx,
      modality,
      "model.install-failed",
      modelId,
      reason,
      error,
    );
    throw error;
  }
}

export function runtimeLaunchOverrides(
  input: ServeInput,
): RuntimeLaunchOverrides {
  return Object.freeze({
    ...(input.llmHost ? { llmHost: input.llmHost } : {}),
    ...(input.llmPort ? { llmPort: input.llmPort } : {}),
    ...(input.sttHost ? { sttHost: input.sttHost } : {}),
    ...(input.sttPort ? { sttPort: input.sttPort } : {}),
    ...(input.imageHost ? { imageHost: input.imageHost } : {}),
    ...(input.imagePort ? { imagePort: input.imagePort } : {}),
    ...(input.ctxSize ? { ctxSize: input.ctxSize } : {}),
    ...(input.llmModelFile ? { llmModelFile: input.llmModelFile } : {}),
    ...(input.sttModelFile ? { sttModelFile: input.sttModelFile } : {}),
    ...(input.imageModelFile ? { imageModelFile: input.imageModelFile } : {}),
    ...(input.ttsModelFile ? { ttsModelFile: input.ttsModelFile } : {}),
  });
}

export function createRuntimeSupervisorFactory(
  ctx: AppContext,
  overrides: RuntimeLaunchOverrides,
  dependencies: RuntimeSupervisorFactoryDependencies,
): RuntimeSupervisorFactory {
  let nextRuntimeGeneration = 0;
  const baseUrl = (
    modality: ServerRuntimeModality,
    snapshot: RuntimeConfigSnapshot,
  ): string => {
    if (modality === "llm") {
      return endpoint(
        llmHost(snapshot.config, overrides),
        llmPort(snapshot.config, overrides),
      );
    }
    if (modality === "stt") {
      return endpoint(
        sttHost(snapshot.config, overrides),
        sttPort(snapshot.config, overrides),
      );
    }
    return endpoint(imageHost(overrides), imagePort(overrides));
  };

  const create = (
    modality: RuntimeModality,
    snapshot: RuntimeConfigSnapshot,
  ): RuntimeSupervisor => {
    const config = structuredClone(snapshot.config) as LocalBaseConfig;
    const modelId = activeModel(modality, snapshot.config);
    const runtimeId = `${modality}:${modelId}:${++nextRuntimeGeneration}`;

    if (modality === "llm") {
      const base = baseUrl(modality, snapshot);
      return new ManagedService({
        runtimeId,
        modality,
        component: "llama-server",
        healthUrl: `${base}/health`,
        logger: ctx.logger,
        launch: async () => {
          let modelFile = overrides.llmModelFile;
          if (!modelFile) {
            const spec = byId(modelId);
            if (spec) {
              const installation = await resolveCatalogInstallation(
                spec,
                config.llmModelsDir,
              );
              modelFile = installation.complete
                ? primaryArtifact(spec).filename
                : basename(
                    await installSelectedModel(
                      ctx,
                      config,
                      modality,
                      modelId,
                      "incomplete",
                    ),
                  );
            } else {
              const bin = `${modelId}.bin`;
              const fallback = (await Bun.file(
                join(config.llmModelsDir, bin),
              ).exists())
                ? bin
                : `${modelId}.gguf`;
              modelFile = (await Bun.file(
                join(config.llmModelsDir, fallback),
              ).exists())
                ? fallback
                : basename(
                    await installSelectedModel(
                      ctx,
                      config,
                      modality,
                      modelId,
                      "missing",
                    ),
                  );
            }
          }
          const spec = byId(modelId);
          const recommended = spec
            ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
            : ctx.specs.gpuVramGb >= 32
              ? 32768
              : 8192;
          const ctxSize =
            overrides.ctxSize ?? Math.min(recommended, config.ctxSize);
          ctx.logger.info(
            "llama-server",
            `Spawning model "${modelId}" (file: ${modelFile}, context: ${ctxSize} tokens)`,
          );
          return resolveLlmLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.llmModelsDir,
            modelId,
            modelFile,
            host: llmHost(snapshot.config, overrides),
            port: llmPort(snapshot.config, overrides),
            ctxSize,
            parallel: config.parallel,
            modelRequirementGb: spec?.minVramGb,
            artifactBytes: await artifactBytes(
              modelId,
              config.llmModelsDir,
              modelFile,
            ),
            hardware: { memoryGb: ctx.specs.gpuVramGb },
          });
        },
        start: async (plan) => {
          if (plan.component !== "llama-server") {
            throw new Error("Expected llama-server launch plan.");
          }
          return await startLlamaServerProcess(plan);
        },
        memorySafety: dependencies.memorySafety,
        otel: ctx.otel,
        startupTimeoutMs:
          byId(modelId)?.minVramGb && byId(modelId)!.minVramGb >= 16
            ? 180000
            : 60000,
      });
    }

    if (modality === "stt") {
      const base = baseUrl(modality, snapshot);
      return new ManagedService({
        runtimeId,
        modality,
        component: "whisper-server",
        healthUrl: `${base}/health`,
        logger: ctx.logger,
        launch: async () => {
          let modelFile = overrides.sttModelFile;
          if (!modelFile) {
            modelFile = await configuredModelFile(config, modelId, modality);
            if (!modelFile) {
              modelFile = basename(
                await installSelectedModel(
                  ctx,
                  config,
                  modality,
                  modelId,
                  "missing",
                ),
              );
            }
          }
          ctx.logger.info(
            "whisper-server",
            `Spawning STT model "${modelId}" (file: ${modelFile})`,
          );
          const spec = byId(modelId);
          return resolveSttLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.sttModelsDir,
            modelId,
            modelFile,
            host: sttHost(snapshot.config, overrides),
            port: sttPort(snapshot.config, overrides),
            modelRequirementGb: spec?.minVramGb,
            artifactBytes: await artifactBytes(
              modelId,
              config.sttModelsDir,
              modelFile,
            ),
          });
        },
        start: async (plan) => {
          if (plan.component !== "whisper-server") {
            throw new Error("Expected whisper-server launch plan.");
          }
          return await startWhisperServerProcess(plan);
        },
        memorySafety: dependencies.memorySafety,
        otel: ctx.otel,
        startupTimeoutMs: 30000,
      });
    }

    if (modality === "tts") {
      return new SpeechSupervisor({
        runtimeId,
        root: config.root,
        modelId,
        logger: ctx.logger,
        memorySafety: dependencies.memorySafety,
        prepare: async (): Promise<SpeechPreparation> => {
          const spec = byId(modelId);
          if (!spec || spec.kind !== "tts") {
            throw new Error(`Unknown TTS model "${modelId}".`);
          }

          let installation = await resolveCatalogInstallation(
            spec,
            config.ttsModelsDir,
          );
          if (!installation.complete) {
            await installSelectedModel(
              ctx,
              config,
              modality,
              modelId,
              "incomplete",
            );
            installation = await resolveCatalogInstallation(
              spec,
              config.ttsModelsDir,
            );
          }
          if (!installation.complete) {
            throw new Error(
              `TTS model "${modelId}" is incomplete after installation.`,
            );
          }
          const projector = spec.artifacts.find(
            ({ role }) => role === "supplementary",
          );
          if (!projector) {
            throw new Error(
              `TTS model "${modelId}" has no projector artifact.`,
            );
          }
          const modelPath = overrides.ttsModelFile
            ? join(config.ttsModelsDir, overrides.ttsModelFile)
            : installation.primaryPath;
          if (!(await Bun.file(modelPath).exists())) {
            throw new Error(`Configured TTS model file does not exist.`);
          }
          return Object.freeze({
            binaryPath: await ensureBinary(config, "llama-tts"),
            modelPath,
            projectorPath: join(config.ttsModelsDir, projector.filename),
          });
        },
      });
    }

    const base = baseUrl(modality, snapshot);
    return new ManagedService({
      runtimeId,
      modality,
      component: "sd-server",
      healthUrl: `${base}/`,
      logger: ctx.logger,
      launch: async () => {
        let modelFile = overrides.imageModelFile;
        if (!modelFile) {
          modelFile = await configuredModelFile(config, modelId, modality);
          if (!modelFile) {
            modelFile = basename(
              await installSelectedModel(
                ctx,
                config,
                modality,
                modelId,
                "missing",
              ),
            );
          }
        }
        ctx.logger.info(
          "sd-server",
          `Spawning image model "${modelId}" (file: ${modelFile})`,
        );
        const spec = byId(modelId);
        return resolveImageLaunchPlan({
          runtimeId,
          root: config.root,
          modelsDirectory: config.imageModelsDir,
          modelId,
          modelFile,
          host: imageHost(overrides),
          port: imagePort(overrides),
          modelRequirementGb: spec?.minVramGb,
          artifactBytes: await artifactBytes(
            modelId,
            config.imageModelsDir,
            modelFile,
          ),
        });
      },
      start: async (plan) => {
        if (plan.component !== "sd-server") {
          throw new Error("Expected sd-server launch plan.");
        }
        return await startSdServerProcess(plan);
      },
      memorySafety: dependencies.memorySafety,
      otel: ctx.otel,
      startupTimeoutMs: 30000,
    });
  };

  return Object.freeze({ create, baseUrl });
}
