import { basename, join } from "node:path";
import { verifyAuthoritativeFile } from "../../utils/checksum";
import {
  byId,
  primaryArtifact,
  resolveCatalogInstallation,
  ttsReferenceArtifacts,
} from "../../catalog";
import type { AppContext } from "../../context";
import {
  ensureBinary,
  installModel,
  type LocalBaseConfig,
  type ModelInstallEvent,
  type ModelInstallReporter,
} from "../../manager";
import type { ServeInput } from "../app/commands/inputs";
import type { RuntimeConfigSnapshot } from "./config-snapshot";
import {
  readLlmKvGeometry,
  readLlmTrainingContextLength,
} from "./gguf-metadata";
import {
  backendBindHost,
  configuredLlmContextSize,
  resolveImageLaunchPlan,
  resolveConfiguredLlmLaunchPlan,
  resolveSttLaunchPlan,
  resolveVideoLaunchPlan,
} from "./launch-plan";
import { assertModelInstallConsent } from "./startup-preflight";
import {
  startLlamaServerProcess,
  startSdServerProcess,
  startSdVideoServerProcess,
  startWhisperServerProcess,
} from "./launcher";
import type { RuntimeModality } from "./modality";
import type { VideoRuntimeTarget } from "../../catalog";
import { assertModelEligible } from "../models/model-eligibility";
import type { MemorySafetyController } from "./memory-controller";
import type { MemoryTopology } from "./memory-safety";
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
  videoHost?: string;
  videoPort?: number;
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
  installMissing?: boolean;
  /** Host platform used to select the video runtime target; defaults to this process. */
  host?: Readonly<{ platform: NodeJS.Platform; arch: NodeJS.Architecture }>;
}>;

export function runtimeEndpoint(host: string, port: number): string {
  const safeHost = backendBindHost(host);
  const urlHost =
    safeHost.includes(":") && !safeHost.startsWith("[")
      ? `[${safeHost}]`
      : safeHost;
  return `http://${urlHost}:${port}`;
}

function llmHost(
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeLaunchOverrides,
): string {
  return backendBindHost(overrides.llmHost ?? config.host);
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
  return backendBindHost(overrides.sttHost ?? config.sttHost);
}

function sttPort(
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeLaunchOverrides,
): number {
  return overrides.sttPort ?? config.sttPort;
}

function imageHost(overrides: RuntimeLaunchOverrides): string {
  return backendBindHost(overrides.imageHost ?? "127.0.0.1");
}

function imagePort(overrides: RuntimeLaunchOverrides): number {
  return overrides.imagePort ?? 8090;
}

function videoHost(overrides: RuntimeLaunchOverrides): string {
  return backendBindHost(overrides.videoHost ?? "127.0.0.1");
}

function videoPort(overrides: RuntimeLaunchOverrides): number {
  return overrides.videoPort ?? 8091;
}

function videoRuntimeTarget(
  topology: MemoryTopology,
  host: Readonly<{
    platform: NodeJS.Platform;
    arch: NodeJS.Architecture;
  }> = process,
): VideoRuntimeTarget {
  if (
    host.platform === "linux" &&
    host.arch === "x64" &&
    topology.kind === "discrete" &&
    topology.accelerators.length === 1 &&
    topology.accelerators[0]?.id.startsWith("nvidia:")
  ) {
    return { platform: "linux", architecture: "x64", accelerator: "nvidia" };
  }
  if (
    host.platform === "darwin" &&
    host.arch === "arm64" &&
    topology.kind === "unified"
  ) {
    return {
      platform: "darwin",
      architecture: "arm64",
      accelerator: "apple-unified",
    };
  }
  throw new Error("Video runtime target is not supported.");
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
  if (modality === "image") return config.activeImageModel;
  return config.activeVideoModel;
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

// Sharded GGUF weights list every shard after the first as a supplementary
// artifact, and llama-server keeps all of them resident.
async function llmArtifactBytes(
  modelId: string,
  directory: string,
  modelFile: string,
): Promise<number> {
  const spec = byId(modelId);
  if (spec) {
    return spec.artifacts.reduce(
      (total, { expectedSizeBytes }) => total + (expectedSizeBytes ?? 0),
      0,
    );
  }
  return await artifactBytes(modelId, directory, modelFile);
}

function createModelInstallReporter(
  ctx: Pick<AppContext, "logger">,
  modality: RuntimeModality,
  reason: "incomplete" | "missing",
): ModelInstallReporter {
  const emit = (
    event: ModelInstallEvent,
    input: Omit<
      Parameters<AppContext["logger"]["event"]>[0],
      "category" | "component" | "runtime"
    >,
  ): void => {
    ctx.logger.event({
      ...input,
      category: "runtime",
      component: component(modality),
      runtime: modality,
      attributes: {
        model_id: event.modelId,
        reason,
        ...input.attributes,
      },
    });
  };

  return (event) => {
    switch (event.kind) {
      case "started":
        emit(event, {
          severity: "info",
          eventName: "model.installing",
          message: `Installing selected model ${event.modelId} (${event.artifactCount} ${event.artifactCount === 1 ? "artifact" : "artifacts"}).`,
          attributes: { artifact_count: event.artifactCount },
        });
        return;
      case "download-progress":
        emit(event, {
          severity: "info",
          eventName: "model.install-progress",
          message: `Downloading ${event.modelId} artifact ${event.artifactIndex}/${event.artifactCount} (${event.artifactFilename}): ${event.percent}%.`,
          attributes: {
            artifact_filename: event.artifactFilename,
            artifact_index: event.artifactIndex,
            artifact_count: event.artifactCount,
            downloaded_bytes: event.downloadedBytes,
            total_bytes: event.totalBytes,
            percent: event.percent,
          },
        });
        return;
      case "verification-started":
        emit(event, {
          severity: "info",
          eventName: "model.install-verifying",
          message: `Validating ${event.modelId} artifact ${event.artifactIndex}/${event.artifactCount} (${event.artifactFilename}) against authoritative size and checksum state.`,
          attributes: {
            artifact_filename: event.artifactFilename,
            artifact_index: event.artifactIndex,
            artifact_count: event.artifactCount,
          },
        });
        return;
      case "verification-completed":
        emit(event, {
          severity: "info",
          eventName: "model.install-verified",
          message:
            event.verification === "sha256"
              ? `${event.modelId} artifact ${event.artifactIndex}/${event.artifactCount} (${event.artifactFilename}) checksum verified.`
              : `${event.modelId} artifact ${event.artifactIndex}/${event.artifactCount} (${event.artifactFilename}) matched cached authoritative verification.`,
          attributes: {
            artifact_filename: event.artifactFilename,
            artifact_index: event.artifactIndex,
            artifact_count: event.artifactCount,
            verification_method: event.verification,
          },
        });
        return;
      case "completed":
        emit(event, {
          severity: "info",
          eventName: "model.installed",
          message: `Selected model ${event.modelId} installation completed.`,
        });
        return;
      case "failed":
        emit(event, {
          severity: "error",
          eventName: "model.install-failed",
          message: `Selected model ${event.modelId} installation failed during ${event.phase}.`,
          error: {
            type: "ModelInstallError",
            message: "Selected model installation failed.",
          },
          attributes: { phase: event.phase },
        });
        return;
    }
    event satisfies never;
  };
}

export async function installSelectedModel(
  ctx: Pick<AppContext, "logger">,
  config: LocalBaseConfig,
  modality: RuntimeModality,
  modelId: string,
  reason: "incomplete" | "missing",
  installMissing = false,
): Promise<string> {
  assertModelInstallConsent(modelId, installMissing);
  return await installModel(
    config,
    modelId,
    undefined,
    createModelInstallReporter(ctx, modality, reason),
  );
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
    ...(input.videoHost ? { videoHost: input.videoHost } : {}),
    ...(input.videoPort ? { videoPort: input.videoPort } : {}),
    ...(input.ctxSize ? { ctxSize: input.ctxSize } : {}),
    ...(input.llmModelFile ? { llmModelFile: input.llmModelFile } : {}),
    ...(input.sttModelFile ? { sttModelFile: input.sttModelFile } : {}),
    ...(input.imageModelFile ? { imageModelFile: input.imageModelFile } : {}),
    ...(input.ttsModelFile ? { ttsModelFile: input.ttsModelFile } : {}),
  });
}

export function createRuntimeSupervisorFactory(
  ctx: Pick<AppContext, "logger" | "otel" | "specs">,
  overrides: RuntimeLaunchOverrides,
  dependencies: RuntimeSupervisorFactoryDependencies,
): RuntimeSupervisorFactory {
  let nextRuntimeGeneration = 0;
  const baseUrl = (
    modality: ServerRuntimeModality,
    snapshot: RuntimeConfigSnapshot,
  ): string => {
    if (modality === "llm") {
      return runtimeEndpoint(
        llmHost(snapshot.config, overrides),
        llmPort(snapshot.config, overrides),
      );
    }
    if (modality === "stt") {
      return runtimeEndpoint(
        sttHost(snapshot.config, overrides),
        sttPort(snapshot.config, overrides),
      );
    }
    return modality === "image"
      ? runtimeEndpoint(imageHost(overrides), imagePort(overrides))
      : runtimeEndpoint(videoHost(overrides), videoPort(overrides));
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
        llmProfile: {
          modelId,
          ...(overrides.llmModelFile
            ? { modelFile: overrides.llmModelFile }
            : {}),
          configCtxSize: config.ctxSize,
          ...(overrides.ctxSize !== undefined
            ? { ctxSizeOverride: overrides.ctxSize }
            : {}),
          parallel: config.parallel,
        },
        healthUrl: `${base}/health`,
        logger: ctx.logger,
        preflightDemand: async (signal) => {
          if (signal?.aborted) return undefined;
          let modelFile = overrides.llmModelFile;
          if (!modelFile) {
            const spec = byId(modelId);
            if (spec) {
              modelFile = primaryArtifact(spec).filename;
            } else {
              for (const candidate of [`${modelId}.bin`, `${modelId}.gguf`]) {
                if (
                  await Bun.file(join(config.llmModelsDir, candidate)).exists()
                ) {
                  modelFile = candidate;
                  break;
                }
              }
              if (!modelFile) return undefined;
            }
          }
          const modelPath = join(config.llmModelsDir, modelFile);
          const plan = resolveConfiguredLlmLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.llmModelsDir,
            modelId,
            modelFile,
            host: llmHost(snapshot.config, overrides),
            port: llmPort(snapshot.config, overrides),
            model: byId(modelId),
            configCtxSize: config.ctxSize,
            ctxSizeOverride: overrides.ctxSize,
            parallel: config.parallel,
            artifactBytes: await llmArtifactBytes(
              modelId,
              config.llmModelsDir,
              modelFile,
            ),
            memoryGb: ctx.specs.gpuVramGb,
            kvGeometry: await readLlmKvGeometry(modelPath),
            trainingContextLength:
              await readLlmTrainingContextLength(modelPath),
          });
          return signal?.aborted ? undefined : plan.memoryDemand;
        },
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
                      dependencies.installMissing,
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
                      dependencies.installMissing,
                    ),
                  );
            }
          }
          const spec = byId(modelId);
          const ctxSize = configuredLlmContextSize(
            spec,
            config.ctxSize,
            ctx.specs.gpuVramGb,
            overrides.ctxSize,
          );
          ctx.logger.info(
            "llama-server",
            `Spawning model "${modelId}" (file: ${modelFile}, context: ${ctxSize} tokens)`,
          );
          const modelPath = join(config.llmModelsDir, modelFile);
          return resolveConfiguredLlmLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.llmModelsDir,
            modelId,
            modelFile,
            host: llmHost(snapshot.config, overrides),
            port: llmPort(snapshot.config, overrides),
            model: spec,
            configCtxSize: config.ctxSize,
            ctxSizeOverride: overrides.ctxSize,
            parallel: config.parallel,
            artifactBytes: await llmArtifactBytes(
              modelId,
              config.llmModelsDir,
              modelFile,
            ),
            memoryGb: ctx.specs.gpuVramGb,
            kvGeometry: await readLlmKvGeometry(modelPath),
            trainingContextLength:
              await readLlmTrainingContextLength(modelPath),
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
        preflightDemand: async (signal) => {
          if (signal?.aborted) return undefined;
          const spec = byId(modelId);
          const modelFile =
            overrides.sttModelFile ??
            (await configuredModelFile(config, modelId, modality));
          const preflightModelFile =
            modelFile || (spec ? primaryArtifact(spec).filename : undefined);
          if (!preflightModelFile) return undefined;
          const plan = resolveSttLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.sttModelsDir,
            modelId,
            modelFile: preflightModelFile,
            host: sttHost(snapshot.config, overrides),
            port: sttPort(snapshot.config, overrides),
            modelRequirementGb: spec?.minVramGb,
            artifactBytes: await artifactBytes(
              modelId,
              config.sttModelsDir,
              preflightModelFile,
            ),
          });
          return signal?.aborted ? undefined : plan.memoryDemand;
        },
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
                  dependencies.installMissing,
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
          return await startWhisperServerProcess(
            plan,
            dependencies.memorySafety.topology,
          );
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
              dependencies.installMissing,
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
            ({ filename }) =>
              filename === spec.ttsRuntime?.projectorArtifactFilename,
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
          const references = ttsReferenceArtifacts(spec);
          return Object.freeze({
            binaryPath: await ensureBinary(config, "llama-tts"),
            modelPath,
            projectorPath: join(config.ttsModelsDir, projector.filename),
            speakerFiles: Object.freeze({
              harbor: join(config.ttsModelsDir, references.harbor.filename),
              willow: join(config.ttsModelsDir, references.willow.filename),
            }),
          });
        },
      });
    }

    const base = baseUrl(modality, snapshot);
    if (modality === "video") {
      return new ManagedService({
        runtimeId,
        modality,
        component: "sd-server",
        healthUrl: `${base}/`,
        logger: ctx.logger,
        preflightDemand: async (signal) => {
          if (signal?.aborted) return undefined;
          const target = videoRuntimeTarget(
            dependencies.memorySafety.topology,
            dependencies.host,
          );
          const spec = byId(modelId);
          if (!spec || spec.kind !== "video" || !spec.videoRuntime)
            return undefined;
          assertModelEligible(spec, {
            allowExperimental: config.allowExperimental,
            target,
          });
          const installation = await resolveCatalogInstallation(
            spec,
            config.videoModelsDir,
          );
          if (!installation.complete) {
            return signal?.aborted
              ? undefined
              : {
                  ...spec.videoRuntime.estimatedMemoryDemand,
                  confidence: "estimated" as const,
                };
          }
          const plan = resolveVideoLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.videoModelsDir,
            modelId,
            diffusionModelFile: spec.videoRuntime.artifacts.diffusionModel,
            textEncoderFile: spec.videoRuntime.artifacts.textEncoder,
            host: videoHost(overrides),
            port: videoPort(overrides),
            videoRuntime: spec.videoRuntime,
            target,
          });
          return signal?.aborted ? undefined : plan.memoryDemand;
        },
        launch: async () => {
          const target = videoRuntimeTarget(
            dependencies.memorySafety.topology,
            dependencies.host,
          );
          const spec = byId(modelId);
          if (!spec || spec.kind !== "video" || !spec.videoRuntime) {
            throw new Error(
              `Video model \"${modelId}\" has no runtime profile.`,
            );
          }
          const artifacts = spec.videoRuntime.artifacts;
          const plan = resolveVideoLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.videoModelsDir,
            modelId,
            diffusionModelFile: artifacts.diffusionModel,
            textEncoderFile: artifacts.textEncoder,
            host: videoHost(overrides),
            port: videoPort(overrides),
            videoRuntime: spec.videoRuntime,
            target,
          });
          let installation = await resolveCatalogInstallation(
            spec,
            config.videoModelsDir,
          );
          if (!installation.complete) {
            await installSelectedModel(
              ctx,
              config,
              modality,
              modelId,
              "incomplete",
              dependencies.installMissing,
            );
            installation = await resolveCatalogInstallation(
              spec,
              config.videoModelsDir,
            );
          }
          if (!installation.complete) {
            throw new Error(
              `Video model \"${modelId}\" is incomplete after installation.`,
            );
          }
          const requiredPaths = [
            artifacts.diffusionModel,
            artifacts.textEncoder,
            artifacts.decoder.artifactFilename,
            ...(spec.videoRuntime.mode === "s2v"
              ? [spec.videoRuntime.artifacts.audioEncoder]
              : []),
          ].map((filename) => join(config.videoModelsDir, filename));
          if (
            !(
              await Promise.all(
                requiredPaths.map((path) => Bun.file(path).exists()),
              )
            ).every(Boolean)
          ) {
            throw new Error("Video model artifacts are missing.");
          }
          for (const artifact of spec.artifacts) {
            if (
              artifact.expectedSizeBytes === undefined ||
              artifact.sha256 === undefined
            ) {
              throw new Error(
                `Video model \"${modelId}\" lacks immutable artifact authority.`,
              );
            }
            await verifyAuthoritativeFile(
              join(config.videoModelsDir, artifact.filename),
              {
                filename: artifact.filename,
                expectedSizeBytes: artifact.expectedSizeBytes,
                sha256: artifact.sha256,
              },
              config.videoModelsDir,
            );
          }
          return plan;
        },
        start: async (plan) => {
          if (plan.component !== "sd-server" || plan.modality !== "video") {
            throw new Error("Expected video sd-server launch plan.");
          }
          return await startSdVideoServerProcess(
            plan,
            dependencies.memorySafety.topology,
          );
        },
        memorySafety: dependencies.memorySafety,
        otel: ctx.otel,
        startupTimeoutMs: 180000,
      });
    }
    return new ManagedService({
      runtimeId,
      modality,
      component: "sd-server",
      healthUrl: `${base}/`,
      logger: ctx.logger,
      preflightDemand: async (signal) => {
        if (signal?.aborted) return undefined;
        const spec = byId(modelId);
        let modelFile = overrides.imageModelFile;
        if (spec?.imageRuntime) {
          modelFile = primaryArtifact(spec).filename;
        } else if (!modelFile) {
          // A missing catalog artifact is admitted on its catalog demand.
          modelFile =
            (await configuredModelFile(config, modelId, modality)) ||
            (spec ? primaryArtifact(spec).filename : undefined);
          if (!modelFile) return undefined;
        }
        const plan = resolveImageLaunchPlan({
          runtimeId,
          root: config.root,
          modelsDirectory: config.imageModelsDir,
          modelId,
          modelFile,
          host: imageHost(overrides),
          port: imagePort(overrides),
          modelRequirementGb: spec?.minVramGb,
          imageRuntime: spec?.imageRuntime,
          artifactBytes: spec?.imageRuntime
            ? spec.artifacts.reduce(
                (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
                0,
              )
            : await artifactBytes(modelId, config.imageModelsDir, modelFile),
        });
        return signal?.aborted ? undefined : plan.memoryDemand;
      },
      launch: async () => {
        let modelFile = overrides.imageModelFile;
        const imageSpec = byId(modelId);
        if (imageSpec?.imageRuntime) {
          if (modelFile && modelFile !== primaryArtifact(imageSpec).filename) {
            throw new Error(
              "Image artifact overrides cannot replace a catalog runtime profile.",
            );
          }
          let installation = await resolveCatalogInstallation(
            imageSpec,
            config.imageModelsDir,
          );
          if (!installation.complete) {
            await installSelectedModel(
              ctx,
              config,
              modality,
              modelId,
              "incomplete",
              dependencies.installMissing,
            );
            installation = await resolveCatalogInstallation(
              imageSpec,
              config.imageModelsDir,
            );
          }
          if (!installation.complete)
            throw new Error("Image model installation is incomplete.");
          modelFile = basename(installation.primaryPath);
        }
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
                dependencies.installMissing,
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
          imageRuntime: spec?.imageRuntime,
          artifactBytes: spec?.imageRuntime
            ? spec.artifacts.reduce(
                (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
                0,
              )
            : await artifactBytes(modelId, config.imageModelsDir, modelFile),
        });
      },
      start: async (plan) => {
        if (plan.component !== "sd-server" || plan.modality !== "image") {
          throw new Error("Expected sd-server launch plan.");
        }
        return await startSdServerProcess(
          plan,
          dependencies.memorySafety.topology,
        );
      },
      memorySafety: dependencies.memorySafety,
      otel: ctx.otel,
      startupTimeoutMs: 30000,
    });
  };

  return Object.freeze({ create, baseUrl });
}
