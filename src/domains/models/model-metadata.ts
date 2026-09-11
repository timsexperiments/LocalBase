import { z } from "zod";
import {
  resolveCatalogInstallation,
  type ModelKind,
  type ModelSpec,
} from "../../catalog";
import type { LocalBaseConfig } from "../../manager";
import { modalityLifecycleStateSchema } from "../runtime/health";
import type { RuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import type { RuntimeModality } from "../runtime/modality";

const nullableNumberSchema = z.number().nullable();

export const modelMetadataSchema = z
  .object({
    object: z.literal("localbase.model"),
    id: z.string().min(1),
    catalog: z
      .object({
        name: z.string().min(1),
        revision: z.string().min(1),
        kind: z.enum(["llm", "stt", "image"]),
        quantization: z.string().min(1),
        artifacts: z
          .array(
            z
              .object({
                role: z.enum(["primary", "supplementary"]),
                sha256: z.string().nullable(),
                sizeBytes: nullableNumberSchema,
              })
              .strict(),
          )
          .min(1),
        memory: z
          .object({
            minimumVramEstimateGb: z.number().nonnegative(),
            storageEstimateGb: z.number().positive(),
          })
          .strict(),
        capabilities: z.null(),
        contextWindowTokens: z.null(),
        maxOutputTokens: z.null(),
      })
      .strict(),
    device: z
      .object({
        selected: z.boolean(),
        installed: z.boolean(),
        runtime: z
          .object({
            configured: z.boolean(),
            state: modalityLifecycleStateSchema,
            effectiveSlots: z.number().int().positive().nullable(),
          })
          .strict()
          .nullable(),
      })
      .strict(),
  })
  .strict();
export type ModelMetadata = z.infer<typeof modelMetadataSchema>;

export const modelMetadataListSchema = z
  .object({
    object: z.literal("list"),
    data: z.array(modelMetadataSchema),
  })
  .strict();
export type ModelMetadataList = z.infer<typeof modelMetadataListSchema>;

export type ModelInstallation = readonly [modelId: string, complete: boolean];

export type ModelMetadataProjectionInput = Readonly<{
  catalog: readonly ModelSpec[];
  selectedModels: Readonly<Record<RuntimeModality, readonly string[]>>;
  installations: ReadonlyMap<string, boolean>;
  runtimes: Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>>;
}>;

function selectedModelIds(
  selectedModels: ModelMetadataProjectionInput["selectedModels"],
): ReadonlySet<string> {
  return new Set([
    ...selectedModels.llm,
    ...selectedModels.stt,
    ...selectedModels.image,
  ]);
}

function runtimeForModel(
  model: ModelSpec,
  runtimes: ModelMetadataProjectionInput["runtimes"],
): ModelMetadata["device"]["runtime"] {
  const runtime = runtimes[model.kind];
  if (runtime.modelId !== model.modelId) return null;
  return {
    configured: runtime.configured,
    state: runtime.state,
    effectiveSlots: runtime.configuredSlots,
  };
}

/** Projects catalog facts and observed device state without initiating runtime work. */
export function projectModelMetadata(
  model: ModelSpec,
  input: ModelMetadataProjectionInput,
): ModelMetadata {
  return modelMetadataSchema.parse({
    object: "localbase.model",
    id: model.modelId,
    catalog: {
      name: model.family,
      revision: model.repositoryRevision,
      kind: model.kind,
      quantization: model.quant,
      artifacts: model.artifacts.map((artifact) => ({
        role: artifact.role,
        sha256: artifact.sha256 ?? null,
        sizeBytes: artifact.expectedSizeBytes ?? null,
      })),
      memory: {
        minimumVramEstimateGb: model.minVramGb,
        storageEstimateGb: model.storageGb,
      },
      capabilities: null,
      contextWindowTokens: null,
      maxOutputTokens: null,
    },
    device: {
      selected: selectedModelIds(input.selectedModels).has(model.modelId),
      installed: input.installations.get(model.modelId) ?? false,
      runtime: runtimeForModel(model, input.runtimes),
    },
  });
}

export function projectModelMetadataList(
  input: ModelMetadataProjectionInput,
): ModelMetadataList {
  return modelMetadataListSchema.parse({
    object: "list",
    data: input.catalog.map((model) => projectModelMetadata(model, input)),
  });
}

function directoryForModel(config: LocalBaseConfig, kind: ModelKind): string {
  if (kind === "llm") return config.llmModelsDir;
  if (kind === "stt") return config.sttModelsDir;
  return config.imageModelsDir;
}

/** Reads file existence and size only. It never hashes, downloads, or launches models. */
export async function inspectCatalogInstallations(
  config: LocalBaseConfig,
  catalog: readonly ModelSpec[],
): Promise<ReadonlyMap<string, boolean>> {
  const installations = await Promise.all(
    catalog.map(async (model): Promise<ModelInstallation> => [
      model.modelId,
      (
        await resolveCatalogInstallation(
          model,
          directoryForModel(config, model.kind),
        )
      ).complete,
    ]),
  );
  return new Map(installations);
}

export function modelMetadataById(
  modelId: string,
  input: ModelMetadataProjectionInput,
): ModelMetadata | undefined {
  const model = input.catalog.find(
    (candidate) => candidate.modelId === modelId,
  );
  return model ? projectModelMetadata(model, input) : undefined;
}
