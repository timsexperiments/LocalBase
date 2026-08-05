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
