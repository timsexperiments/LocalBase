import { z } from "zod";
import { LOCALBASE_VERSION } from "../../version";

export const modalityLifecycleStateSchema = z.enum([
  "disabled",
  "idle",
  "starting",
  "running",
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

export const gatewayHealthSchema = z
  .object({
    status: z.enum(["ok", "error"]),
    version: z.literal(LOCALBASE_VERSION),
    uptimeSeconds: z.number().int().nonnegative(),
    modalities: z
      .object({
        llm: gatewayModalityHealthSchema,
        stt: gatewayModalityHealthSchema,
        image: gatewayModalityHealthSchema,
      })
      .strict(),
    error: z.enum(["gateway_stopping"]).optional(),
  })
  .strict();
export type GatewayHealth = z.infer<typeof gatewayHealthSchema>;

export const gatewayIdentitySchema = z
  .object({
    instanceId: z.uuid(),
    rootHash: z.string(),
  })
  .strict();
