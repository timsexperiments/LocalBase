import { z } from "zod";
import {
  kokoroSpeechVoiceSchema,
  legacyFeatures,
  modelModalitySchema,
  referenceSpeechVoiceSchema,
  resolveCatalogInstallation,
  speechVoiceSchema,
  type ModelKind,
  type ModelSpec,
} from "../../catalog";
import {
  annotationSummaryFor,
  annotationSummarySchema,
} from "../../catalog/annotations";
import {
  capabilityMismatchSchema,
  capabilitySchema,
  capabilitySourceSchema,
  unsupportedCapabilitySchema,
} from "../../catalog/capabilities";
import {
  catalogProvenance,
  catalogTierSchema,
  catalogTrustSchema,
} from "../../catalog/sources";
import type { LocalBaseConfig } from "../../manager";
import { modalityLifecycleStateSchema } from "../runtime/health";
import type { RuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import type {
  HostMemorySnapshot,
  MemoryPool,
  MemoryTopology,
} from "../runtime/memory-safety";
import type { RuntimeModality } from "../runtime/modality";
import {
  evaluateModelFit,
  modelMemoryFigure,
  referenceLaunchShape,
  type ModelMemoryInputs,
} from "./memory-fit";
import {
  resolveModelCapabilities,
  type ResolvedCapabilities,
} from "./capability-detection";

const nullableNumberSchema = z.number().nullable();
const memorySourceSchema = z.enum(["estimate", "feed", "measured", "observed"]);
const nullableByteCountSchema = z.number().int().nonnegative().nullable();

const videoCapabilitiesSchema = z
  .object({
    kind: z.literal("video"),
    mode: z.enum(["t2v", "s2v", "ti2v"]),
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

/** Decision models answer typed questions from POST /v1/systemone only. */
const decisionCapabilitiesSchema = z
  .object({
    kind: z.literal("decision"),
  })
  .strict();

const speechCapabilitiesSchema = z
  .object({
    kind: z.literal("speech"),
    outputFormats: z.tuple([z.literal("wav")]),
    voice: z.discriminatedUnion("selection", [
      z
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
      z
        .object({
          selection: z.literal("bundled-native"),
          requestValues: z.array(kokoroSpeechVoiceSchema).min(1),
          defaultRequestValue: kokoroSpeechVoiceSchema,
        })
        .strict(),
    ]),
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
        displayName: z.string().min(1),
        /** Who publishes the weights, such as `Qwen`. */
        provider: z.string().min(1),
        /** Repository the artifacts download from. */
        source: z.string().min(1),
        revision: z.string().min(1),
        kind: z.enum(["llm", "stt", "tts", "image", "video"]),
        qualificationState: z.enum(["qualified", "experimental"]),
        /** How far LocalBase trusts this entry: qualified, community, user or modified. */
        trust: catalogTrustSchema,
        /** The highest catalog tier that contributed to the entry. */
        catalogTier: catalogTierSchema,
        /** The remote feed a community entry came from (origin and path only); null otherwise. */
        catalogFeed: z.string().min(1).nullable(),
        commercialStatus: z.enum(["open", "conditional", "prohibited"]),
        summary: z.string(),
        /** Longer catalog notes. */
        notes: z.string(),
        quantization: z.string().min(1),
        features: z.array(z.string()),
        artifacts: z
          .array(
            z
              .object({
                role: z.enum(["primary", "supplementary"]),
                filename: z.string().min(1),
                /** Repository and revision when it differs from the model source. */
                source: z
                  .object({
                    repositoryUrl: z.string().min(1),
                    revision: z.string().min(1),
                  })
                  .strict()
                  .nullable(),
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
            source: memorySourceSchema.optional(),
            demandBytes: z.number().int().nonnegative().optional(),
            launchShape: z.string().min(1).optional(),
            reference: z
              .object({
                source: z.enum(["estimate", "feed", "measured"]),
                bytes: z.number().int().nonnegative(),
                /** The launch shape the estimate or measurement describes. */
                launchShape: z.string().min(1),
              })
              .strict()
              .optional(),
            /** Every peak recorded for the installed files, newest first. */
            observedPeaks: z
              .array(
                z
                  .object({
                    peakBytes: z.number().int().nonnegative(),
                    launchShape: z.string().min(1),
                    method: z.enum(["footprint", "vmhwm"]),
                    observedAt: z.string().min(1),
                  })
                  .strict(),
              )
              .optional(),
            observed: z
              .object({
                peakBytes: z.number().int().nonnegative(),
                launchShape: z.string().min(1),
                method: z.enum(["footprint", "vmhwm"]),
                observedAt: z.string().min(1),
              })
              .strict()
              .nullable()
              .optional(),
            /**
             * The catalog-feed peak behind the reference figure: published for
             * exactly this file and revision at the planned launch shape.
             */
            feedPeak: z
              .object({
                peakBytes: z.number().int().nonnegative(),
                launchShape: z.string().min(1),
                variant: z.string().min(1),
                hardware: z.string().min(1),
                measuredAt: z.string().min(1),
              })
              .strict()
              .nullable()
              .optional(),
            fit: z
              .object({
                verdict: z.enum(["fits", "tight", "wont-fit"]),
                systemMemoryGb: z.number().nonnegative(),
                headroomGb: z.number(),
              })
              .strict()
              .optional(),
          })
          .strict(),
        capabilities: z
          .discriminatedUnion("kind", [
            embeddingCapabilitiesSchema,
            decisionCapabilitiesSchema,
            speechCapabilitiesSchema,
            videoCapabilitiesSchema,
          ])
          .nullable(),
        inputModalities: z.array(modelModalitySchema).min(1),
        outputModalities: z.array(modelModalitySchema).min(1),
        contextWindowTokens: z.number().int().positive().nullable(),
        maxOutputTokens: z.null(),
        /** Capabilities the catalog verified as unsupported, with the reason. */
        unsupportedCapabilities: z.array(unsupportedCapabilitySchema),
        /**
         * What the remote catalog feed publishes for this entry's repository
         * (status, unsupported marks, measured peaks, recipe); null without one.
         */
        feedAnnotation: annotationSummarySchema.nullable(),
        capabilityResolution: z
          .object({
            capabilities: z.array(
              z
                .object({
                  id: capabilitySchema,
                  source: capabilitySourceSchema,
                })
                .strict(),
            ),
            mismatches: z.array(capabilityMismatchSchema),
            inputModalities: z
              .object({
                values: z.array(modelModalitySchema).min(1),
                source: capabilitySourceSchema,
              })
              .strict(),
          })
          .strict(),
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
  /** Resolved capabilities per model; declared-only when absent. */
  capabilities?: ReadonlyMap<string, ResolvedCapabilities>;
  /** Live runtime snapshots; the runtime field is null when absent (CLI). */
  runtimes?: Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>>;
  /** Local geometry and observed peaks per model; catalog estimate when absent. */
  memory?: ReadonlyMap<string, ModelMemoryInputs>;
  /** This machine's memory in GiB; adds a fit verdict when present. */
  systemMemoryGb?: number;
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
  const runtime = runtimes?.[model.kind];
  if (!runtime || runtime.modelId !== model.modelId) return null;
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
/** The feed URL behind a remote entry, without query, fragment or credentials. */
function catalogFeedOf(modelId: string): string | null {
  const provenance = catalogProvenance(modelId);
  if (provenance.origin !== "remote" || !provenance.path) return null;
  try {
    const url = new URL(provenance.path);
    return `${url.origin}${url.pathname}`;
  } catch {
    return null;
  }
}

export function projectModelMetadata(
  model: ModelSpec,
  input: ModelMetadataProjectionInput,
): ModelMetadata {
  return modelMetadataSchema.parse({
    object: "localbase.model",
    id: model.modelId,
    catalog: {
      name: model.family,
      displayName: model.displayName ?? model.family,
      provider: model.provider,
      source: model.source,
      revision: model.repositoryRevision,
      kind: model.kind,
      qualificationState: model.qualificationState ?? "qualified",
      trust: catalogProvenance(model.modelId).trust,
      catalogTier: catalogProvenance(model.modelId).tier,
      catalogFeed: catalogFeedOf(model.modelId),
      commercialStatus: model.commercialStatus,
      summary: model.catch,
      notes: model.notes,
      quantization: model.quant,
      features: legacyFeatures(model),
      artifacts: model.artifacts.map((artifact) => ({
        role: artifact.role,
        filename: artifact.filename,
        source: artifact.source ?? null,
        sha256: artifact.sha256 ?? null,
        sizeBytes: artifact.expectedSizeBytes ?? null,
      })),
      memory: {
        storageEstimateGb: model.storageGb,
        ...memoryProjection(model, input),
      },
      capabilities:
        model.llmRuntime?.capability === "embedding-only"
          ? {
              kind: "embedding",
              dimensions: model.llmRuntime.dimensions,
            }
          : model.llmRuntime?.capability === "decision-only"
            ? { kind: "decision" }
            : model.kind === "tts" && model.ttsRuntime?.kind === "llama-tts"
              ? {
                  kind: "speech",
                  outputFormats: ["wav"],
                  voice: {
                    selection: "catalog-reference",
                    requestValues: [
                      "default",
                      ...model.ttsRuntime.referenceVoices.map(
                        ({ name }) => name,
                      ),
                    ],
                    defaultRequestValue: "default",
                    references: model.ttsRuntime.referenceVoices.map(
                      ({ name, license, provenanceUrl }) => ({
                        name,
                        license,
                        provenanceUrl,
                      }),
                    ),
                  },
                  residency: "cold-per-request",
                }
              : model.kind === "tts" && model.ttsRuntime?.kind === "kokoro-onnx"
                ? {
                    kind: "speech",
                    outputFormats: ["wav"],
                    voice: {
                      selection: "bundled-native",
                      requestValues: model.ttsRuntime.voices.map(
                        ({ name }) => name,
                      ),
                      defaultRequestValue: model.ttsRuntime.defaultVoice,
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
                      jobDeadlineMs:
                        model.videoRuntime.qualification.jobDeadlineMs,
                      outputFormats: ["mp4"],
                    }
                  : null,
      inputModalities: model.inputModalities,
      outputModalities: model.outputModalities,
      contextWindowTokens: model.contextWindowTokens,
      maxOutputTokens: null,
      unsupportedCapabilities: (model.unsupportedCapabilities ?? []).map(
        (entry) => ({ ...entry }),
      ),
      feedAnnotation: annotationSummaryFor(model),
      capabilityResolution: capabilityResolution(
        input.capabilities?.get(model.modelId) ??
          resolveModelCapabilities(model, {}),
      ),
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

export function directoryForModel(
  config: Pick<
    LocalBaseConfig,
    | "llmModelsDir"
    | "sttModelsDir"
    | "ttsModelsDir"
    | "imageModelsDir"
    | "videoModelsDir"
  >,
  kind: ModelKind,
): string {
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

function capabilityResolution(
  resolved: ResolvedCapabilities,
): ModelMetadata["catalog"]["capabilityResolution"] {
  return {
    capabilities: resolved.capabilities.map(({ id, source }) => ({
      id,
      source,
    })),
    mismatches: resolved.mismatches.map((mismatch) => ({ ...mismatch })),
    inputModalities: {
      values: [...resolved.inputModalities.values],
      source: resolved.inputModalities.source,
    },
  };
}

/** Computed minimum memory with its source, and the fit for this machine. */
function memoryProjection(
  model: ModelSpec,
  input: ModelMetadataProjectionInput,
) {
  const inputs = input.memory?.get(model.modelId);
  const figure = modelMemoryFigure(model, inputs);
  const fit =
    input.systemMemoryGb === undefined
      ? undefined
      : evaluateModelFit(model, input.systemMemoryGb, inputs);
  return {
    minimumVramEstimateGb: figure.minVramGb,
    source: figure.source,
    demandBytes: Math.round(figure.demandBytes),
    launchShape: figure.launchShape,
    reference: {
      source: figure.referenceSource,
      bytes: Math.round(figure.referenceBytes),
      launchShape:
        figure.feedPeak?.launchShape ?? referenceLaunchShape(model, inputs),
    },
    observedPeaks: (inputs?.observed ?? []).map(
      ({ peakBytes, launchShape, method, observedAt }) => ({
        peakBytes,
        launchShape,
        method,
        observedAt,
      }),
    ),
    observed: figure.observed
      ? {
          peakBytes: figure.observed.peakBytes,
          launchShape: figure.observed.launchShape,
          method: figure.observed.method,
          observedAt: figure.observed.observedAt,
        }
      : null,
    feedPeak: figure.feedPeak
      ? {
          peakBytes: figure.feedPeak.peakBytes,
          launchShape: figure.feedPeak.launchShape,
          variant: figure.feedPeak.variant,
          hardware: figure.feedPeak.hardware,
          measuredAt: figure.feedPeak.measuredAt,
        }
      : null,
    ...(fit
      ? {
          fit: {
            verdict: fit.verdict,
            systemMemoryGb: fit.systemVramGb,
            headroomGb: fit.headroomGb,
          },
        }
      : {}),
  };
}
