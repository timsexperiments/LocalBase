import { z } from "zod";
import type { ApiKeyRecord, LocalBaseConfig } from "../../../manager";
import { ModelSpecSchema } from "../../../catalog";
import {
  gatewayReadinessSchema,
  serviceStatusSchema,
} from "../../service/manager";
import { logEventSchema } from "../../observability/logging";
import type { OtelConfiguration } from "../../observability/otel";
import { sanitizedOtelEndpoint } from "../../observability/otel-config";

export const configurationOutputSchema = z
  .object({
    root: z.string(),
    llmModelsDir: z.string(),
    sttModelsDir: z.string(),
    imageModelsDir: z.string(),
    runtimeBackend: z.literal("llama.cpp"),
    sttBackend: z.literal("whisper.cpp"),
    host: z.string(),
    port: z.number().int(),
    ctxSize: z.number().int(),
    sttHost: z.string(),
    sttPort: z.number().int(),
    selectedLlmModels: z.array(z.string()),
    selectedSttModels: z.array(z.string()),
    selectedImageModels: z.array(z.string()),
    activeLlmModel: z.string(),
    activeSttModel: z.string(),
    activeImageModel: z.string(),
    parallel: z.union([z.literal("auto"), z.number().int().min(1).max(4)]),
    hfTokenConfigured: z.boolean(),
    observability: z
      .object({
        enabled: z.boolean(),
        source: z.enum(["disabled", "persistent", "environment"]),
        endpoint: z.string(),
        persistedEndpoint: z.string(),
        sampler: z.enum([
          "always_on",
          "always_off",
          "traceidratio",
          "parentbased_always_on",
          "parentbased_always_off",
          "parentbased_traceidratio",
        ]),
        sampleRatio: z.number().min(0).max(1),
      })
      .strict(),
  })
  .strict();

export const hardwareOutputSchema = z
  .object({
    osName: z.string(),
    ramGb: z.number(),
    cpuModel: z.string(),
    gpuName: z.string(),
    gpuVramGb: z.number(),
    isMac: z.boolean(),
    isAppleSilicon: z.boolean(),
  })
  .strict();

export const modelOutputSchema = ModelSpecSchema.strict();

export const apiKeyMetadataOutputSchema = z
  .object({
    id: z.string(),
    name: z.string(),
    prefix: z.string(),
    createdAt: z.string(),
    lastRotatedAt: z.string(),
    expiresAt: z.string().optional(),
    revokedAt: z.string().optional(),
  })
  .strict();

export const initResultSchema = z
  .object({ configuration: configurationOutputSchema })
  .strict();
export const configureResultSchema = z
  .object({
    configuration: configurationOutputSchema,
    createdKey: apiKeyMetadataOutputSchema
      .extend({ secret: z.string().min(1) })
      .optional(),
  })
  .strict();
export const doctorResultSchema = z
  .object({
    hardware: hardwareOutputSchema,
    configuration: configurationOutputSchema,
  })
  .strict();
export const gatewayReadinessOutputSchema = gatewayReadinessSchema;
export const serviceLifecycleResultSchema = z
  .object({
    service: serviceStatusSchema,
    gateway: gatewayReadinessOutputSchema,
  })
  .strict();
export const logsResultSchema = z
  .object({ events: z.array(logEventSchema) })
  .strict();
export const catalogResultSchema = z
  .object({ models: z.array(modelOutputSchema) })
  .strict();
export const recommendResultSchema = z
  .object({
    kind: z.enum(["llm", "stt", "image"]),
    vramGb: z.number(),
    models: z.array(modelOutputSchema),
  })
  .strict();
export const installedResultSchema = z
  .object({ models: z.array(z.string()) })
  .strict();
export const installResultSchema = z
  .object({
    installed: z.array(
      z.object({ modelId: z.string(), path: z.string() }).strict(),
    ),
  })
  .strict();
export const keysListResultSchema = z
  .object({ keys: z.array(apiKeyMetadataOutputSchema) })
  .strict();
export const keySecretResultSchema = z
  .object({ key: apiKeyMetadataOutputSchema, secret: z.string().min(1) })
  .strict();
export const keyRevocationResultSchema = z
  .object({ key: apiKeyMetadataOutputSchema })
  .strict();
export const resetResultSchema = z
  .object({ reset: z.literal(true), root: z.string() })
  .strict();
export const uninstallResultSchema = z
  .object({ removed: z.literal(true), root: z.string() })
  .strict();
export const serveResultSchema = z
  .object({ exitCode: z.number().int().min(0) })
  .strict();

/** Omits credentials from general-purpose command output. */
export function publicConfiguration(
  config: LocalBaseConfig,
  effective?: OtelConfiguration,
) {
  const enabled = effective?.enabled ?? Boolean(config.otelEndpoint);
  return configurationOutputSchema.parse({
    root: config.root,
    llmModelsDir: config.llmModelsDir,
    sttModelsDir: config.sttModelsDir,
    imageModelsDir: config.imageModelsDir,
    runtimeBackend: config.runtimeBackend,
    sttBackend: config.sttBackend,
    host: config.host,
    port: config.port,
    ctxSize: config.ctxSize,
    sttHost: config.sttHost,
    sttPort: config.sttPort,
    selectedLlmModels: config.selectedLlmModels,
    selectedSttModels: config.selectedSttModels,
    selectedImageModels: config.selectedImageModels,
    activeLlmModel: config.activeLlmModel,
    activeSttModel: config.activeSttModel,
    activeImageModel: config.activeImageModel,
    parallel: config.parallel,
    hfTokenConfigured: Boolean(config.hfToken),
    observability: {
      enabled,
      source: enabled ? (effective?.source ?? "persistent") : "disabled",
      endpoint:
        effective?.displayEndpoint ??
        sanitizedOtelEndpoint(config.otelEndpoint),
      persistedEndpoint: sanitizedOtelEndpoint(config.otelEndpoint),
      sampler: effective?.sampler ?? "parentbased_traceidratio",
      sampleRatio: effective?.sampleRatio ?? config.otelSampleRatio / 100,
    },
  });
}

/** API-key hashes are deliberately never part of command output. */
export function publicApiKey(record: ApiKeyRecord) {
  return apiKeyMetadataOutputSchema.parse({
    id: record.id,
    name: record.name,
    prefix: record.prefix,
    createdAt: record.createdAt,
    lastRotatedAt: record.lastRotatedAt,
    ...(record.expiresAt ? { expiresAt: record.expiresAt } : {}),
    ...(record.revokedAt ? { revokedAt: record.revokedAt } : {}),
  });
}
