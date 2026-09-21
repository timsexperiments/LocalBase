import { z } from "zod";
import { parallelSlotsSchema } from "../../config/parallel";
import { hostSchema, portSchema } from "../../config/schema";
import { localBaseRootInputSchema } from "../../../utils/root";
import {
  otelEndpointSchema,
  otelHeadersTextSchema,
} from "../../observability/otel-config";
import { safeFilenameSchema } from "../../../utils/checksum";
import {
  defaultApiKeyScopes,
  permissionsSchema,
} from "../../auth/authorization";
import {
  browserAccessConfigSchema,
  cloudflareAccessProviderSchema,
  oidcAccessRegistrationSchema,
  oidcRegistrationIdSchema,
} from "../../auth/browser-access";

export const modelKindSchema = z.enum(["llm", "stt", "tts", "image", "video"]);

export const dataRootSchema = localBaseRootInputSchema;

const positiveInteger = (maximum = 2_147_483_647) =>
  z
    .string()
    .regex(/^\d+$/, "must be an integer")
    .transform(Number)
    .pipe(z.number().int().min(1).max(maximum));

const portInputSchema = z
  .string()
  .regex(/^\d+$/, "must be an integer")
  .transform(Number)
  .pipe(portSchema);

const modelListSchema = z.string().transform((value) =>
  value
    .split(",")
    .map((modelId) => modelId.trim())
    .filter(Boolean),
);

const nonEmptyModelListSchema = modelListSchema.pipe(
  z.array(z.string()).min(1, "must include at least one model ID"),
);

export const globalOptionsSchema = z
  .object({
    root: dataRootSchema.optional(),
    nonInteractive: z.boolean().default(false),
    json: z.boolean().default(false),
  })
  .transform(({ json, nonInteractive, root }) => ({
    root,
    json,
    nonInteractive: nonInteractive || json,
  }));

export type GlobalOptions = {
  root?: string;
  nonInteractive: boolean;
  json: boolean;
};

export const configureInputSchema = z.object({
  all: z.boolean().default(false),
  defaults: z.boolean().default(false),
  configPath: z.string().min(1).optional(),
  host: hostSchema.optional(),
  port: portInputSchema.optional(),
  ctxSize: positiveInteger().optional(),
  parallel: z
    .union([
      z.literal("auto"),
      z
        .string()
        .regex(/^[1-4]$/)
        .transform(Number),
    ])
    .pipe(parallelSlotsSchema)
    .optional(),
  sttHost: hostSchema.optional(),
  sttPort: portInputSchema.optional(),
  llmModels: nonEmptyModelListSchema.optional(),
  sttModels: modelListSchema.optional(),
  ttsModels: modelListSchema.optional(),
  imageModels: modelListSchema.optional(),
  videoModels: modelListSchema.optional(),
  activeLlm: z.string().min(1).optional(),
  activeStt: z.string().min(1).optional(),
  activeTts: z.string().min(1).optional(),
  activeImage: z.string().min(1).optional(),
  activeVideo: z.string().min(1).optional(),
  hfToken: z.string().optional(),
  otelEndpoint: z.union([z.literal(""), otelEndpointSchema]).optional(),
  otelHeaders: otelHeadersTextSchema.optional(),
  otelSampleRatio: positiveInteger(100)
    .or(z.literal("0").transform(Number))
    .optional(),
  createKey: z.boolean().optional(),
});

export type ConfigureInput = z.infer<typeof configureInputSchema>;

export const initInputSchema = z.object({});
export type InitInput = z.infer<typeof initInputSchema>;

export const catalogInputSchema = z.object({
  kind: modelKindSchema.optional(),
});
export type CatalogInput = z.infer<typeof catalogInputSchema>;

export const recommendInputSchema = z.object({
  kind: modelKindSchema.optional(),
  vram: positiveInteger().optional(),
});
export type RecommendInput = z.infer<typeof recommendInputSchema>;

export const installedInputSchema = z.object({
  kind: modelKindSchema.optional(),
});
export type InstalledInput = z.infer<typeof installedInputSchema>;

export const installInputSchema = z
  .object({
    all: z.boolean().default(false),
    modelId: z.string().min(1).optional(),
  })
  .superRefine(({ all, modelId }, ctx) => {
    if (all && modelId) {
      ctx.addIssue({
        code: "custom",
        path: ["modelId"],
        message: "cannot be used with --all",
      });
    }
    if (!all && !modelId) {
      ctx.addIssue({
        code: "custom",
        path: ["modelId"],
        message: "is required unless --all is provided",
      });
    }
  });
export type InstallInput = z.infer<typeof installInputSchema>;

export const serveInputSchema = z.object({
  host: hostSchema.optional(),
  port: portInputSchema.optional(),
  llm: z.boolean().optional(),
  stt: z.boolean().optional(),
  tts: z.boolean().optional(),
  image: z.boolean().optional(),
  video: z.boolean().optional(),
  llmHost: hostSchema.optional(),
  llmPort: portInputSchema.optional(),
  sttHost: hostSchema.optional(),
  sttPort: portInputSchema.optional(),
  imageHost: hostSchema.optional(),
  imagePort: portInputSchema.optional(),
  videoHost: hostSchema.optional(),
  videoPort: portInputSchema.optional(),
  ctxSize: positiveInteger().optional(),
  inferenceQueueCapacity: positiveInteger(10_000).optional(),
  inferenceQueueTimeoutMs: positiveInteger(600_000).optional(),
  sttPath: z.string().min(1).optional(),
  llmModelFile: safeFilenameSchema.optional(),
  sttModelFile: safeFilenameSchema.optional(),
  ttsModelFile: safeFilenameSchema.optional(),
  imageModelFile: safeFilenameSchema.optional(),
  auth: z.boolean().optional(),
  authMode: z.enum(["bearer", "x-api-key", "either"]).optional(),
  bypassMemoryCheck: z.boolean().default(false),
});
export type ServeInput = z.infer<typeof serveInputSchema>;

export const doctorInputSchema = z.object({});
export type DoctorInput = z.infer<typeof doctorInputSchema>;

export const serviceInputSchema = z.object({});
export type ServiceInput = z.infer<typeof serviceInputSchema>;

export const logsInputSchema = z.object({
  follow: z.boolean().default(false),
  limit: z.coerce.number().int().min(1).max(5_000).default(200),
  since: z.iso.datetime({ offset: true }).optional(),
  level: z.enum(["debug", "info", "warn", "error"]).optional(),
  runtime: z
    .enum(["gateway", "llm", "stt", "tts", "image", "video", "service", "cli"])
    .optional(),
  requestId: z
    .string()
    .min(1)
    .max(128)
    .regex(/^[A-Za-z0-9._:-]+$/)
    .optional(),
});
export type LogsInput = z.infer<typeof logsInputSchema>;

export const diagnosticsInputSchema = z
  .object({ output: z.string().min(1).max(4_096).optional() })
  .strict();
export type DiagnosticsInput = z.infer<typeof diagnosticsInputSchema>;

export const keysListInputSchema = z.object({});
export type KeysListInput = z.infer<typeof keysListInputSchema>;

const keyScopesInputSchema = z
  .string()
  .transform((value): unknown =>
    value.trim() === "" ? [] : value.split(",").map((scope) => scope.trim()),
  )
  .pipe(permissionsSchema);

export const keysCreateInputSchema = z.object({
  name: z.string().min(1).default("manual"),
  expiresDays: positiveInteger().optional(),
  scopes: keyScopesInputSchema.default(defaultApiKeyScopes),
});
export type KeysCreateInput = z.infer<typeof keysCreateInputSchema>;

export const keyIdInputSchema = z.object({
  keyId: z.string().min(1),
});
export type KeyIdInput = z.infer<typeof keyIdInputSchema>;

export const keysScopesInputSchema = keyIdInputSchema.extend({
  scopes: keyScopesInputSchema,
});
export type KeysScopesInput = z.infer<typeof keysScopesInputSchema>;

export const accessShowInputSchema = z.object({});
export type AccessShowInput = z.infer<typeof accessShowInputSchema>;

export const accessCloudflareInputSchema = z.object({
  teamDomain: cloudflareAccessProviderSchema.shape.teamDomain,
  audience: cloudflareAccessProviderSchema.shape.audience,
  origin: browserAccessConfigSchema.shape.origin,
  permissions: keyScopesInputSchema.optional(),
});
export type AccessCloudflareInput = z.infer<typeof accessCloudflareInputSchema>;

export const accessOidcAddInputSchema = z
  .object({
    id: oidcAccessRegistrationSchema.shape.id,
    name: oidcAccessRegistrationSchema.shape.name,
    issuer: oidcAccessRegistrationSchema.shape.issuer,
    clientId: oidcAccessRegistrationSchema.shape.clientId,
    clientSecretEnv: z
      .string()
      .regex(/^[A-Za-z_][A-Za-z0-9_]*$/)
      .optional(),
    publicClient: z.boolean().default(false),
    origin: browserAccessConfigSchema.shape.origin,
    permissions: keyScopesInputSchema.optional(),
  })
  .superRefine((input, context) => {
    if (input.publicClient === Boolean(input.clientSecretEnv)) {
      context.addIssue({
        code: "custom",
        message:
          "Choose exactly one of --public-client or --client-secret-env.",
      });
    }
  });
export type AccessOidcAddInput = z.infer<typeof accessOidcAddInputSchema>;

export const accessOidcListInputSchema = z.object({});
export type AccessOidcListInput = z.infer<typeof accessOidcListInputSchema>;

export const accessOidcRemoveInputSchema = z.object({
  id: oidcRegistrationIdSchema,
});
export type AccessOidcRemoveInput = z.infer<typeof accessOidcRemoveInputSchema>;

export const accessDisableInputSchema = z.object({});
export type AccessDisableInput = z.infer<typeof accessDisableInputSchema>;

export const accessPolicyShowInputSchema = z.object({});
export type AccessPolicyShowInput = z.infer<typeof accessPolicyShowInputSchema>;

export const accessPolicyApplyInputSchema = z.object({
  file: z.string().min(1),
});
export type AccessPolicyApplyInput = z.infer<
  typeof accessPolicyApplyInputSchema
>;

export const accessPolicyTestInputSchema = z.object({
  issuer: z.string().url().max(2_048),
  subject: z.string().min(1).max(512),
  email: z.string().email().max(320).optional(),
});
export type AccessPolicyTestInput = z.infer<typeof accessPolicyTestInputSchema>;

export const accessPolicyClearInputSchema = z.object({});
export type AccessPolicyClearInput = z.infer<
  typeof accessPolicyClearInputSchema
>;

export const resetInputSchema = z.object({
  yes: z.boolean().default(false),
});
export type ResetInput = z.infer<typeof resetInputSchema>;

export const uninstallInputSchema = z.object({
  yes: z.boolean().default(false),
});
export type UninstallInput = z.infer<typeof uninstallInputSchema>;
