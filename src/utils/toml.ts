import { type LocalBaseConfig } from "../manager";
import { z } from "zod";
import { parallelSlotsSchema } from "../domains/config/parallel";
import { hostSchema, portSchema } from "../domains/config/schema";
import {
  modelIdSchema,
  selectedModelsSchema,
} from "../domains/models/model-selection";
import {
  otelEndpointSchema,
  otelHeadersTextSchema,
} from "../domains/observability/otel-config";
import { CliInputError, formatZodError } from "../domains/app/commands/errors";

export type ConfigOverrides = Partial<LocalBaseConfig>;

const configOverridesSchema = z
  .object({
    root: z.string().min(1).optional(),
    host: hostSchema.optional(),
    port: portSchema.optional(),
    ctxSize: z.number().int().positive().optional(),
    parallel: parallelSlotsSchema.optional(),
    sttHost: hostSchema.optional(),
    sttPort: portSchema.optional(),
    selectedLlmModels: selectedModelsSchema("llm", true).optional(),
    selectedSttModels: selectedModelsSchema("stt", false).optional(),
    selectedImageModels: selectedModelsSchema("image", false).optional(),
    activeLlmModel: modelIdSchema("llm").optional(),
    activeSttModel: z.union([z.literal(""), modelIdSchema("stt")]).optional(),
    activeImageModel: z
      .union([z.literal(""), modelIdSchema("image")])
      .optional(),
    hfToken: z.string().optional(),
    otelEndpoint: z.union([z.literal(""), otelEndpointSchema]).optional(),
    otelHeaders: otelHeadersTextSchema.optional(),
    otelSampleRatio: z.number().int().min(0).max(100).optional(),
  })
  .strict();

export async function loadTomlOverrides(
  path: string,
): Promise<ConfigOverrides> {
  const file = Bun.file(path);
  if (!(await file.exists())) throw new Error(`Config file not found: ${path}`);

  const raw = await file.text();
  let values: unknown;
  try {
    values = Bun.TOML.parse(raw);
  } catch (error) {
    throw new CliInputError(
      `Invalid TOML configuration: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  const parsed = configOverridesSchema.safeParse(values);
  if (!parsed.success) throw new CliInputError(formatZodError(parsed.error));
  return parsed.data;
}
