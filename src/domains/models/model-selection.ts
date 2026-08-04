import { byId, type ModelKind, type ModelSpec } from "../../catalog";
import { z } from "zod";

function modelHasExpectedModalities(
  model: ModelSpec,
  kind: ModelKind,
): boolean {
  const expected =
    kind === "llm"
      ? { input: "text", output: "text" }
      : kind === "stt"
        ? { input: "audio", output: "text" }
        : { input: "text", output: "image" };
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
    );
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
    selectedImageModels: selectedModelsSchema("image", false),
    activeLlmModel: modelIdSchema("llm"),
    activeSttModel: z.union([z.literal(""), modelIdSchema("stt")]),
    activeImageModel: z.union([z.literal(""), modelIdSchema("image")]),
  })
  .strict()
  .superRefine((config, ctx) => {
    const activeModels = [
      ["activeLlmModel", config.activeLlmModel, config.selectedLlmModels],
      ["activeSttModel", config.activeSttModel, config.selectedSttModels],
      ["activeImageModel", config.activeImageModel, config.selectedImageModels],
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

export type ModelConfiguration = z.infer<typeof modelConfigurationSchema>;

export function validateModelList(
  ids: string[] | undefined,
  kind: ModelKind,
): string[] | undefined {
  if (!ids) return undefined;
  return selectedModelsSchema(kind, kind === "llm").parse(ids);
}
