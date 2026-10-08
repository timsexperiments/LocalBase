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
import type { ParallelSlots } from "../config/parallel";
import type { LlmKvGeometry } from "../runtime/gguf-metadata";

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
): readonly string[] {
  switch (kind) {
    case "llm":
      return [config.activeLlmModel, ...config.selectedLlmModels];
    case "stt":
      return config.selectedSttModels.length > 0
        ? [config.activeSttModel, ...config.selectedSttModels]
        : [];
    case "tts":
      return config.selectedTtsModels.length > 0
        ? [config.activeTtsModel, ...config.selectedTtsModels]
        : [];
    case "image":
      return config.selectedImageModels.length > 0
        ? [config.activeImageModel, ...config.selectedImageModels]
        : [];
    case "video":
      return config.selectedVideoModels.length > 0
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
    memoryGb?: number;
    llmModelFile?: string;
    kvGeometryForModel?: (id: string) => Promise<LlmKvGeometry | null>;
  }> = {},
): Promise<OpenAiModel[]> {
  const seen = new Set<string>();
  const data: OpenAiModel[] = [];
  for (const kind of kindOrder) {
    if (options.enabled?.[kind] === false) continue;
    for (const id of configuredIds(kind, config)) {
      if (!id || seen.has(id)) continue;
      const spec = byId(id);
      const permission = permissionFor(kind, spec);
      if (permissions && !permissions.includes(permission)) continue;
      seen.add(id);
      let model = project(kind, id, spec);
      if (kind === "llm") {
        const modelFile =
          options.llmModelFile ??
          (spec ? primaryArtifact(spec).filename : `${id}.gguf`);
        const modelPath = join(config.llmModelsDir, modelFile);
        const artifactBytes =
          (spec ? primaryArtifact(spec).expectedSizeBytes : undefined) ??
          (await Bun.file(modelPath)
            .stat()
            .then((file) => file.size)
            .catch(() => 0));
        const launch = resolveConfiguredLlmLaunchPlan({
          runtimeId: `models-list:${id}`,
          root: config.root,
          modelsDirectory: config.llmModelsDir,
          modelId: id,
          modelFile,
          host: "127.0.0.1",
          port: 1,
          model: spec,
          configCtxSize: config.ctxSize,
          ctxSizeOverride: options.ctxSizeOverride,
          parallel: options.parallel ?? config.parallel ?? 1,
          artifactBytes,
          memoryGb: options.memoryGb ?? 0,
          kvGeometry: await options.kvGeometryForModel?.(id),
        });
        model = { ...model, context_length: launch.parallel.contextPerSlot };
      }
      data.push(model);
    }
  }
  return data;
}
