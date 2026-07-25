import { eq } from "drizzle-orm";
import { z } from "zod";
import { byId } from "../../catalog";
import type { LocalBaseDatabase } from "../../db/client";
import { modelSystemPromptsTable } from "../../db/schema";

export const systemPromptTextSchema = z
  .string()
  .refine((value) => value.trim().length > 0, "cannot be empty");

export const canonicalLlmModelIdSchema = z
  .string()
  .min(1)
  .superRefine((modelId, ctx) => {
    const model = byId(modelId);
    if (!model) {
      ctx.addIssue({
        code: "custom",
        message: "must name a catalog model",
      });
    } else if (model.kind !== "llm") {
      ctx.addIssue({
        code: "custom",
        message: "must name a catalog LLM model",
      });
    }
  });

const modelSystemPromptRowSchema = z
  .object({
    modelId: canonicalLlmModelIdSchema,
    prompt: systemPromptTextSchema,
  })
  .strict();

export type ModelSystemPrompt = z.infer<typeof modelSystemPromptRowSchema>;

export function getModelSystemPrompt(
  database: LocalBaseDatabase,
  modelId: string,
): ModelSystemPrompt | undefined {
  const canonicalModelId = canonicalLlmModelIdSchema.parse(modelId);
  const row = database
    .select()
    .from(modelSystemPromptsTable)
    .where(eq(modelSystemPromptsTable.modelId, canonicalModelId))
    .get();

  return row === undefined ? undefined : modelSystemPromptRowSchema.parse(row);
}

export function saveModelSystemPrompt(
  database: LocalBaseDatabase,
  input: ModelSystemPrompt,
): void {
  const prompt = modelSystemPromptRowSchema.parse(input);
  database
    .insert(modelSystemPromptsTable)
    .values(prompt)
    .onConflictDoUpdate({
      target: modelSystemPromptsTable.modelId,
      set: { prompt: prompt.prompt },
    })
    .run();
}

export function deleteModelSystemPrompt(
  database: LocalBaseDatabase,
  modelId: string,
): void {
  const canonicalModelId = canonicalLlmModelIdSchema.parse(modelId);
  database
    .delete(modelSystemPromptsTable)
    .where(eq(modelSystemPromptsTable.modelId, canonicalModelId))
    .run();
}
