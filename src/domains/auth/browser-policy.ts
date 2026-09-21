import { z } from "zod";
import { permissionsSchema, type Permission } from "./authorization";

const roleNameSchema = z
  .string()
  .min(1)
  .max(64)
  .regex(/^[a-z][a-z0-9-]*$/);

const exactValueSchema = z.string().min(1).max(512);
const exactEmailSchema = z.email().max(320);
const normalizedDomainSchema = z
  .string()
  .min(1)
  .max(253)
  .transform((value) => value.toLowerCase())
  .pipe(z.string().regex(/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/));

const bindingSchema = z
  .object({
    role: roleNameSchema,
    match: z.discriminatedUnion("kind", [
      z
        .object({
          kind: z.literal("subject"),
          issuer: z.url().max(2_048),
          subject: exactValueSchema,
        })
        .strict(),
      z.object({ kind: z.literal("email"), email: exactEmailSchema }).strict(),
      z
        .object({
          kind: z.literal("email-domain"),
          domain: normalizedDomainSchema,
        })
        .strict(),
    ]),
  })
  .strict();

export const browserAccessPolicySchema = z
  .object({
    roles: z
      .record(roleNameSchema, permissionsSchema)
      .refine(
        (roles) => Object.keys(roles).length <= 64,
        "Browser access policies support at most 64 roles.",
      ),
    bindings: z.array(bindingSchema).max(256).readonly(),
  })
  .strict()
  .superRefine((policy, context) => {
    for (const [index, binding] of policy.bindings.entries()) {
      if (!Object.hasOwn(policy.roles, binding.role))
        context.addIssue({
          code: "custom",
          path: ["bindings", index, "role"],
          message: `Unknown role: ${binding.role}`,
        });
    }
    if (
      !policy.bindings.some(
        (binding) =>
          Object.hasOwn(policy.roles, binding.role) &&
          policy.roles[binding.role]?.includes("access:manage"),
      )
    )
      context.addIssue({
        code: "custom",
        path: ["bindings"],
        message: "At least one binding must grant access:manage.",
      });
  });

export type BrowserAccessPolicy = z.infer<typeof browserAccessPolicySchema>;

export type BrowserIdentity = Readonly<{
  issuer: string;
  subject: string;
  verifiedEmail?: string;
}>;

function matches(
  identity: BrowserIdentity,
  binding: BrowserAccessPolicy["bindings"][number],
): boolean {
  switch (binding.match.kind) {
    case "subject":
      return (
        identity.issuer === binding.match.issuer &&
        identity.subject === binding.match.subject
      );
    case "email":
      return identity.verifiedEmail === binding.match.email;
    case "email-domain":
      return (
        identity.verifiedEmail?.split("@").at(-1)?.toLowerCase() ===
        binding.match.domain
      );
  }
}

export function evaluateBrowserAccessPolicy(
  policy: BrowserAccessPolicy,
  identity: BrowserIdentity,
): Readonly<{
  matchedRoles: readonly string[];
  permissions: readonly Permission[];
}> {
  const matchedRoles = [
    ...new Set(
      policy.bindings
        .filter((binding) => matches(identity, binding))
        .map((binding) => binding.role),
    ),
  ].sort();
  return Object.freeze({
    matchedRoles,
    permissions: permissionsSchema.parse(
      matchedRoles.flatMap((role) =>
        Object.hasOwn(policy.roles, role) ? (policy.roles[role] ?? []) : [],
      ),
    ),
  });
}
