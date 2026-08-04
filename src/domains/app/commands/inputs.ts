import { z } from "zod";
import { parallelSlotsSchema } from "../../config/parallel";
import { hostSchema, portSchema } from "../../config/schema";
import { localBaseRootInputSchema } from "../../../utils/root";
import {
  otelEndpointSchema,
  otelHeadersTextSchema,
} from "../../observability/otel-config";

export const modelKindSchema = z.enum(["llm", "stt", "image"]);

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
  imageModels: modelListSchema.optional(),
  activeLlm: z.string().min(1).optional(),
  activeStt: z.string().min(1).optional(),
  activeImage: z.string().min(1).optional(),
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
  image: z.boolean().optional(),
  llmHost: hostSchema.optional(),
  llmPort: portInputSchema.optional(),
  sttHost: hostSchema.optional(),
  sttPort: portInputSchema.optional(),
  imageHost: hostSchema.optional(),
  imagePort: portInputSchema.optional(),
  ctxSize: positiveInteger().optional(),
  sttPath: z.string().min(1).optional(),
  llmModelFile: z.string().min(1).optional(),
  sttModelFile: z.string().min(1).optional(),
  imageModelFile: z.string().min(1).optional(),
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
    .enum(["gateway", "llm", "stt", "image", "service", "cli"])
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

export const keysCreateInputSchema = z.object({
  name: z.string().min(1).default("manual"),
  expiresDays: positiveInteger().optional(),
});
export type KeysCreateInput = z.infer<typeof keysCreateInputSchema>;

export const keyIdInputSchema = z.object({
  keyId: z.string().min(1),
});
export type KeyIdInput = z.infer<typeof keyIdInputSchema>;

export const resetInputSchema = z.object({
  yes: z.boolean().default(false),
});
export type ResetInput = z.infer<typeof resetInputSchema>;

export const uninstallInputSchema = z.object({
  yes: z.boolean().default(false),
});
export type UninstallInput = z.infer<typeof uninstallInputSchema>;
