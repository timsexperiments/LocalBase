import { chmod, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import {
  defaultApiKeyScopes,
  permissionSchema,
  permissionsSchema,
  type Permission,
} from "./authorization";

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

export const browserIdentityIssuerSchema = z
  .string()
  .max(2_048)
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
    issuer: browserIdentityIssuerSchema,
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

const roleNameSchema = z
  .string()
  .regex(/^[a-z][a-z0-9-]{0,62}$/, "Expected a lowercase role name.");

export const browserIdentitySubjectSchema = z
  .string()
  .min(1)
  .max(255)
  .regex(/^[\x00-\x7F]+$/, "Expected an ASCII identity subject.");

export const verifiedBrowserEmailSchema = z
  .string()
  .trim()
  .toLowerCase()
  .email()
  .max(320);

export const verifiedBrowserEmailDomainSchema = z
  .string()
  .trim()
  .toLowerCase()
  .max(253)
  .regex(
    /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/,
    "Expected a DNS email domain.",
  );

const browserAccessRolePermissionsSchema = z
  .record(
    roleNameSchema,
    z
      .array(permissionSchema)
      .max(permissionSchema.options.length)
      .transform((permissions) => permissionsSchema.parse(permissions)),
  )
  .superRefine((roles, context) => {
    if (Object.keys(roles).length > 64)
      context.addIssue({
        code: "custom",
        message: "A browser access policy supports at most 64 roles.",
      });
  })
  .transform((roles) =>
    Object.fromEntries(
      Object.entries(roles).sort(([left], [right]) =>
        left.localeCompare(right),
      ),
    ),
  );

const browserAccessBindingSchema = z.discriminatedUnion("kind", [
  z
    .object({
      kind: z.literal("subject"),
      role: roleNameSchema,
      issuer: browserIdentityIssuerSchema,
      subject: browserIdentitySubjectSchema,
    })
    .strict(),
  z
    .object({
      kind: z.literal("email"),
      role: roleNameSchema,
      email: verifiedBrowserEmailSchema,
    })
    .strict(),
  z
    .object({
      kind: z.literal("email-domain"),
      role: roleNameSchema,
      domain: verifiedBrowserEmailDomainSchema,
    })
    .strict(),
]);

export const browserAccessPolicySchema = z
  .object({
    roles: browserAccessRolePermissionsSchema,
    bindings: z
      .array(browserAccessBindingSchema)
      .max(256, "A browser access policy supports at most 256 bindings."),
  })
  .strict()
  .superRefine((policy, context) => {
    for (const [index, binding] of policy.bindings.entries()) {
      if (policy.roles[binding.role] === undefined)
        context.addIssue({
          code: "custom",
          path: ["bindings", index, "role"],
          message: `Unknown browser access role: ${binding.role}.`,
        });
    }
  })
  .transform((policy) => ({
    roles: policy.roles,
    bindings: [...policy.bindings].sort((left, right) =>
      JSON.stringify(left).localeCompare(JSON.stringify(right)),
    ),
  }));

export type BrowserAccessPolicy = z.infer<typeof browserAccessPolicySchema>;

export type BrowserIdentity = Readonly<{
  issuer: string;
  subject: string;
  email?: string;
}>;

export function browserAccessProviderIssuer(
  config: BrowserAccessConfig,
): string {
  return config.provider.kind === "cloudflare-access"
    ? `https://${config.provider.teamDomain}`
    : config.provider.issuer;
}

function normalizedIdentityEmail(email: string | undefined): string | null {
  if (!email) return null;
  const result = verifiedBrowserEmailSchema.safeParse(email);
  return result.success ? result.data : null;
}

export function evaluateBrowserAccessPolicy({
  config,
  identity,
}: {
  config: BrowserAccessConfig;
  identity: BrowserIdentity;
}): Readonly<{
  matchedRoles: readonly string[];
  permissions: readonly Permission[];
}> {
  if (identity.issuer !== browserAccessProviderIssuer(config))
    return { matchedRoles: [], permissions: [] };
  if (!config.policy)
    return { matchedRoles: [], permissions: config.permissions };
  const email = normalizedIdentityEmail(identity.email);
  const matchedRoles = [
    ...new Set(
      config.policy.bindings.flatMap((binding) => {
        switch (binding.kind) {
          case "subject":
            return binding.issuer === identity.issuer &&
              binding.subject === identity.subject
              ? [binding.role]
              : [];
          case "email":
            return binding.email === email ? [binding.role] : [];
          case "email-domain":
            return email?.endsWith(`@${binding.domain}`) ? [binding.role] : [];
          default: {
            const exhaustive: never = binding;
            return exhaustive;
          }
        }
      }),
    ),
  ].sort((left, right) => left.localeCompare(right));
  return {
    matchedRoles,
    permissions: permissionsSchema.parse(
      matchedRoles.flatMap((role) => config.policy?.roles[role] ?? []),
    ),
  };
}

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
  .omit({
    clientAuthentication: true,
  })
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

export function browserAccessConfigPath(root: string): string {
  return join(root, "ui-access.json");
}

export async function loadBrowserAccessConfig(
  root: string,
): Promise<BrowserAccessConfig | null> {
  let contents: string;
  try {
    contents = await readFile(browserAccessConfigPath(root), "utf8");
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return null;
    throw new Error("Unable to read browser access configuration.");
  }
  try {
    return browserAccessConfigSchema.parse(JSON.parse(contents));
  } catch {
    throw new Error(
      "Invalid ui-access.json. Configure browser access again with the LocalBase CLI.",
    );
  }
}

export async function saveBrowserAccessConfig(
  root: string,
  input: BrowserAccessConfig,
): Promise<BrowserAccessConfig> {
  const config = browserAccessConfigSchema.parse(input);
  const path = browserAccessConfigPath(root);
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  try {
    await writeFile(temporary, `${JSON.stringify(config, null, 2)}\n`, {
      encoding: "utf8",
      flag: "wx",
      mode: 0o600,
    });
    await rename(temporary, path);
    await chmod(path, 0o600);
  } finally {
    await unlink(temporary).catch((error: unknown) => {
      if (!(
        error instanceof Error &&
        "code" in error &&
        error.code === "ENOENT"
      ))
        throw error;
    });
  }
  return config;
}

export async function disableBrowserAccess(root: string): Promise<boolean> {
  try {
    await unlink(browserAccessConfigPath(root));
    return true;
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return false;
    throw new Error("Unable to disable browser access.");
  }
}
