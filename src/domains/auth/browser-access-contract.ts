import { z } from "zod";
import { defaultApiKeyScopes, permissionsSchema } from "./authorization";
import { browserAccessPolicySchema } from "./browser-policy";

export const defaultBrowserPermissions = permissionsSchema.parse([
  ...defaultApiKeyScopes,
  "models:manage",
]);

export const cloudflareAccessProviderSchema = z
  .object({
    kind: z.literal("cloudflare-access"),
    teamDomain: z
      .string()
      .regex(/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$/),
    audience: z
      .string()
      .min(1)
      .refine((value) => value.trim() === value),
  })
  .strict();

const oidcIssuerSchema = z
  .string()
  .url()
  .refine((value) => {
    try {
      const url = new URL(value);
      return (
        url.protocol === "https:" &&
        url.username === "" &&
        url.password === "" &&
        url.search === "" &&
        url.hash === ""
      );
    } catch {
      return false;
    }
  }, "Expected an HTTPS OIDC issuer without credentials, query, or fragment.");

const oidcClientAuthenticationSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("none") }).strict(),
  z
    .object({
      kind: z.literal("client-secret-basic"),
      clientSecret: z.string().min(1),
    })
    .strict(),
]);

export const accessRegistrationIdSchema = z
  .string()
  .min(1)
  .max(64)
  .regex(/^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/);

export const oidcAccessRegistrationSchema = z
  .object({
    kind: z.literal("oidc"),
    id: accessRegistrationIdSchema,
    name: z
      .string()
      .min(1)
      .max(64)
      .refine((value) => value.trim() === value),
    issuer: oidcIssuerSchema,
    clientId: z
      .string()
      .min(1)
      .refine((value) => value.trim() === value),
    clientAuthentication: oidcClientAuthenticationSchema,
  })
  .strict();

export type OidcAccessRegistration = z.infer<
  typeof oidcAccessRegistrationSchema
>;

export const githubAccessRegistrationSchema = z
  .object({
    kind: z.literal("github-oauth"),
    id: accessRegistrationIdSchema,
    name: z
      .string()
      .min(1)
      .max(64)
      .refine((value) => value.trim() === value),
    clientId: z
      .string()
      .min(1)
      .refine((value) => value.trim() === value),
    clientSecret: z.string().min(1),
  })
  .strict();

export type GithubAccessRegistration = z.infer<
  typeof githubAccessRegistrationSchema
>;

export const directAccessRegistrationSchema = z.discriminatedUnion("kind", [
  oidcAccessRegistrationSchema,
  githubAccessRegistrationSchema,
]);

export type DirectAccessRegistration = z.infer<
  typeof directAccessRegistrationSchema
>;

export const directAccessProviderSchema = z
  .object({
    kind: z.literal("direct"),
    registrations: z.array(directAccessRegistrationSchema).min(1).max(16),
  })
  .strict()
  .superRefine((provider, context) => {
    const ids = new Set<string>();
    for (const [index, registration] of provider.registrations.entries()) {
      if (ids.has(registration.id))
        context.addIssue({
          code: "custom",
          path: ["registrations", index, "id"],
          message: "Identity provider registration IDs must be unique.",
        });
      ids.add(registration.id);
    }
  });

export type DirectAccessProvider = z.infer<typeof directAccessProviderSchema>;

const browserAccessProviderSchema = z.discriminatedUnion("kind", [
  cloudflareAccessProviderSchema,
  directAccessProviderSchema,
]);

const exactHttpsOriginSchema = z
  .string()
  .url()
  .refine((value) => {
    try {
      const url = new URL(value);
      return url.protocol === "https:" && url.origin === value;
    } catch {
      return false;
    }
  }, "Expected an exact HTTPS origin without path, credentials, or trailing slash.");

export const browserAccessConfigSchema = z
  .object({
    provider: browserAccessProviderSchema,
    origin: exactHttpsOriginSchema,
    permissions: permissionsSchema,
    policy: browserAccessPolicySchema.optional(),
  })
  .strict();

export type BrowserAccessConfig = z.infer<typeof browserAccessConfigSchema>;

export const oidcAccessRegistrationSummarySchema = oidcAccessRegistrationSchema
  .omit({ clientAuthentication: true })
  .extend({
    clientAuthentication: z.enum(["none", "client-secret-basic"]),
  });

export type OidcAccessRegistrationSummary = z.infer<
  typeof oidcAccessRegistrationSummarySchema
>;

export const githubAccessRegistrationSummarySchema =
  githubAccessRegistrationSchema.omit({ clientSecret: true }).extend({
    clientAuthentication: z.literal("client-secret"),
  });

export type GithubAccessRegistrationSummary = z.infer<
  typeof githubAccessRegistrationSummarySchema
>;

export const directAccessRegistrationSummarySchema = z.discriminatedUnion(
  "kind",
  [oidcAccessRegistrationSummarySchema, githubAccessRegistrationSummarySchema],
);

const directAccessProviderSummarySchema = z
  .object({
    kind: z.literal("direct"),
    registrations: z
      .array(directAccessRegistrationSummarySchema)
      .min(1)
      .max(16),
  })
  .strict();

export const browserAccessConfigSummarySchema = z
  .object({
    provider: z.discriminatedUnion("kind", [
      cloudflareAccessProviderSchema,
      directAccessProviderSummarySchema,
    ]),
    origin: exactHttpsOriginSchema,
    permissions: permissionsSchema,
    policy: browserAccessPolicySchema.optional(),
  })
  .strict();

export type BrowserAccessConfigSummary = z.infer<
  typeof browserAccessConfigSummarySchema
>;

export function summarizeBrowserAccessConfig(
  config: BrowserAccessConfig,
): BrowserAccessConfigSummary {
  if (config.provider.kind === "cloudflare-access")
    return browserAccessConfigSummarySchema.parse(config);
  return browserAccessConfigSummarySchema.parse({
    ...config,
    provider: {
      kind: config.provider.kind,
      registrations: config.provider.registrations.map((registration) =>
        directAccessRegistrationSummarySchema.parse(
          registration.kind === "oidc"
            ? {
                kind: registration.kind,
                id: registration.id,
                name: registration.name,
                issuer: registration.issuer,
                clientId: registration.clientId,
                clientAuthentication: registration.clientAuthentication.kind,
              }
            : {
                kind: registration.kind,
                id: registration.id,
                name: registration.name,
                clientId: registration.clientId,
                clientAuthentication: "client-secret",
              },
        ),
      ),
    },
  });
}
