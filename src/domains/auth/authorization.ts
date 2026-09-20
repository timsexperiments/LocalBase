import { z } from "zod";

export const permissionSchema = z.enum([
  "inference:chat",
  "inference:embeddings",
  "inference:image",
  "inference:video",
  "inference:speech",
  "inference:transcription",
  "models:read",
  "models:manage",
  "configuration:read",
  "configuration:manage",
  "keys:read",
  "keys:manage",
  "access:read",
  "access:manage",
  "sessions:read",
  "sessions:revoke",
  "system:read",
  "system:manage",
]);

export type Permission = z.infer<typeof permissionSchema>;

export const permissionsSchema = z
  .array(permissionSchema)
  .transform((permissions) =>
    permissionSchema.options.filter((permission) =>
      permissions.includes(permission),
    ),
  )
  .readonly();

export const defaultApiKeyScopes = permissionsSchema.parse([
  "inference:chat",
  "inference:embeddings",
  "inference:image",
  "inference:video",
  "inference:speech",
  "inference:transcription",
  "models:read",
]);

export const principalSchema = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("anonymous") }).readonly(),
  z
    .strictObject({
      kind: z.literal("api-key"),
      id: z.string().min(1).brand<"ApiKeyId">(),
      name: z.string().min(1),
      permissions: permissionsSchema,
    })
    .readonly(),
  z
    .strictObject({
      kind: z.literal("environment"),
      permissions: permissionsSchema,
    })
    .readonly(),
]);

export type Principal = z.infer<typeof principalSchema>;

export type AuthorizationRequirement =
  | Readonly<{ kind: "public" }>
  | Readonly<{ kind: "authenticated" }>
  | Readonly<{ kind: "permission"; permission: Permission }>;

export type AuthorizationDecision =
  | Readonly<{ kind: "public" }>
  | Readonly<{
      kind: "authorized";
      principal: Exclude<Principal, { kind: "anonymous" }>;
    }>
  | Readonly<{ kind: "unauthenticated" }>
  | Readonly<{ kind: "forbidden"; permission: Permission }>;

export function authorize({
  principal,
  requirement,
}: {
  principal: Principal;
  requirement: AuthorizationRequirement;
}): AuthorizationDecision {
  if (requirement.kind === "public") return { kind: "public" };
  if (principal.kind === "anonymous") return { kind: "unauthenticated" };
  switch (requirement.kind) {
    case "authenticated":
      return { kind: "authorized", principal };
    case "permission":
      return principal.permissions.includes(requirement.permission)
        ? { kind: "authorized", principal }
        : { kind: "forbidden", permission: requirement.permission };
    default: {
      const exhaustive: never = requirement;
      return exhaustive;
    }
  }
}

export function principalOwnerId(
  principal: Exclude<Principal, { kind: "anonymous" }>,
): string {
  switch (principal.kind) {
    case "api-key":
      return `api-key:${principal.id}`;
    case "environment":
      return "environment";
    default: {
      const exhaustive: never = principal;
      return exhaustive;
    }
  }
}
