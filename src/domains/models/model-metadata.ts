import { z } from "zod";
import {
  modelModalitySchema,
  referenceSpeechVoiceSchema,
  resolveCatalogInstallation,
  speechVoiceSchema,
  type ModelKind,
  type ModelSpec,
} from "../../catalog";
import type { LocalBaseConfig } from "../../manager";
import { modalityLifecycleStateSchema } from "../runtime/health";
import type { RuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import type {
  HostMemorySnapshot,
  MemoryPool,
  MemoryTopology,
} from "../runtime/memory-safety";
import type { RuntimeModality } from "../runtime/modality";

const nullableNumberSchema = z.number().nullable();
const nullableByteCountSchema = z.number().int().nonnegative().nullable();

const videoCapabilitiesSchema = z
  .object({
    kind: z.literal("video"),
    mode: z.enum(["t2v", "s2v"]),
    width: z.number().int().positive(),
    height: z.number().int().positive(),
    frames: z.number().int().positive(),
    fps: z.number().int().positive(),
    jobDeadlineMs: z.number().int().positive(),
    outputFormats: z.tuple([z.literal("mp4")]),
  })
  .strict();

const embeddingCapabilitiesSchema = z
  .object({
    kind: z.literal("embedding"),
    dimensions: z
      .object({
        minimum: z.number().int().positive(),
        maximum: z.number().int().positive(),
      })
      .strict(),
  })
  .strict();

const speechCapabilitiesSchema = z
  .object({
    kind: z.literal("speech"),
    outputFormats: z.tuple([z.literal("wav")]),
    voice: z
      .object({
        selection: z.literal("catalog-reference"),
        requestValues: z.array(speechVoiceSchema).min(1),
        defaultRequestValue: z.literal("default"),
        references: z.array(
          z
            .object({
              name: referenceSpeechVoiceSchema,
              license: z.literal("CC0-1.0"),
              provenanceUrl: z.string().url(),
            })
            .strict(),
        ),
      })
      .strict(),
    residency: z.literal("cold-per-request"),
  })
  .strict();

export const modelMetadataSchema = z
  .object({
    object: z.literal("localbase.model"),
    id: z.string().min(1),
    catalog: z
      .object({
        name: z.string().min(1),
        revision: z.string().min(1),
        kind: z.enum(["llm", "stt", "tts", "image", "video"]),
        quantization: z.string().min(1),
        features: z.array(z.string()),
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
        capabilities: z
          .discriminatedUnion("kind", [
            embeddingCapabilitiesSchema,
            speechCapabilitiesSchema,
            videoCapabilitiesSchema,
          ])
          .nullable(),
        inputModalities: z.array(modelModalitySchema).min(1),
        outputModalities: z.array(modelModalitySchema).min(1),
        contextWindowTokens: z.number().int().positive().nullable(),
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
            executionSlots: z.number().int().positive().nullable(),
            activeAdmissions: z.number().int().nonnegative().nullable(),
            availableExecutionSlots: z.number().int().nonnegative().nullable(),
            immediateDispatchAvailable: z.boolean().nullable(),
            queuedRequests: z.number().int().nonnegative().nullable(),
            waitingCapacity: z.number().int().nonnegative().nullable(),
            availableWaitingCapacity: z.number().int().nonnegative().nullable(),
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
    host: z
      .object({
        memory: z
          .object({
            kind: z.enum(["unified", "discrete"]),
            system: z
              .object({
                capacityBytes: z.number().int().nonnegative(),
                availableBytes: nullableByteCountSchema,
              })
              .strict(),
            accelerators: z.array(
              z
                .object({
                  capacityBytes: z.number().int().nonnegative(),
                  availableBytes: nullableByteCountSchema,
                })
                .strict(),
            ),
          })
          .strict(),
      })
      .strict(),
    data: z.array(modelMetadataSchema),
  })
  .strict();
export type ModelMetadataList = z.infer<typeof modelMetadataListSchema>;

export type ModelInstallation = readonly [modelId: string, complete: boolean];

export type ModelMetadataProjectionInput = Readonly<{
  catalog: readonly ModelSpec[];
  config: Pick<
    LocalBaseConfig,
    | "selectedLlmModels"
    | "selectedSttModels"
    | "selectedTtsModels"
    | "selectedImageModels"
    | "selectedVideoModels"
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
    ...config.selectedTtsModels,
    ...config.selectedImageModels,
    ...config.selectedVideoModels,
  ]);
}

function availableWaitingCapacity(
  queue: RuntimeLifecycleSnapshot["queue"],
): number | null {
  if (queue === null) return null;
  if (!queue.accepting) return 0;
  return Math.max(0, queue.capacity - queue.waiting);
}

function runtimeForModel(
  model: ModelSpec,
  runtimes: ModelMetadataProjectionInput["runtimes"],
): ModelMetadata["device"]["runtime"] {
  const runtime = runtimes[model.kind];
  if (runtime.modelId !== model.modelId) return null;
  const queue = runtime.queue;
  return {
    configured: runtime.configured,
    state: runtime.state,
    executionSlots: runtime.execution.slots,
    activeAdmissions: runtime.execution.activeAdmissions,
    availableExecutionSlots: runtime.execution.available,
    immediateDispatchAvailable: runtime.execution.immediateDispatchAvailable,
    queuedRequests: queue?.waiting ?? null,
    waitingCapacity: queue?.capacity ?? null,
    availableWaitingCapacity: availableWaitingCapacity(queue),
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
      features: model.features,
      artifacts: model.artifacts.map((artifact) => ({
        role: artifact.role,
        sha256: artifact.sha256 ?? null,
        sizeBytes: artifact.expectedSizeBytes ?? null,
      })),
      memory: {
        minimumVramEstimateGb: model.minVramGb,
        storageEstimateGb: model.storageGb,
      },
      capabilities: model.llmRuntime
        ? {
            kind: "embedding",
            dimensions: model.llmRuntime.dimensions,
          }
        : model.kind === "tts"
          ? {
              kind: "speech",
              outputFormats: ["wav"],
              voice: {
                selection: "catalog-reference",
                requestValues: [
                  "default",
                  ...(model.ttsRuntime?.referenceVoices ?? []).map(
                    ({ name }) => name,
                  ),
                ],
                defaultRequestValue: "default",
                references: (model.ttsRuntime?.referenceVoices ?? []).map(
                  ({ name, license, provenanceUrl }) => ({
                    name,
                    license,
                    provenanceUrl,
                  }),
                ),
              },
              residency: "cold-per-request",
            }
          : model.videoRuntime
            ? {
                kind: "video",
                mode: model.videoRuntime.mode,
                width: model.videoRuntime.qualification.maxWidth,
                height: model.videoRuntime.qualification.maxHeight,
                frames: model.videoRuntime.qualification.maxFrames,
                fps: model.videoRuntime.qualification.fps,
                jobDeadlineMs: model.videoRuntime.qualification.jobDeadlineMs,
                outputFormats: ["mp4"],
              }
            : null,
      inputModalities: model.inputModalities,
      outputModalities: model.outputModalities,
      contextWindowTokens: model.contextWindowTokens,
      maxOutputTokens: null,
    },
    device: {
      selected: selectedModelIds(input.config).has(model.modelId),
      installed: input.installations.get(model.modelId) ?? false,
      runtime: runtimeForModel(model, input.runtimes),
    },
  } satisfies ModelMetadata);
}

export function projectModelMetadataList(
  input: ModelMetadataProjectionInput,
  hostMemory: Readonly<{
    topology: MemoryTopology;
    snapshot: HostMemorySnapshot;
  }>,
): ModelMetadataList {
  const pool = (memoryPool: MemoryPool) => {
    const snapshot = hostMemory.snapshot.pools.find(
      ({ poolId }) => poolId === memoryPool.id,
    );
    return {
      capacityBytes: memoryPool.capacityBytes,
      availableBytes:
        snapshot?.availability === "available" ? snapshot.availableBytes : null,
    };
  };
  return modelMetadataListSchema.parse({
    object: "list",
    host: {
      memory: {
        kind: hostMemory.topology.kind,
        system: pool(hostMemory.topology.system),
        accelerators:
          hostMemory.topology.kind === "discrete"
            ? hostMemory.topology.accelerators.map(pool)
            : [],
      },
    },
    data: input.catalog.map((model) => projectModelMetadata(model, input)),
  });
}

function directoryForModel(config: LocalBaseConfig, kind: ModelKind): string {
  if (kind === "llm") return config.llmModelsDir;
  if (kind === "stt") return config.sttModelsDir;
  if (kind === "tts") return config.ttsModelsDir;
  if (kind === "video") return config.videoModelsDir;
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
