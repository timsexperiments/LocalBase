/**
 * Browser-safe managed-user contract: the zod schemas shared by the server
 * and the web UI, kept free of database imports so the UI bundle stays lean.
 */
import { z } from "zod";
import { roleNameSchema } from "./access-control-contract";

export const managedUserStatusSchema = z.enum([
  "pending",
  "active",
  "disabled",
  "removed",
]);

export const managedUserEmailSchema = z
  .email()
  .max(320)
  .transform((value) => value.toLowerCase());

export const managedUserRolesSchema = z
  .array(roleNameSchema)
  .max(64)
  .superRefine((roles, context) => {
    if (new Set(roles).size !== roles.length)
      context.addIssue({
        code: "custom",
        message: "Role names must be unique.",
      });
  });

export const managedUserSchema = z
  .object({
    id: z.string().uuid(),
    email: managedUserEmailSchema,
    status: managedUserStatusSchema,
    roles: managedUserRolesSchema,
    createdAt: z.string().datetime(),
    updatedAt: z.string().datetime(),
  })
  .strict();

export type ManagedUser = z.infer<typeof managedUserSchema>;

export const inviteManagedUserInputSchema = z
  .object({
    email: managedUserEmailSchema,
    roles: managedUserRolesSchema.min(1),
  })
  .strict();

export type InviteManagedUserInput = z.infer<
  typeof inviteManagedUserInputSchema
>;

export const replaceManagedUserRolesInputSchema = z
  .object({
    email: managedUserEmailSchema,
    roles: managedUserRolesSchema,
  })
  .strict();

export type ReplaceManagedUserRolesInput = z.infer<
  typeof replaceManagedUserRolesInputSchema
>;

export const managedUserEmailInputSchema = z
  .object({ email: managedUserEmailSchema })
  .strict();

export type ManagedUserEmailInput = z.infer<typeof managedUserEmailInputSchema>;
