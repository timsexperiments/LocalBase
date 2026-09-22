import { z } from "zod";
import { permissionsSchema } from "./authorization";
import {
  browserAccessConfigSchema,
  browserAccessConfigSummarySchema,
  cloudflareAccessProviderSchema,
  githubAccessRegistrationSchema,
  magicLinkAccessRegistrationSchema,
  oidcAccessRegistrationSchema,
  accessRegistrationIdSchema,
} from "./browser-access-contract";
import {
  accessControlConfigSchema,
  accessControlRoleSchema,
} from "./access-control";
import { apiKeyMetadataSchema } from "./api-key-public";
import {
  inviteManagedUserInputSchema,
  managedUserEmailInputSchema,
  managedUserSchema,
  replaceManagedUserRolesInputSchema,
} from "./users";

const identitySchema = z
  .object({
    issuer: z.string().url().max(2_048),
    subject: z.string().min(1).max(512),
    verifiedEmail: z.string().email().max(320).optional(),
  })
  .strict();

export const accessPolicyRevisionSchema = z.string().regex(/^[a-f0-9]{64}$/);

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
      action: z.literal("upsert-github"),
      registration: githubAccessRegistrationSchema,
      origin: browserAccessConfigSchema.shape.origin,
      permissions: permissionsSchema,
    })
    .strict(),
  z
    .object({
      action: z.literal("upsert-magic-link"),
      registration: magicLinkAccessRegistrationSchema,
      origin: browserAccessConfigSchema.shape.origin,
      permissions: permissionsSchema,
    })
    .strict(),
  z
    .object({
      action: z.literal("remove-registration"),
      registrationId: accessRegistrationIdSchema,
    })
    .strict(),
  z.object({ action: z.literal("disable") }).strict(),
  z
    .object({
      action: z.literal("invite-user"),
      ...inviteManagedUserInputSchema.shape,
    })
    .strict(),
  z
    .object({
      action: z.literal("replace-user-roles"),
      ...replaceManagedUserRolesInputSchema.shape,
    })
    .strict(),
  z
    .object({
      action: z.literal("enable-user"),
      ...managedUserEmailInputSchema.shape,
    })
    .strict(),
  z
    .object({
      action: z.literal("disable-user"),
      ...managedUserEmailInputSchema.shape,
    })
    .strict(),
  z
    .object({
      action: z.literal("remove-user"),
      ...managedUserEmailInputSchema.shape,
    })
    .strict(),
  z
    .object({
      action: z.literal("apply-policy"),
      policy: accessControlConfigSchema,
      expectedPolicyRevision: accessPolicyRevisionSchema.nullable(),
    })
    .strict(),
  z
    .object({
      action: z.literal("clear-policy"),
      expectedPolicyRevision: accessPolicyRevisionSchema,
    })
    .strict(),
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
          "email_delivery_not_configured",
          "policy_not_configured",
          "registration_not_found",
          "key_not_found",
          "managed_user_not_found",
          "managed_user_exists",
          "role_not_found",
          "policy_conflict",
          "policy_revision_conflict",
        ]),
        message: z.string().min(1).max(512),
      })
      .strict(),
  })
  .strict();

export const accessManagementReadResponseSchema = z
  .object({
    config: browserAccessConfigSummarySchema.nullable(),
    policy: accessControlConfigSchema.nullable(),
    policyRevision: accessPolicyRevisionSchema.nullable(),
    users: z.array(managedUserSchema),
    roles: z.array(accessControlRoleSchema).max(64),
  })
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
      removedRegistrationId: accessRegistrationIdSchema,
      config: browserAccessConfigSummarySchema.nullable(),
      restartRequired: z.literal(true),
    })
    .strict(),
  z
    .object({
      policy: accessControlConfigSchema,
      policyRevision: accessPolicyRevisionSchema,
      restartRequired: z.literal(false),
    })
    .strict(),
  z
    .object({
      cleared: z.boolean(),
      policyRevision: z.null(),
      restartRequired: z.boolean(),
    })
    .strict(),
  z
    .object({
      policyConfigured: z.boolean(),
      matchedRoles: z.array(z.string()),
      permissions: permissionsSchema,
    })
    .strict(),
  z
    .object({
      user: managedUserSchema,
      signInUrl: z.string().url(),
    })
    .strict(),
  z.object({ user: managedUserSchema }).strict(),
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
