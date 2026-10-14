/**
 * Browser-safe access-control contract: the zod schemas shared by the server
 * and the web UI, kept free of database imports so the UI bundle stays lean.
 */
import { z } from "zod";
import { permissionsSchema } from "./authorization";

export const roleNameSchema = z
  .string()
  .min(1)
  .max(64)
  .regex(/^[a-z][a-z0-9-]*$/);

export const accessControlRoleSchema = z
  .object({
    name: roleNameSchema,
    description: z.string().max(256).default(""),
    permissions: permissionsSchema,
  })
  .strict();

const bindingSchema = z.discriminatedUnion("kind", [
  z
    .object({
      kind: z.literal("subject"),
      role: roleNameSchema,
      issuer: z.url().max(2_048),
      subject: z.string().min(1).max(512),
    })
    .strict(),
  z
    .object({
      kind: z.literal("email"),
      role: roleNameSchema,
      email: z
        .email()
        .max(320)
        .transform((value) => value.toLowerCase()),
    })
    .strict(),
  z
    .object({
      kind: z.literal("email-domain"),
      role: roleNameSchema,
      domain: z
        .string()
        .min(1)
        .max(253)
        .transform((value) => value.toLowerCase())
        .pipe(z.string().regex(/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/)),
    })
    .strict(),
]);

export const accessControlConfigSchema = z
  .object({
    roles: z.array(accessControlRoleSchema).min(1).max(64),
    bindings: z.array(bindingSchema).max(256),
    defaultRole: roleNameSchema.nullable().default(null),
  })
  .strict()
  .superRefine((config, context) => {
    const roles = new Map(config.roles.map((role) => [role.name, role]));
    if (roles.size !== config.roles.length)
      context.addIssue({
        code: "custom",
        path: ["roles"],
        message: "Role names must be unique.",
      });
    if (config.defaultRole && !roles.has(config.defaultRole))
      context.addIssue({
        code: "custom",
        path: ["defaultRole"],
        message: "Default role must exist.",
      });
    for (const [index, binding] of config.bindings.entries())
      if (!roles.has(binding.role))
        context.addIssue({
          code: "custom",
          path: ["bindings", index, "role"],
          message: `Unknown role: ${binding.role}`,
        });
    const bindings = new Set<string>();
    for (const [index, binding] of config.bindings.entries()) {
      const key = JSON.stringify(binding);
      if (bindings.has(key))
        context.addIssue({
          code: "custom",
          path: ["bindings", index],
          message: "Bindings must be unique.",
        });
      bindings.add(key);
    }
    const reachableRoles = new Set([
      ...(config.defaultRole ? [config.defaultRole] : []),
      ...config.bindings.map((binding) => binding.role),
    ]);
    if (
      ![...reachableRoles].some((name) =>
        roles.get(name)?.permissions.includes("access:manage"),
      )
    )
      context.addIssue({
        code: "custom",
        path: ["bindings"],
        message: "At least one reachable role must grant access:manage.",
      });
  });

export type AccessControlConfig = z.infer<typeof accessControlConfigSchema>;
