import { z } from "zod";

export const openAIErrorSchema = z
  .object({
    message: z.string().min(1),
    type: z.string().min(1),
    param: z.string().nullable(),
    code: z.union([z.string(), z.number()]).nullable(),
  })
  .strict();

export const openAIErrorResponseSchema = z
  .object({ error: openAIErrorSchema })
  .strict();

export type OpenAIError = z.infer<typeof openAIErrorSchema>;
export type OpenAIErrorResponse = z.infer<typeof openAIErrorResponseSchema>;

/**
 * Accepts upstream errors that are OpenAI-shaped but not strictly canonical,
 * such as llama-server bodies that omit `param` or add diagnostic fields like
 * `n_prompt_tokens`, and normalizes them to the public error shape.
 */
export const upstreamOpenAIErrorResponseSchema = z
  .object({
    error: z.object({
      message: z.string().min(1),
      type: z.string().min(1),
      param: z.string().nullable().optional(),
      code: z.union([z.string(), z.number()]).nullable().optional(),
    }),
  })
  .transform(({ error }): OpenAIErrorResponse => ({
    error: {
      message: error.message,
      type: error.type,
      param: error.param ?? null,
      code: error.code ?? null,
    },
  }));
