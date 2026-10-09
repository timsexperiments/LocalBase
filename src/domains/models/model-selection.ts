import {
  byId,
  type ModelKind,
  type ModelModality,
  type ModelSpec,
} from "../../catalog";
import { z } from "zod";
import { modelEligibilityReason } from "./model-eligibility";
import type { VideoRuntimeTarget } from "../../catalog";

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

export function modelIdSchema(
  kind: ModelKind,
  allowExperimental = false,
  videoTarget: VideoRuntimeTarget | null = null,
  checkEligibility = true,
) {
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
    })
    .refine(
      (id) => {
        if (!checkEligibility) return true;
        const model = byId(id);
        return (
          !model ||
          !modelEligibilityReason(model, {
            allowExperimental,
            ...(model.videoRuntime
              ? { target: videoTarget }
              : { platform: process.platform, architecture: process.arch }),
          })
        );
      },
      {
        message: "model is experimental or unsupported on this platform",
      },
    );
}

export function selectedModelsSchema(
  kind: ModelKind,
  requireOne: boolean,
  allowExperimental = false,
  videoTarget: VideoRuntimeTarget | null = null,
  checkEligibility = true,
) {
  const schema = modelIdSchema(
    kind,
    allowExperimental,
    videoTarget,
    checkEligibility,
  )
    .array()
    .refine(
      (ids) => new Set(ids).size === ids.length,
      "must not contain duplicates",
    );
  return requireOne ? schema.min(1) : schema;
}

export function createModelConfigurationSchema(
  videoTarget: VideoRuntimeTarget | null,
  checkEligibility = true,
) {
  return z
    .object({
      allowExperimental: z.boolean().default(false),
      selectedLlmModels: z.array(z.string().min(1)),
      selectedSttModels: z.array(z.string().min(1)),
      selectedTtsModels: z.array(z.string().min(1)),
      selectedImageModels: z.array(z.string().min(1)),
      selectedVideoModels: z.array(z.string().min(1)),
      activeLlmModel: z.string().min(1),
      activeSttModel: z.string(),
      activeTtsModel: z.string(),
      activeImageModel: z.string(),
      activeVideoModel: z.string(),
    })
    .strict()
    .superRefine((config, ctx) => {
      const allowExperimental = !checkEligibility || config.allowExperimental;
      const selections = [
        ["selectedLlmModels", config.selectedLlmModels, "llm", true],
        ["selectedSttModels", config.selectedSttModels, "stt", false],
        ["selectedTtsModels", config.selectedTtsModels, "tts", false],
        ["selectedImageModels", config.selectedImageModels, "image", false],
        ["selectedVideoModels", config.selectedVideoModels, "video", false],
      ] as const;
      for (const [field, ids, kind, required] of selections) {
        const parsed = selectedModelsSchema(
          kind,
          required,
          allowExperimental,
          videoTarget,
          checkEligibility,
        ).safeParse(ids);
        if (!parsed.success) {
          for (const issue of parsed.error.issues)
            ctx.addIssue({ ...issue, path: [field, ...issue.path] });
        }
        for (const [index, id] of ids.entries()) {
          const model = byId(id);
          const reason =
            checkEligibility &&
            model &&
            modelEligibilityReason(model, {
              allowExperimental,
              ...(model.videoRuntime
                ? { target: videoTarget }
                : { platform: process.platform, architecture: process.arch }),
            });
          if (reason)
            ctx.addIssue({
              code: "custom",
              path: [field, index],
              message: reason,
            });
        }
      }
      const activeFields = [
        ["activeLlmModel", config.activeLlmModel, "llm"],
        ["activeSttModel", config.activeSttModel, "stt"],
        ["activeTtsModel", config.activeTtsModel, "tts"],
        ["activeImageModel", config.activeImageModel, "image"],
        ["activeVideoModel", config.activeVideoModel, "video"],
      ] as const;
      for (const [field, id, kind] of activeFields) {
        if (!id) continue;
        const parsed = modelIdSchema(
          kind,
          allowExperimental,
          videoTarget,
          checkEligibility,
        ).safeParse(id);
        if (!parsed.success)
          for (const issue of parsed.error.issues)
            ctx.addIssue({ ...issue, path: [field, ...issue.path] });
      }
      const activeModels = [
        ["activeLlmModel", config.activeLlmModel, config.selectedLlmModels],
        ["activeSttModel", config.activeSttModel, config.selectedSttModels],
        ["activeTtsModel", config.activeTtsModel, config.selectedTtsModels],
        [
          "activeImageModel",
          config.activeImageModel,
          config.selectedImageModels,
        ],
        [
          "activeVideoModel",
          config.activeVideoModel,
          config.selectedVideoModels,
        ],
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
}

export const modelConfigurationSchema = createModelConfigurationSchema(null);

export function validateModelList(
  ids: string[] | undefined,
  kind: ModelKind,
  videoTarget: VideoRuntimeTarget | null,
): string[] | undefined {
  if (!ids) return undefined;
  return selectedModelsSchema(kind, kind === "llm", false, videoTarget).parse(
    ids,
  );
}
