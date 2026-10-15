import { z } from "zod";
import type { LocalBaseConfig } from "../../../manager";
import { modelSpecObjectSchema, refineModelSpec } from "../../../catalog";
import {
  catalogTierSchema,
  catalogTrustSchema,
} from "../../../catalog/sources";
import {
  gatewayReadinessSchema,
  serviceStatusSchema,
} from "../../service/manager";
import { logEventSchema } from "../../observability/logging";
import type { OtelConfiguration } from "../../observability/otel";
import { sanitizedOtelEndpoint } from "../../observability/otel-config";
import { memorySafetyConfigSchema } from "../../runtime/memory-safety";
import { permissionsSchema } from "../../auth/authorization";
import { modelMetadataSchema } from "../../models/model-metadata";
import {
  browserAccessConfigSummarySchema,
  githubAccessRegistrationSummarySchema,
  magicLinkAccessRegistrationSchema,
  oidcAccessRegistrationSummarySchema,
} from "../../auth/browser-access";
import { emailDeliveryConfigSummarySchema } from "../../auth/email-delivery";
import { accessControlConfigSchema } from "../../auth/access-control";
import { apiKeyMetadataSchema } from "../../auth/api-key-public";
import { managedUserSchema } from "../../auth/users";

export { publicApiKey } from "../../auth/api-key-public";

export const configurationOutputSchema = z
  .object({
    root: z.string(),
    llmModelsDir: z.string(),
    sttModelsDir: z.string(),
    ttsModelsDir: z.string(),
    imageModelsDir: z.string(),
    videoModelsDir: z.string(),
    host: z.string(),
    port: z.number().int(),
    ctxSize: z.number().int(),
    ctxMode: z.enum(["auto", "explicit"]),
    sttHost: z.string(),
    sttPort: z.number().int(),
    selectedLlmModels: z.array(z.string()),
    selectedSttModels: z.array(z.string()),
    selectedTtsModels: z.array(z.string()),
    selectedImageModels: z.array(z.string()),
    selectedVideoModels: z.array(z.string()),
    activeLlmModel: z.string(),
    activeSttModel: z.string(),
    activeTtsModel: z.string(),
    activeImageModel: z.string(),
    activeVideoModel: z.string(),
    parallel: z.union([z.literal("auto"), z.number().int().min(1).max(4)]),
    memory: memorySafetyConfigSchema,
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

/** Catalog spec plus the legacy `features` list existing JSON clients read. */
export const modelOutputSchema = modelSpecObjectSchema
  .extend({
    features: z.array(z.string().min(1)),
    /** Computed minimum memory (GiB); see domains/models/memory-fit.ts. */
    minVramGb: z.number().int().positive(),
    minimumVramEstimateGb: z.number().int().positive(),
    memorySource: z.enum(["estimate", "measured", "observed"]),
    memoryLaunchShape: z.string().min(1),
    observedPeakBytes: z.number().int().positive().nullable(),
    fitVerdict: z.enum(["fits", "tight", "wont-fit"]),
    systemMemoryGb: z.number().nonnegative(),
    /** Catalog trust and tier (`models catalog` only). */
    trust: catalogTrustSchema.optional(),
    catalogTier: catalogTierSchema.optional(),
  })
  .strict()
  .superRefine(refineModelSpec);

export const apiKeyMetadataOutputSchema = apiKeyMetadataSchema;

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
export const diagnosticsResultSchema = z
  .object({
    archive: z
      .object({
        path: z.string().min(1),
        entries: z.number().int().nonnegative(),
        bytes: z.number().int().nonnegative(),
      })
      .strict(),
  })
  .strict();
export const catalogResultSchema = z
  .object({ models: z.array(modelOutputSchema) })
  .strict();
export const showResultSchema = z
  .object({
    model: modelMetadataSchema,
    status: z
      .object({
        installed: z.boolean(),
        enabled: z.boolean(),
        default: z.boolean(),
      })
      .strict(),
  })
  .strict();
export const recommendResultSchema = z
  .object({
    kind: z.enum(["llm", "stt", "tts", "image"]),
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
export const addResultSchema = z
  .object({
    modelId: z.string(),
    path: z.string(),
    repo: z.string(),
    revision: z.string(),
    license: z.string().nullable(),
    pipelineTag: z.string().nullable(),
    files: z.array(
      z
        .object({ path: z.string(), sizeBytes: z.number(), sha256: z.string() })
        .strict(),
    ),
    projector: z.string().nullable(),
    declaredCapabilities: z.array(z.string()),
    detectedCapabilities: z.array(z.string()),
    contextWindowTokens: z.number().nullable(),
    replaced: z.boolean(),
    installedPath: z.string().nullable(),
  })
  .strict();
export const searchResultSchema = z
  .object({
    query: z.string(),
    cached: z.boolean(),
    results: z.array(
      z
        .object({
          repo: z.string(),
          downloads: z.number(),
          likes: z.number().optional(),
          license: z.string().nullable(),
          pipelineTag: z.string().nullable(),
          likelyKind: z.string(),
          addable: z.boolean(),
        })
        .strict(),
    ),
  })
  .strict();
export const removeEntryResultSchema = z
  .object({
    modelId: z.string(),
    path: z.string(),
    restoredTier: z.enum(["built-in", "remote"]).nullable(),
    selected: z.boolean(),
  })
  .strict();
const catalogFeedStatusResultSchema = z
  .object({
    enabled: z.boolean(),
    url: z.string().nullable(),
    source: z.enum(["default", "settings", "environment"]),
    problem: z.string().nullable(),
    fetchedAt: z.string().nullable(),
    checkedAt: z.string().nullable(),
    revision: z.string().nullable(),
    stale: z.boolean(),
    lastError: z.string().nullable(),
    entries: z.number(),
    builtIn: z.number(),
    added: z.array(z.string()),
    skipped: z.number(),
  })
  .strict();
export const feedRefreshResultSchema = z
  .object({
    outcome: z.enum(["disabled", "fresh", "unchanged", "updated", "failed"]),
    error: z.string().nullable(),
    feed: catalogFeedStatusResultSchema,
  })
  .strict();
export const feedStatusResultSchema = z
  .object({ feed: catalogFeedStatusResultSchema })
  .strict();
export const keysListResultSchema = z
  .object({ keys: z.array(apiKeyMetadataOutputSchema) })
  .strict();
export const keySecretResultSchema = z
  .object({ key: apiKeyMetadataOutputSchema, secret: z.string().min(1) })
  .strict();
export const keyMetadataResultSchema = z
  .object({ key: apiKeyMetadataOutputSchema })
  .strict();
export const accessShowResultSchema = z
  .object({ config: browserAccessConfigSummarySchema.nullable() })
  .strict();
export const accessConfigureResultSchema = z
  .object({
    config: browserAccessConfigSummarySchema,
    restartRequired: z.literal(true),
  })
  .strict();
export const accessOidcListResultSchema = z
  .object({ registrations: z.array(oidcAccessRegistrationSummarySchema) })
  .strict();
export const accessOidcRemoveResultSchema = z
  .object({
    removed: z.literal(true),
    config: browserAccessConfigSummarySchema.nullable(),
    restartRequired: z.literal(true),
  })
  .strict();
export const accessGithubListResultSchema = z
  .object({ registrations: z.array(githubAccessRegistrationSummarySchema) })
  .strict();
export const accessGithubRemoveResultSchema = accessOidcRemoveResultSchema;
export const accessMagicLinkListResultSchema = z
  .object({ registrations: z.array(magicLinkAccessRegistrationSchema) })
  .strict();
export const accessMagicLinkRemoveResultSchema = accessOidcRemoveResultSchema;
export const accessDisableResultSchema = z
  .object({ disabled: z.boolean(), restartRequired: z.boolean() })
  .strict();
export const accessEmailShowResultSchema = z
  .object({ config: emailDeliveryConfigSummarySchema.nullable() })
  .strict();
export const accessEmailConfigureResultSchema = z
  .object({
    config: emailDeliveryConfigSummarySchema,
    restartRequired: z.literal(false),
  })
  .strict();
export const accessEmailDisableResultSchema = z
  .object({ disabled: z.boolean(), restartRequired: z.literal(false) })
  .strict();
export const accessEmailTestResultSchema = z
  .object({ delivered: z.literal(true), to: z.email() })
  .strict();
export const accessPolicyShowResultSchema = z
  .object({ policy: accessControlConfigSchema.nullable() })
  .strict();
export const accessPolicyApplyResultSchema = z
  .object({
    policy: accessControlConfigSchema,
    restartRequired: z.literal(false),
  })
  .strict();
export const accessPolicyTestResultSchema = z
  .object({
    policyConfigured: z.boolean(),
    matchedRoles: z.array(z.string()),
    permissions: permissionsSchema,
  })
  .strict();
export const accessPolicyClearResultSchema = z
  .object({ cleared: z.boolean(), restartRequired: z.boolean() })
  .strict();
export const accessUsersListResultSchema = z
  .object({ users: z.array(managedUserSchema) })
  .strict();
export const accessUsersInviteResultSchema = z
  .object({
    user: managedUserSchema,
    signInUrl: z.string().url(),
    emailDelivered: z.boolean(),
  })
  .strict();
export const accessUsersUserResultSchema = z
  .object({ user: managedUserSchema })
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
    ttsModelsDir: config.ttsModelsDir,
    imageModelsDir: config.imageModelsDir,
    videoModelsDir: config.videoModelsDir,
    host: config.host,
    port: config.port,
    ctxSize: config.ctxSize,
    ctxMode: config.ctxMode,
    sttHost: config.sttHost,
    sttPort: config.sttPort,
    selectedLlmModels: config.selectedLlmModels,
    selectedSttModels: config.selectedSttModels,
    selectedTtsModels: config.selectedTtsModels,
    selectedImageModels: config.selectedImageModels,
    selectedVideoModels: config.selectedVideoModels,
    activeLlmModel: config.activeLlmModel,
    activeSttModel: config.activeSttModel,
    activeTtsModel: config.activeTtsModel,
    activeImageModel: config.activeImageModel,
    activeVideoModel: config.activeVideoModel,
    parallel: config.parallel,
    memory: config.memory,
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
