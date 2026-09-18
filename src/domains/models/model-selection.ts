import {
  byId,
  type ModelKind,
  type ModelModality,
  type ModelSpec,
} from "../../catalog";
import { z } from "zod";

const expectedModalities = {
  llm: { input: "text", output: "text" },
  stt: { input: "audio", output: "text" },
  tts: { input: "text", output: "audio" },
  image: { input: "text", output: "image" },
  video: { input: "text", output: "video" },
} satisfies Record<
  ModelKind,
  Readonly<{ input: ModelModality; output: ModelModality }>
>;

function modelHasExpectedModalities(
  model: ModelSpec,
  kind: ModelKind,
): boolean {
  const expected = expectedModalities[kind];
  return (
    model.inputModalities.includes(expected.input) &&
    model.outputModalities.includes(expected.output)
  );
}

export function modelIdSchema(kind: ModelKind) {
  return z
    .string()
    .min(1)
    .refine(
      (id) => {
        const model = byId(id);
        return (
          !!model &&
          model.kind === kind &&
          modelHasExpectedModalities(model, kind)
        );
      },
      {
        message: `must name a catalog ${kind} model with compatible modalities`,
      },
    )
    .refine((id) => !byId(id)?.videoCatalogOnly, {
      message:
        "catalog-only models cannot be selected or active because LocalBase inference is unavailable",
    });
}

export function selectedModelsSchema(kind: ModelKind, requireOne: boolean) {
  const schema = modelIdSchema(kind)
    .array()
    .refine(
      (ids) => new Set(ids).size === ids.length,
      "must not contain duplicates",
    );
  return requireOne ? schema.min(1) : schema;
}

export const modelConfigurationSchema = z
  .object({
    selectedLlmModels: selectedModelsSchema("llm", true),
    selectedSttModels: selectedModelsSchema("stt", false),
    selectedTtsModels: selectedModelsSchema("tts", false),
    selectedImageModels: selectedModelsSchema("image", false),
    selectedVideoModels: selectedModelsSchema("video", false),
    activeLlmModel: modelIdSchema("llm"),
    activeSttModel: z.union([z.literal(""), modelIdSchema("stt")]),
    activeTtsModel: z.union([z.literal(""), modelIdSchema("tts")]),
    activeImageModel: z.union([z.literal(""), modelIdSchema("image")]),
    activeVideoModel: z.union([z.literal(""), modelIdSchema("video")]),
  })
  .strict()
  .superRefine((config, ctx) => {
    const activeModels = [
      ["activeLlmModel", config.activeLlmModel, config.selectedLlmModels],
      ["activeSttModel", config.activeSttModel, config.selectedSttModels],
      ["activeTtsModel", config.activeTtsModel, config.selectedTtsModels],
      ["activeImageModel", config.activeImageModel, config.selectedImageModels],
      ["activeVideoModel", config.activeVideoModel, config.selectedVideoModels],
    ] as const;
    for (const [field, id, selected] of activeModels) {
      if (id && !selected.includes(id)) {
        ctx.addIssue({
          code: "custom",
          path: [field],
          message: "must also be present in its selected model list",
        });
      }
    }
  });

export function validateModelList(
  ids: string[] | undefined,
  kind: ModelKind,
): string[] | undefined {
  if (!ids) return undefined;
  return selectedModelsSchema(kind, kind === "llm").parse(ids);
}
