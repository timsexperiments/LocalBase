import { byId, type ModelKind, type ModelSpec } from "../../catalog";
import type { LocalBaseConfig } from "../../manager";
import type { Permission } from "../auth/authorization";

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
export function listServedModels(
  config: ServedModelConfig,
  permissions?: readonly Permission[],
): OpenAiModel[] {
  const seen = new Set<string>();
  const data: OpenAiModel[] = [];
  for (const kind of kindOrder) {
    for (const id of configuredIds(kind, config)) {
      if (!id || seen.has(id)) continue;
      const spec = byId(id);
      const permission = permissionFor(kind, spec);
      if (permissions && !permissions.includes(permission)) continue;
      seen.add(id);
      data.push(project(kind, id, spec));
    }
  }
  return data;
}
