import { basename, join } from "node:path";
import { verifyAuthoritativeFile } from "../../utils/checksum";
import {
  byId,
  embeddingRuntimeProfile,
  isDecisionModel,
  llmProjectorArtifact,
  primaryArtifact,
  resolveCatalogInstallation,
  ttsReferenceArtifacts,
  type ModelSpec,
} from "../../catalog";
import type { AppContext } from "../../context";
import type { ILogger } from "../observability/logging";
import {
  ensureBinary,
  installModel,
  type LocalBaseConfig,
  type ModelInstallEvent,
  type ModelInstallReporter,
} from "../../manager";
import {
  ensureKokoroRuntime,
  kokoroRuntimeCommand,
  kokoroRuntimeRelease,
} from "../../manager/kokoro-runtime";
import type { ServeInput } from "../app/commands/inputs";
import type { RuntimeConfigSnapshot } from "./config-snapshot";
import {
  readLlmKvGeometry,
  readLlmTrainingContextLength,
} from "./gguf-metadata";
import {
  backendBindHost,
  configuredLlmContextSize,
  describeLlmContextBudget,
  resolveImageLaunchPlan,
  resolveConfiguredLlmLaunchPlan,
  resolveSttLaunchPlan,
  resolveTtsLaunchPlan,
  resolveVideoLaunchPlan,
} from "./launch-plan";
import {
  imageStartupTimeoutMs,
  llmStartupTimeoutMs,
  VIDEO_STARTUP_TIMEOUT_MS,
} from "./startup-timeout";
import { assertModelInstallConsent } from "./startup-preflight";
import {
  startLlamaServerProcess,
  startSdServerProcess,
  startSdVideoServerProcess,
  startTtsServerProcess,
  startWhisperServerProcess,
} from "./launcher";
import type { RuntimeModality } from "./modality";
import type { VideoRuntimeTarget } from "../../catalog";
import {
  assertModelEligible,
  ModelHardwareIneligibleError,
  unsupportedModelTargetReason,
  videoTargetFromTopology,
} from "../models/model-eligibility";
import type { MemorySafetyController } from "./memory-controller";
import type { MemoryTopology } from "./memory-safety";
import { catalogModelDemand } from "./memory-estimate";
import {
  ttsAdmissionDemand,
  withAdmissionDemand,
  type ObservedPeakSource,
} from "./admission-demand";
import {
  SPEECH_MEMORY_DEMAND,
  SpeechSupervisor,
  type SpeechPreparation,
  type TtsServerPreparation,
  type WarmSpeechServer,
} from "./speech-supervisor";
import {
  ManagedService,
  type BackendReady,
  type ManagedServiceOptions,
} from "./supervisor";
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
  ttsHost?: string;
  ttsPort?: number;
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
  /**
   * Peaks this machine recorded for each model. When present, launch
   * admission plans with the observed peak for the same artifact and launch
   * shape ahead of checked-in, feed and estimated figures.
   */
  observedMemory?: ObservedPeakSource;
  /** Host platform used to select the video runtime target; defaults to this process. */
  host?: Readonly<{ platform: NodeJS.Platform; arch: NodeJS.Architecture }>;
  /**
   * Called after llama-server for a catalog model becomes healthy, so the
   * gateway can record what the backend reports about itself.
   */
  onLlmReady?: (
    event: Readonly<{
      modelId: string;
      baseUrl: string;
      embedding: boolean;
      decision: boolean;
    }>,
  ) => void | Promise<void>;
  /**
   * Called after any backend becomes healthy, with its process and plan, so
   * the gateway can sample its memory while it serves.
   */
  onBackendReady?: (
    event: Readonly<{
      modality: RuntimeModality;
      modelId: string;
      ready: BackendReady;
    }>,
  ) => void | Promise<void>;
}>;

export function runtimeEndpoint(host: string, port: number): string {
  const safeHost = backendBindHost(host);
  const urlHost =
    safeHost.includes(":") && !safeHost.startsWith("[")
      ? `[${safeHost}]`
      : safeHost;
  return `http://${urlHost}:${port}`;
}

/**
 * Runtime reports are keyed to the catalog artifact, so a launch that loads
 * some other file (a model-file override or an unpinned model) is not
 * recorded.
 */
function llmReadyReportable(
  modelId: string,
  overrides: RuntimeLaunchOverrides,
): boolean {
  const spec = byId(modelId);
  if (!spec || spec.kind !== "llm") return false;
  const primary = primaryArtifact(spec);
  if (!primary.sha256) return false;
  return (
    overrides.llmModelFile === undefined ||
    overrides.llmModelFile === primary.filename
  );
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

function ttsHost(overrides: RuntimeLaunchOverrides): string {
  return backendBindHost(overrides.ttsHost ?? "127.0.0.1");
}

function ttsPort(overrides: RuntimeLaunchOverrides): number {
  return overrides.ttsPort ?? 8092;
}

function videoRuntimeTarget(
  modelId: string,
  topology: MemoryTopology,
  host: Readonly<{
    platform: NodeJS.Platform;
    arch: NodeJS.Architecture;
  }> = process,
): VideoRuntimeTarget {
  const target = videoTargetFromTopology(topology, host);
  if (target) return target;
  throw new ModelHardwareIneligibleError(modelId, unsupportedModelTargetReason);
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

/**
 * Resolves the installed multimodal projector for a catalog LLM. A missing
 * projector launches text-only with a warning rather than failing the model.
 */
export async function resolveLlmProjectorPath(
  spec: ModelSpec | undefined,
  modelsDirectory: string,
  logger: Pick<ILogger, "info" | "warn">,
): Promise<string | null> {
  const projector = spec ? llmProjectorArtifact(spec) : undefined;
  if (!spec || !projector) return null;
  const path = join(modelsDirectory, projector.filename);
  if (await Bun.file(path).exists()) {
    logger.info("llama-server", `Loading projector ${projector.filename}`);
    return path;
  }
  logger.warn(
    "llama-server",
    `Projector ${projector.filename} for "${spec.modelId}" is not installed; launching text-only`,
  );
  return null;
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
    ...(input.ttsHost ? { ttsHost: input.ttsHost } : {}),
    ...(input.ttsPort ? { ttsPort: input.ttsPort } : {}),
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
  const admit = <Plan extends Parameters<typeof withAdmissionDemand>[0]>(
    modelId: string,
    plan: Plan,
    artifactBytes?: number,
  ): Plan =>
    withAdmissionDemand(plan, byId(modelId), {
      observed: dependencies.observedMemory,
      artifactBytes,
    });
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

  const readyHook = (
    modality: RuntimeModality,
    modelId: string,
    extra?: () => void | Promise<void>,
  ): Pick<ManagedServiceOptions, "onReady"> => {
    const onBackendReady = dependencies.onBackendReady;
    if (!onBackendReady && !extra) return {};
    return {
      onReady: async (ready) => {
        await Promise.all([
          onBackendReady?.({ modality, modelId, ready }),
          extra?.(),
        ]);
      },
    };
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
          ctxMode: config.ctxMode,
          ...(overrides.ctxSize !== undefined
            ? { ctxSizeOverride: overrides.ctxSize }
            : {}),
          parallel: config.parallel,
        },
        healthUrl: `${base}/health`,
        slotsUrl: `${base}/slots`,
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
          const llmBytes = await llmArtifactBytes(
            modelId,
            config.llmModelsDir,
            modelFile,
          );
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
            ctxMode: config.ctxMode,
            ctxSizeOverride: overrides.ctxSize,
            parallel: config.parallel,
            artifactBytes: llmBytes,
            memoryGb: ctx.specs.gpuVramGb,
            kvGeometry: await readLlmKvGeometry(modelPath),
            trainingContextLength:
              await readLlmTrainingContextLength(modelPath),
          });
          const admitted = admit(modelId, plan, llmBytes);
          return signal?.aborted ? undefined : admitted.memoryDemand;
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
            config.ctxMode,
          );
          const projectorPath = await resolveLlmProjectorPath(
            spec,
            config.llmModelsDir,
            ctx.logger,
          );
          ctx.logger.info(
            "llama-server",
            `Spawning model "${modelId}" (file: ${modelFile}, context: ${ctxSize} tokens)`,
          );
          const modelPath = join(config.llmModelsDir, modelFile);
          const llmBytes = await llmArtifactBytes(
            modelId,
            config.llmModelsDir,
            modelFile,
          );
          const plan = resolveConfiguredLlmLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.llmModelsDir,
            modelId,
            modelFile,
            host: llmHost(snapshot.config, overrides),
            port: llmPort(snapshot.config, overrides),
            model: spec,
            configCtxSize: config.ctxSize,
            ctxMode: config.ctxMode,
            ctxSizeOverride: overrides.ctxSize,
            parallel: config.parallel,
            artifactBytes: llmBytes,
            memoryGb: ctx.specs.gpuVramGb,
            kvGeometry: await readLlmKvGeometry(modelPath),
            trainingContextLength:
              await readLlmTrainingContextLength(modelPath),
            projectorPath,
          });
          const budget = describeLlmContextBudget({
            model: spec,
            configCtxSize: config.ctxSize,
            ctxMode: config.ctxMode,
            ctxSizeOverride: overrides.ctxSize,
            memoryGb: ctx.specs.gpuVramGb,
            plan,
          });
          ctx.logger.info(
            "llama-server",
            `Context budget: mode=${budget.mode} ceiling=${budget.configured_ceiling} recommended=${budget.recommended_max} launch=${budget.launch_context} slots=${budget.parallel_slots} effective=${budget.effective_context}`,
          );
          for (const reason of budget.reasons) {
            ctx.logger.info("llama-server", reason);
          }
          return admit(modelId, plan, llmBytes);
        },
        start: async (plan) => {
          if (plan.component !== "llama-server") {
            throw new Error("Expected llama-server launch plan.");
          }
          return await startLlamaServerProcess(plan);
        },
        memorySafety: dependencies.memorySafety,
        otel: ctx.otel,
        startupTimeoutMs: llmStartupTimeoutMs(byId(modelId)),
        ...readyHook(
          modality,
          modelId,
          dependencies.onLlmReady && llmReadyReportable(modelId, overrides)
            ? () =>
                dependencies.onLlmReady?.({
                  modelId,
                  baseUrl: base,
                  embedding: Boolean(embeddingRuntimeProfile(byId(modelId))),
                  decision: isDecisionModel(byId(modelId)),
                })
            : undefined,
        ),
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
            model: spec,
            artifactBytes: await artifactBytes(
              modelId,
              config.sttModelsDir,
              preflightModelFile,
            ),
          });
          return signal?.aborted
            ? undefined
            : admit(modelId, plan).memoryDemand;
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
          return admit(
            modelId,
            resolveSttLaunchPlan({
              runtimeId,
              root: config.root,
              modelsDirectory: config.sttModelsDir,
              modelId,
              modelFile,
              host: sttHost(snapshot.config, overrides),
              port: sttPort(snapshot.config, overrides),
              model: spec,
              artifactBytes: await artifactBytes(
                modelId,
                config.sttModelsDir,
                modelFile,
              ),
            }),
          );
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
        ...readyHook(modality, modelId),
        memorySafety: dependencies.memorySafety,
        otel: ctx.otel,
        startupTimeoutMs: 30000,
      });
    }

    if (modality === "tts") {
      const ttsSpec = byId(modelId);
      const ttsDemand =
        ttsSpec?.kind === "tts"
          ? ttsAdmissionDemand(ttsSpec, catalogModelDemand(ttsSpec), {
              observed: dependencies.observedMemory,
              loadsPrimaryArtifact: !overrides.ttsModelFile,
            })
          : undefined;
      const createWarmServer = (
        preparation: TtsServerPreparation,
      ): WarmSpeechServer => {
        const plan = resolveTtsLaunchPlan({
          runtimeId,
          root: config.root,
          modelId,
          modelPath: preparation.modelPath,
          projectorPath: preparation.projectorPath,
          speakerFiles: preparation.speakerFiles,
          host: ttsHost(overrides),
          port: ttsPort(overrides),
          memoryDemand: ttsDemand ?? SPEECH_MEMORY_DEMAND,
        });
        const service = new ManagedService({
          runtimeId,
          modality,
          component: "tts-server",
          healthUrl: plan.healthUrl,
          slotsUrl: plan.slotsUrl,
          logger: ctx.logger,
          launch: async () => plan,
          start: async (launchPlan) => {
            if (launchPlan.component !== "tts-server") {
              throw new Error("Expected tts-server launch plan.");
            }
            return await startTtsServerProcess(launchPlan);
          },
          ...readyHook(modality, modelId),
          memorySafety: dependencies.memorySafety,
          otel: ctx.otel,
          startupTimeoutMs: 120_000,
        });
        return Object.freeze({
          generateUrl: plan.generateUrl,
          ensureRunning: () => service.ensureRunning(),
          state: () => service.state(),
          processingSlots: () => service.processingSlots(),
          kill: () => service.kill(),
          shutdown: () => service.shutdown(),
        });
      };
      return new SpeechSupervisor({
        runtimeId,
        root: config.root,
        modelId,
        logger: ctx.logger,
        memorySafety: dependencies.memorySafety,
        ...(ttsDemand ? { memoryDemand: ttsDemand } : {}),
        createWarmServer,
        prepare: async (): Promise<SpeechPreparation> => {
          const spec = byId(modelId);
          if (!spec || spec.kind !== "tts") {
            throw new Error(`Unknown TTS model "${modelId}".`);
          }
          const profile = spec.ttsRuntime;
          if (!profile) {
            throw new Error(`TTS model "${modelId}" has no runtime profile.`);
          }
          // Refuse unpublished runtime pins before downloading model weights.
          if (profile.kind === "kokoro-onnx") kokoroRuntimeRelease();

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
          if (profile.kind === "kokoro-onnx") {
            const runtime = await ensureKokoroRuntime(config);
            const modelFile = (filename: string) =>
              join(config.ttsModelsDir, filename);
            return Object.freeze({
              kind: "kokoro-onnx" as const,
              command: kokoroRuntimeCommand(runtime),
              modelFiles: Object.freeze({
                model: modelFile(profile.artifacts.model),
                config: modelFile(profile.artifacts.config),
                tokenizer: modelFile(profile.artifacts.tokenizer),
                tokenizerConfig: modelFile(profile.artifacts.tokenizerConfig),
              }),
              inventorySha256: runtime.inventorySha256,
            });
          }
          const projector = spec.artifacts.find(
            ({ filename }) => filename === profile.projectorArtifactFilename,
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
          const assets = {
            modelPath,
            projectorPath: join(config.ttsModelsDir, projector.filename),
            speakerFiles: Object.freeze({
              harbor: join(config.ttsModelsDir, references.harbor.filename),
              willow: join(config.ttsModelsDir, references.willow.filename),
            }),
          };
          // Prefer the warm, streaming server; one-shot llama-tts remains the
          // fallback until a tts-server release is available on this host.
          try {
            return Object.freeze({
              kind: "tts-server" as const,
              binaryPath: await ensureBinary(config, "tts-server"),
              ...assets,
            });
          } catch (error) {
            ctx.logger.event({
              severity: "warn",
              eventName: "runtime.tts-server-unavailable",
              category: "runtime",
              component: "tts-server",
              runtime: "tts",
              message:
                "tts-server is unavailable; using one-shot llama-tts generation.",
              error: {
                type: error instanceof Error ? error.name : "Error",
                message: error instanceof Error ? error.message : String(error),
              },
            });
          }
          return Object.freeze({
            kind: "llama-tts" as const,
            binaryPath: await ensureBinary(config, "llama-tts"),
            ...assets,
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
          const spec = byId(modelId);
          if (!spec || spec.kind !== "video" || !spec.videoRuntime)
            return undefined;
          const target = videoRuntimeTarget(
            modelId,
            dependencies.memorySafety.topology,
            dependencies.host,
          );
          assertModelEligible(spec, {
            allowExperimental: config.allowExperimental,
            target,
          });
          const installation = await resolveCatalogInstallation(
            spec,
            config.videoModelsDir,
          );
          if (!installation.complete) {
            return signal?.aborted ? undefined : catalogModelDemand(spec);
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
            model: spec,
          });
          return signal?.aborted
            ? undefined
            : admit(modelId, plan).memoryDemand;
        },
        launch: async () => {
          const spec = byId(modelId);
          if (!spec || spec.kind !== "video" || !spec.videoRuntime) {
            throw new Error(
              `Video model \"${modelId}\" has no runtime profile.`,
            );
          }
          const target = videoRuntimeTarget(
            modelId,
            dependencies.memorySafety.topology,
            dependencies.host,
          );
          assertModelEligible(spec, {
            allowExperimental: config.allowExperimental,
            target,
          });
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
            model: spec,
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
          return admit(modelId, plan);
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
        ...readyHook(modality, modelId),
        memorySafety: dependencies.memorySafety,
        otel: ctx.otel,
        startupTimeoutMs: VIDEO_STARTUP_TIMEOUT_MS,
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
          model: spec,
          imageRuntime: spec?.imageRuntime,
          artifactBytes: spec?.imageRuntime
            ? spec.artifacts.reduce(
                (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
                0,
              )
            : await artifactBytes(modelId, config.imageModelsDir, modelFile),
        });
        return signal?.aborted ? undefined : admit(modelId, plan).memoryDemand;
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
        return admit(
          modelId,
          resolveImageLaunchPlan({
            runtimeId,
            root: config.root,
            modelsDirectory: config.imageModelsDir,
            modelId,
            modelFile,
            host: imageHost(overrides),
            port: imagePort(overrides),
            model: spec,
            imageRuntime: spec?.imageRuntime,
            artifactBytes: spec?.imageRuntime
              ? spec.artifacts.reduce(
                  (total, artifact) =>
                    total + (artifact.expectedSizeBytes ?? 0),
                  0,
                )
              : await artifactBytes(modelId, config.imageModelsDir, modelFile),
          }),
        );
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
      ...readyHook(modality, modelId),
      memorySafety: dependencies.memorySafety,
      otel: ctx.otel,
      startupTimeoutMs: imageStartupTimeoutMs(byId(modelId)),
    });
  };

  return Object.freeze({ create, baseUrl });
}
