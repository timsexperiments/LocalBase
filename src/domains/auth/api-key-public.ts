import { z } from "zod";
import type { ApiKeyRecord } from "../../manager";
import { permissionsSchema } from "./authorization";

export const apiKeyMetadataSchema = z
  .object({
    id: z.string(),
    name: z.string(),
    prefix: z.string(),
    scopes: permissionsSchema,
    createdAt: z.string(),
    lastRotatedAt: z.string(),
    expiresAt: z.string().optional(),
    revokedAt: z.string().optional(),
  })
  .strict();

export function publicApiKey(record: ApiKeyRecord) {
  return apiKeyMetadataSchema.parse({
    id: record.id,
    name: record.name,
    prefix: record.prefix,
    scopes: record.scopes,
    createdAt: record.createdAt,
    lastRotatedAt: record.lastRotatedAt,
    ...(record.expiresAt ? { expiresAt: record.expiresAt } : {}),
    ...(record.revokedAt ? { revokedAt: record.revokedAt } : {}),
  });
}
