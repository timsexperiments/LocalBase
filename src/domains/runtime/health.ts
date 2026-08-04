import { z } from "zod";
import { LOCALBASE_VERSION } from "../../version";

export const modalityLifecycleStateSchema = z.enum([
  "disabled",
  "idle",
  "starting",
  "running",
  "draining",
  "stopping",
  "failed",
]);
export type ModalityLifecycleState = z.infer<
  typeof modalityLifecycleStateSchema
>;

export const gatewayModalityHealthSchema = z
  .object({
    configured: z.boolean(),
    state: modalityLifecycleStateSchema,
  })
  .strict();

const gatewayHealthBaseSchema = z
  .object({
    version: z.literal(LOCALBASE_VERSION),
    uptimeSeconds: z.number().int().nonnegative(),
    configurationRevision: z.number().int().nonnegative(),
    modalities: z
      .object({
        llm: gatewayModalityHealthSchema,
        stt: gatewayModalityHealthSchema,
        image: gatewayModalityHealthSchema,
      })
      .strict(),
  })
  .strict();

export const gatewayHealthSchema = z.discriminatedUnion("status", [
  gatewayHealthBaseSchema.extend({ status: z.literal("ok") }).strict(),
  gatewayHealthBaseSchema
    .extend({
      status: z.literal("error"),
      error: z.literal("gateway_stopping"),
    })
    .strict(),
]);
export type GatewayHealth = z.infer<typeof gatewayHealthSchema>;

export const gatewayIdentitySchema = z
  .object({
    instanceId: z.uuid(),
    rootHash: z.string(),
  })
  .strict();
