import { z } from "zod";
import { permissionsSchema } from "./authorization";
import {
  browserAccessConfigSchema,
  browserAccessConfigSummarySchema,
  cloudflareAccessProviderSchema,
  oidcAccessRegistrationSchema,
} from "./browser-access-contract";
import { browserAccessPolicySchema } from "./browser-policy";
import { apiKeyMetadataSchema } from "./api-key-public";

const identitySchema = z
  .object({
    issuer: z.string().url().max(2_048),
    subject: z.string().min(1).max(512),
    verifiedEmail: z.string().email().max(320).optional(),
  })
  .strict();

export const accessManagementRequestSchema = z.discriminatedUnion("action", [
  z
    .object({
      action: z.literal("configure-cloudflare"),
      provider: cloudflareAccessProviderSchema,
      origin: browserAccessConfigSchema.shape.origin,
      permissions: permissionsSchema,
    })
    .strict(),
  z
    .object({
      action: z.literal("upsert-oidc"),
      registration: oidcAccessRegistrationSchema,
      origin: browserAccessConfigSchema.shape.origin,
      permissions: permissionsSchema,
    })
    .strict(),
  z
    .object({
      action: z.literal("remove-oidc"),
      registrationId: oidcAccessRegistrationSchema.shape.id,
    })
    .strict(),
  z.object({ action: z.literal("disable") }).strict(),
  z
    .object({
      action: z.literal("apply-policy"),
      policy: browserAccessPolicySchema,
    })
    .strict(),
  z.object({ action: z.literal("clear-policy") }).strict(),
  z
    .object({ action: z.literal("test-policy"), identity: identitySchema })
    .strict(),
]);

const keyIdSchema = z.string().min(1).max(128);

export const keyManagementRequestSchema = z.discriminatedUnion("action", [
  z
    .object({
      action: z.literal("create"),
      name: z
        .string()
        .min(1)
        .max(128)
        .refine((value) => value.trim().length > 0),
      expiresDays: z.number().int().min(1).max(3_650).optional(),
      scopes: permissionsSchema,
    })
    .strict(),
  z.object({ action: z.literal("rotate"), keyId: keyIdSchema }).strict(),
  z.object({ action: z.literal("revoke"), keyId: keyIdSchema }).strict(),
  z
    .object({
      action: z.literal("set-scopes"),
      keyId: keyIdSchema,
      scopes: permissionsSchema,
    })
    .strict(),
]);

export const managementErrorSchema = z
  .object({
    error: z
      .object({
        code: z.enum([
          "invalid_api_key",
          "insufficient_permissions",
          "validation_failed",
          "payload_too_large",
          "request_aborted",
          "provider_not_configured",
          "registration_not_found",
          "key_not_found",
        ]),
        message: z.string().min(1).max(512),
      })
      .strict(),
  })
  .strict();

export const accessManagementReadResponseSchema = z
  .object({ config: browserAccessConfigSummarySchema.nullable() })
  .strict();

export const accessManagementMutationResponseSchema = z.union([
  z
    .object({
      config: browserAccessConfigSummarySchema,
      restartRequired: z.literal(true),
    })
    .strict(),
  z.object({ disabled: z.boolean(), restartRequired: z.boolean() }).strict(),
  z
    .object({
      removedRegistrationId: oidcAccessRegistrationSchema.shape.id,
      config: browserAccessConfigSummarySchema.nullable(),
      restartRequired: z.literal(true),
    })
    .strict(),
  z
    .object({
      policy: browserAccessPolicySchema,
      restartRequired: z.literal(true),
    })
    .strict(),
  z.object({ cleared: z.boolean(), restartRequired: z.boolean() }).strict(),
  z
    .object({
      policyConfigured: z.boolean(),
      matchedRoles: z.array(z.string()),
      permissions: permissionsSchema,
    })
    .strict(),
]);

export const keyManagementReadResponseSchema = z
  .object({ keys: z.array(apiKeyMetadataSchema) })
  .strict();

export const keyManagementSecretResponseSchema = z
  .object({ key: apiKeyMetadataSchema, secret: z.string().min(1) })
  .strict();

export const keyManagementMetadataResponseSchema = z
  .object({ key: apiKeyMetadataSchema })
  .strict();

export const keyManagementMutationResponseSchema = z.union([
  keyManagementSecretResponseSchema,
  keyManagementMetadataResponseSchema,
]);

export type AccessManagementRequest = z.infer<
  typeof accessManagementRequestSchema
>;
export type KeyManagementRequest = z.infer<typeof keyManagementRequestSchema>;
