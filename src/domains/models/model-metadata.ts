import { z } from "zod";
import {
  primaryArtifact,
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
        artifact: z
          .object({
            sha256: z.string().nullable(),
            sizeBytes: nullableNumberSchema,
          })
          .strict(),
        memory: z
          .object({
            minimumVramGb: z.number().nonnegative(),
            storageGb: z.number().positive(),
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
          })
          .strict()
          .nullable(),
        warm: z.null(),
        slots: z.null(),
        readiness: z.null(),
        queue: z.null(),
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
  config: Pick<
    LocalBaseConfig,
    "selectedLlmModels" | "selectedSttModels" | "selectedImageModels"
  >;
  installations: ReadonlyMap<string, boolean>;
  runtimes: Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>>;
}>;

function selectedModelIds(
  config: ModelMetadataProjectionInput["config"],
): ReadonlySet<string> {
  return new Set([
    ...config.selectedLlmModels,
    ...config.selectedSttModels,
    ...config.selectedImageModels,
  ]);
}

function runtimeForModel(
  model: ModelSpec,
  runtimes: ModelMetadataProjectionInput["runtimes"],
): ModelMetadata["device"]["runtime"] {
  const runtime = runtimes[model.kind];
  if (runtime.modelId !== model.modelId) return null;
  return { configured: runtime.configured, state: runtime.state };
}

/** Projects catalog facts and observed device state without initiating runtime work. */
export function projectModelMetadata(
  model: ModelSpec,
  input: ModelMetadataProjectionInput,
): ModelMetadata {
  const artifact = primaryArtifact(model);
  return modelMetadataSchema.parse({
    object: "localbase.model",
    id: model.modelId,
    catalog: {
      name: model.family,
      revision: model.repositoryRevision,
      kind: model.kind,
      quantization: model.quant,
      artifact: {
        sha256: artifact.sha256 ?? null,
        sizeBytes: artifact.expectedSizeBytes ?? null,
      },
      memory: {
        minimumVramGb: model.minVramGb,
        storageGb: model.storageGb,
      },
      capabilities: null,
      contextWindowTokens: null,
      maxOutputTokens: null,
    },
    device: {
      selected: selectedModelIds(input.config).has(model.modelId),
      installed: input.installations.get(model.modelId) ?? false,
      runtime: runtimeForModel(model, input.runtimes),
      warm: null,
      slots: null,
      readiness: null,
      queue: null,
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
