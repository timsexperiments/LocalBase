import { eq, inArray } from "drizzle-orm";
import { z } from "zod";
import type { LocalBaseDatabase } from "../../db/client";
import {
  authRolesTable,
  authSettingsTable,
  authUserEmailsTable,
  authUserRolesTable,
  authUsersTable,
} from "../../db/schema";
import { roleNameSchema } from "./access-control";

const settingsId = "default";

export const managedUserStatusSchema = z.enum([
  "pending",
  "active",
  "disabled",
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

function requireAccessPolicy(db: LocalBaseDatabase): void {
  const settings = db
    .select({ id: authSettingsTable.id })
    .from(authSettingsTable)
    .where(eq(authSettingsTable.id, settingsId))
    .get();
  if (!settings) throw new Error("Configure a browser access policy first.");
}

function roleIds(
  db: LocalBaseDatabase,
  roles: readonly string[],
): Map<string, string> {
  const stored = roles.length
    ? db
        .select({ id: authRolesTable.id, name: authRolesTable.name })
        .from(authRolesTable)
        .where(inArray(authRolesTable.name, [...roles]))
        .all()
    : [];
  const ids = new Map(stored.map((role) => [role.name, role.id]));
  const missing = roles.filter((role) => !ids.has(role));
  if (missing.length)
    throw new Error(`Unknown browser access role: ${missing.join(", ")}.`);
  return ids;
}

function requiredRoleId(
  ids: ReadonlyMap<string, string>,
  role: string,
): string {
  const id = ids.get(role);
  if (!id) throw new Error(`Unknown browser access role: ${role}.`);
  return id;
}

function userByEmail(db: LocalBaseDatabase, email: string): ManagedUser {
  const user = db
    .select({
      id: authUsersTable.id,
      email: authUserEmailsTable.email,
      status: authUsersTable.status,
      createdAt: authUsersTable.createdAt,
      updatedAt: authUsersTable.updatedAt,
    })
    .from(authUserEmailsTable)
    .innerJoin(
      authUsersTable,
      eq(authUserEmailsTable.userId, authUsersTable.id),
    )
    .where(eq(authUserEmailsTable.email, email))
    .get();
  if (!user) throw new Error(`Managed user ${email} was not found.`);
  const roles = db
    .select({ name: authRolesTable.name })
    .from(authUserRolesTable)
    .innerJoin(authRolesTable, eq(authUserRolesTable.roleId, authRolesTable.id))
    .where(eq(authUserRolesTable.userId, user.id))
    .all()
    .map((role) => role.name)
    .sort();
  return managedUserSchema.parse({ ...user, roles });
}

export function listManagedUsers(
  db: LocalBaseDatabase,
): readonly ManagedUser[] {
  return db.transaction(() => {
    const emails = db
      .select({ email: authUserEmailsTable.email })
      .from(authUserEmailsTable)
      .all()
      .map((entry) => entry.email)
      .sort();
    return emails.map((email) => userByEmail(db, email));
  });
}

export function inviteManagedUser(
  db: LocalBaseDatabase,
  input: InviteManagedUserInput,
): ManagedUser {
  const parsed = inviteManagedUserInputSchema.parse(input);
  return db.transaction(
    () => {
      requireAccessPolicy(db);
      roleIds(db, parsed.roles);
      const existing = db
        .select({ userId: authUserEmailsTable.userId })
        .from(authUserEmailsTable)
        .where(eq(authUserEmailsTable.email, parsed.email))
        .get();
      if (existing)
        throw new Error(`Managed user ${parsed.email} already exists.`);
      const now = new Date().toISOString();
      const id = crypto.randomUUID();
      db.insert(authUsersTable)
        .values({ id, status: "pending", createdAt: now, updatedAt: now })
        .run();
      db.insert(authUserEmailsTable)
        .values({ userId: id, email: parsed.email })
        .run();
      const ids = roleIds(db, parsed.roles);
      db.insert(authUserRolesTable)
        .values(
          parsed.roles.map((role) => ({
            userId: id,
            roleId: requiredRoleId(ids, role),
          })),
        )
        .run();
      return userByEmail(db, parsed.email);
    },
    { behavior: "immediate" },
  );
}

export function replaceManagedUserRoles(
  db: LocalBaseDatabase,
  input: ReplaceManagedUserRolesInput,
): ManagedUser {
  const parsed = replaceManagedUserRolesInputSchema.parse(input);
  return db.transaction(
    () => {
      requireAccessPolicy(db);
      const user = userByEmail(db, parsed.email);
      const ids = roleIds(db, parsed.roles);
      db.delete(authUserRolesTable)
        .where(eq(authUserRolesTable.userId, user.id))
        .run();
      if (parsed.roles.length)
        db.insert(authUserRolesTable)
          .values(
            parsed.roles.map((role) => ({
              userId: user.id,
              roleId: requiredRoleId(ids, role),
            })),
          )
          .run();
      return userByEmail(db, parsed.email);
    },
    { behavior: "immediate" },
  );
}

function setManagedUserStatus(
  db: LocalBaseDatabase,
  input: ManagedUserEmailInput,
  status: z.infer<typeof managedUserStatusSchema>,
): ManagedUser {
  const parsed = managedUserEmailInputSchema.parse(input);
  return db.transaction(
    () => {
      const user = userByEmail(db, parsed.email);
      db.update(authUsersTable)
        .set({ status, updatedAt: new Date().toISOString() })
        .where(eq(authUsersTable.id, user.id))
        .run();
      return userByEmail(db, parsed.email);
    },
    { behavior: "immediate" },
  );
}

export function enableManagedUser(
  db: LocalBaseDatabase,
  input: ManagedUserEmailInput,
): ManagedUser {
  return setManagedUserStatus(db, input, "active");
}

export function disableManagedUser(
  db: LocalBaseDatabase,
  input: ManagedUserEmailInput,
): ManagedUser {
  return setManagedUserStatus(db, input, "disabled");
}

export function removeManagedUser(
  db: LocalBaseDatabase,
  input: ManagedUserEmailInput,
): ManagedUser {
  const parsed = managedUserEmailInputSchema.parse(input);
  return db.transaction(
    () => {
      const user = userByEmail(db, parsed.email);
      db.delete(authUsersTable).where(eq(authUsersTable.id, user.id)).run();
      return user;
    },
    { behavior: "immediate" },
  );
}
