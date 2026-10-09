import { join } from "node:path";
import {
  byId,
  primaryArtifact,
  type ModelKind,
  type ModelSpec,
} from "../../catalog";
import type { LocalBaseConfig } from "../../manager";
import type { Permission } from "../auth/authorization";
import { resolveConfiguredLlmLaunchPlan } from "../runtime/launch-plan";
import {
  llamaContextPerSequence,
  type ParallelSlots,
} from "../config/parallel";
import type { LlmKvGeometry } from "../runtime/gguf-metadata";
import type { LlmSupervisorProfile } from "../runtime/supervisor-registry";

type ServedModelConfig = Pick<
  LocalBaseConfig,
  | "activeLlmModel"
  | "selectedLlmModels"
  | "activeSttModel"
  | "selectedSttModels"
  | "activeTtsModel"
  | "selectedTtsModels"
  | "activeImageModel"
  | "selectedImageModels"
  | "activeVideoModel"
  | "selectedVideoModels"
  | "ctxSize"
  | "parallel"
  | "root"
  | "llmModelsDir"
>;

export type OpenAiModel = Readonly<{
  id: string;
  object: "model";
  created: number;
  owned_by: "local-base";
  name?: string;
  context_length?: number;
  architecture: Readonly<{
    input_modalities: readonly string[];
    output_modalities: readonly string[];
  }>;
}>;

const kindOrder: readonly ModelKind[] = ["llm", "stt", "tts", "image", "video"];

const defaultModalities: Record<
  ModelKind,
  { input: readonly string[]; output: readonly string[] }
> = {
  llm: { input: ["text"], output: ["text"] },
  stt: { input: ["audio"], output: ["text"] },
  tts: { input: ["text"], output: ["audio"] },
  image: { input: ["text"], output: ["image"] },
  video: { input: ["text"], output: ["video"] },
};

function configuredIds(
  kind: ModelKind,
  config: ServedModelConfig,
  enabled: boolean | undefined,
): readonly string[] {
  switch (kind) {
    case "llm":
      return [config.activeLlmModel, ...config.selectedLlmModels];
    case "stt":
      return enabled || config.selectedSttModels.length > 0
        ? [config.activeSttModel, ...config.selectedSttModels]
        : [];
    case "tts":
      return enabled || config.selectedTtsModels.length > 0
        ? [config.activeTtsModel, ...config.selectedTtsModels]
        : [];
    case "image":
      return enabled || config.selectedImageModels.length > 0
        ? [config.activeImageModel, ...config.selectedImageModels]
        : [];
    case "video":
      return enabled || config.selectedVideoModels.length > 0
        ? [config.activeVideoModel, ...config.selectedVideoModels]
        : [];
  }
}

function permissionFor(kind: ModelKind, spec: ModelSpec | undefined) {
  switch (kind) {
    case "llm":
      return spec?.llmRuntime ? "inference:embeddings" : "inference:chat";
    case "stt":
      return "inference:transcription";
    case "tts":
      return "inference:speech";
    case "image":
      return "inference:image";
    case "video":
      return "inference:video";
  }
}

function project(kind: ModelKind, id: string, spec?: ModelSpec): OpenAiModel {
  const fallback = defaultModalities[kind];
  const output = spec?.llmRuntime
    ? ["embeddings"]
    : (spec?.outputModalities ?? fallback.output);
  return {
    id,
    object: "model",
    created: 1670000000,
    owned_by: "local-base",
    ...(spec ? { name: spec.family } : {}),
    ...(spec?.kind === "llm" && spec.contextWindowTokens
      ? { context_length: spec.contextWindowTokens }
      : {}),
    architecture: {
      input_modalities: spec?.inputModalities ?? fallback.input,
      output_modalities: output,
    },
  };
}

function omitContextLength(model: OpenAiModel): OpenAiModel {
  const { context_length, ...withoutContextLength } = model;
  void context_length;
  return withoutContextLength;
}

/**
 * Lists the models the gateway would route requests to: per kind, the active
 * model plus selected models (the reconciler's resolution rule), skipping
 * unconfigured modalities and empty ids. When `permissions` is given, models
 * the caller cannot invoke are omitted.
 */
export async function listServedModels(
  config: ServedModelConfig,
  permissions?: readonly Permission[],
  options: Readonly<{
    enabled?: Partial<Record<ModelKind, boolean>>;
    ctxSizeOverride?: number;
    parallel?: ParallelSlots;
    pinnedContextLength?: number;
    memoryGb?: number;
    llmModelFile?: string;
    kvGeometryForModel?: (id: string) => Promise<LlmKvGeometry | null>;
    trainingContextLengthForModel?: (id: string) => Promise<number | null>;
    llmProfile?: LlmSupervisorProfile;
  }> = {},
): Promise<OpenAiModel[]> {
  const seen = new Set<string>();
  const data: OpenAiModel[] = [];
  for (const kind of kindOrder) {
    if (options.enabled?.[kind] === false) continue;
    for (const id of configuredIds(kind, config, options.enabled?.[kind])) {
      if (!id || seen.has(id)) continue;
      const spec = byId(id);
      const permission = permissionFor(kind, spec);
      if (permissions && !permissions.includes(permission)) continue;
      seen.add(id);
      let model = project(kind, id, spec);
      if (kind === "llm") {
        const capturedProfile = options.llmProfile;
        const profile =
          capturedProfile &&
          (id === capturedProfile.modelId ||
            (options.llmModelFile !== undefined &&
              capturedProfile.modelFile === options.llmModelFile))
            ? capturedProfile
            : undefined;
        if (options.llmModelFile && options.pinnedContextLength !== undefined) {
          const geometry = await options.kvGeometryForModel?.(
            config.activeLlmModel,
          );
          const trainingContextLength =
            (await options.trainingContextLengthForModel?.(
              config.activeLlmModel,
            )) ?? geometry?.contextLength;
          model = {
            ...model,
            context_length: Math.min(
              options.pinnedContextLength,
              trainingContextLength ?? Number.POSITIVE_INFINITY,
            ),
          };
        } else {
          const launchModelId =
            profile?.modelId ??
            (options.llmModelFile ? config.activeLlmModel : id);
          const launchSpec = byId(launchModelId);
          const modelFile =
            profile?.modelFile ??
            options.llmModelFile ??
            (launchSpec
              ? primaryArtifact(launchSpec).filename
              : `${launchModelId}.gguf`);
          const modelPath = join(config.llmModelsDir, modelFile);
          const artifactBytes =
            (launchSpec
              ? launchSpec.artifacts.reduce(
                  (total, { expectedSizeBytes }) =>
                    total + (expectedSizeBytes ?? 0),
                  0,
                ) || undefined
              : undefined) ??
            (await Bun.file(modelPath)
              .stat()
              .then((file) => file.size)
              .catch(() => 0));
          try {
            const launch = resolveConfiguredLlmLaunchPlan({
              runtimeId: `models-list:${launchModelId}`,
              root: config.root,
              modelsDirectory: config.llmModelsDir,
              modelId: launchModelId,
              modelFile,
              host: "127.0.0.1",
              port: 1,
              model: launchSpec,
              configCtxSize: profile?.configCtxSize ?? config.ctxSize,
              ctxSizeOverride: profile
                ? profile.ctxSizeOverride
                : options.ctxSizeOverride,
              parallel:
                profile?.parallel ?? options.parallel ?? config.parallel ?? 1,
              artifactBytes,
              memoryGb: options.memoryGb ?? 0,
              kvGeometry: await options.kvGeometryForModel?.(launchModelId),
            });
            model = {
              ...model,
              context_length: llamaContextPerSequence(
                launch.ctxSize,
                launch.parallel.slots,
              ),
            };
            const trainingContextLength =
              (await options.trainingContextLengthForModel?.(launchModelId)) ??
              launch.kvGeometry?.contextLength;
            if (trainingContextLength != null) {
              model = {
                ...model,
                context_length: Math.min(
                  model.context_length ?? Number.POSITIVE_INFINITY,
                  trainingContextLength,
                ),
              };
            }
          } catch {
            model = omitContextLength(model);
          }
        }
      }
      data.push(model);
    }
  }
  return data;
}
