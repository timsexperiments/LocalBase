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

export const oidcAccessProviderSchema = z
  .object({
    kind: z.literal("oidc"),
    issuer: oidcIssuerSchema,
    clientId: z
      .string()
      .min(1)
      .refine((value) => value.trim() === value),
    clientAuthentication: oidcClientAuthenticationSchema,
  })
  .strict();

export type OidcAccessProvider = z.infer<typeof oidcAccessProviderSchema>;

const browserAccessProviderSchema = z.discriminatedUnion("kind", [
  cloudflareAccessProviderSchema,
  oidcAccessProviderSchema,
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

const oidcAccessProviderSummarySchema = oidcAccessProviderSchema
  .omit({ clientAuthentication: true })
  .extend({
    clientAuthentication: z.enum(["none", "client-secret-basic"]),
  });

export const browserAccessConfigSummarySchema = z
  .object({
    provider: z.discriminatedUnion("kind", [
      cloudflareAccessProviderSchema,
      oidcAccessProviderSummarySchema,
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
      issuer: config.provider.issuer,
      clientId: config.provider.clientId,
      clientAuthentication: config.provider.clientAuthentication.kind,
    },
  });
}
