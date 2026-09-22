import { and, eq, inArray, notInArray } from "drizzle-orm";
import { z } from "zod";
import type { LocalBaseDatabase } from "../../db/client";
import {
  authDomainRoleBindingsTable,
  authEmailRoleBindingsTable,
  authIdentitiesTable,
  authRolesTable,
  authRolePermissionsTable,
  authSettingsTable,
  authSubjectRoleBindingsTable,
  authUserEmailsTable,
  authUserRolesTable,
  authUsersTable,
} from "../../db/schema";
import { permissionsSchema, type Permission } from "./authorization";
import type { BrowserIdentity } from "./browser-identity";
import { BrowserAccessError } from "./errors";

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
type AccessControlBinding = AccessControlConfig["bindings"][number];

const settingsId = "default";

function bindingId(parts: readonly string[]): string {
  return new Bun.CryptoHasher("sha256")
    .update(JSON.stringify(parts))
    .digest("hex");
}

export function applyAccessControl(
  db: LocalBaseDatabase,
  input: AccessControlConfig,
): AccessControlConfig {
  const config = accessControlConfigSchema.parse(input);
  db.transaction(
    () => {
      const now = new Date().toISOString();
      const existing = new Map(
        db
          .select({ id: authRolesTable.id, name: authRolesTable.name })
          .from(authRolesTable)
          .all()
          .map((role) => [role.name, role.id]),
      );
      const roleIds = new Map<string, string>();
      for (const role of config.roles) {
        const id = existing.get(role.name) ?? crypto.randomUUID();
        roleIds.set(role.name, id);
        db.insert(authRolesTable)
          .values({
            id,
            name: role.name,
            description: role.description,
            createdAt: now,
            updatedAt: now,
          })
          .onConflictDoUpdate({
            target: authRolesTable.name,
            set: { description: role.description, updatedAt: now },
          })
          .run();
      }
      const removedRoleIds = [...existing.entries()]
        .filter(([name]) => !roleIds.has(name))
        .map(([, id]) => id);
      if (removedRoleIds.length) {
        const assignedRole = db
          .select({ name: authRolesTable.name })
          .from(authUserRolesTable)
          .innerJoin(
            authRolesTable,
            eq(authUserRolesTable.roleId, authRolesTable.id),
          )
          .where(inArray(authUserRolesTable.roleId, removedRoleIds))
          .limit(1)
          .get();
        if (assignedRole)
          throw new BrowserAccessError(
            "policy-conflict",
            `Cannot remove browser access role ${assignedRole.name} while it is assigned to a managed user.`,
          );
      }
      db.delete(authSubjectRoleBindingsTable).run();
      db.delete(authEmailRoleBindingsTable).run();
      db.delete(authDomainRoleBindingsTable).run();
      db.delete(authRolePermissionsTable).run();
      const retainedIds = [...roleIds.values()];
      if (retainedIds.length)
        db.delete(authRolesTable)
          .where(notInArray(authRolesTable.id, retainedIds))
          .run();
      for (const role of config.roles) {
        const roleId = roleIds.get(role.name);
        if (!roleId) throw new Error("Role identity was not created.");
        if (role.permissions.length)
          db.insert(authRolePermissionsTable)
            .values(
              role.permissions.map((permission) => ({ roleId, permission })),
            )
            .run();
      }
      for (const binding of config.bindings) {
        const roleId = roleIds.get(binding.role);
        if (!roleId) throw new Error("Role identity was not created.");
        switch (binding.kind) {
          case "subject":
            db.insert(authSubjectRoleBindingsTable)
              .values({
                id: bindingId([
                  binding.kind,
                  roleId,
                  binding.issuer,
                  binding.subject,
                ]),
                roleId,
                issuer: binding.issuer,
                subject: binding.subject,
              })
              .run();
            break;
          case "email":
            db.insert(authEmailRoleBindingsTable)
              .values({
                id: bindingId([binding.kind, roleId, binding.email]),
                roleId,
                email: binding.email,
              })
              .run();
            break;
          case "email-domain":
            db.insert(authDomainRoleBindingsTable)
              .values({
                id: bindingId([binding.kind, roleId, binding.domain]),
                roleId,
                domain: binding.domain,
              })
              .run();
            break;
        }
      }
      db.insert(authSettingsTable)
        .values({
          id: settingsId,
          defaultRoleId: config.defaultRole
            ? (roleIds.get(config.defaultRole) ?? null)
            : null,
          updatedAt: now,
        })
        .onConflictDoUpdate({
          target: authSettingsTable.id,
          set: {
            defaultRoleId: config.defaultRole
              ? (roleIds.get(config.defaultRole) ?? null)
              : null,
            updatedAt: now,
          },
        })
        .run();
    },
    { behavior: "immediate" },
  );
  return config;
}

export function loadAccessControl(
  db: LocalBaseDatabase,
): AccessControlConfig | null {
  const settings = db
    .select({ defaultRoleId: authSettingsTable.defaultRoleId })
    .from(authSettingsTable)
    .where(eq(authSettingsTable.id, settingsId))
    .get();
  if (!settings) return null;
  const roles = db
    .select()
    .from(authRolesTable)
    .all()
    .sort((left, right) => left.name.localeCompare(right.name));
  const permissions = db.select().from(authRolePermissionsTable).all();
  const names = new Map(roles.map((role) => [role.id, role.name]));
  const bindings = [
    ...db
      .select()
      .from(authSubjectRoleBindingsTable)
      .all()
      .map((binding): AccessControlBinding => ({
        kind: "subject",
        role: names.get(binding.roleId) ?? "",
        issuer: binding.issuer,
        subject: binding.subject,
      })),
    ...db
      .select()
      .from(authEmailRoleBindingsTable)
      .all()
      .map((binding): AccessControlBinding => ({
        kind: "email",
        role: names.get(binding.roleId) ?? "",
        email: binding.email,
      })),
    ...db
      .select()
      .from(authDomainRoleBindingsTable)
      .all()
      .map((binding): AccessControlBinding => ({
        kind: "email-domain",
        role: names.get(binding.roleId) ?? "",
        domain: binding.domain,
      })),
  ].sort((left, right) =>
    JSON.stringify(left).localeCompare(JSON.stringify(right)),
  );
  return accessControlConfigSchema.parse({
    roles: roles.map((role) => ({
      name: role.name,
      description: role.description,
      permissions: permissions
        .filter((permission) => permission.roleId === role.id)
        .map((permission) => permission.permission),
    })),
    bindings,
    defaultRole: settings.defaultRoleId
      ? (names.get(settings.defaultRoleId) ?? null)
      : null,
  });
}

export function accessControlRevision(
  policy: AccessControlConfig | null,
): string | null {
  if (!policy) return null;
  const parsed = accessControlConfigSchema.parse(policy);
  const compareStrings = (left: string, right: string) =>
    left < right ? -1 : left > right ? 1 : 0;
  const canonical = {
    roles: [...parsed.roles]
      .sort((left, right) => compareStrings(left.name, right.name))
      .map((role) => ({
        ...role,
        permissions: [...role.permissions].sort(compareStrings),
      })),
    bindings: [...parsed.bindings].sort((left, right) =>
      compareStrings(JSON.stringify(left), JSON.stringify(right)),
    ),
    defaultRole: parsed.defaultRole,
  };
  return new Bun.CryptoHasher("sha256")
    .update(JSON.stringify(canonical))
    .digest("hex");
}

export function clearAccessControl(db: LocalBaseDatabase): boolean {
  const configured = db
    .select({ id: authSettingsTable.id })
    .from(authSettingsTable)
    .where(eq(authSettingsTable.id, settingsId))
    .get();
  if (!configured) return false;
  db.transaction(
    () => {
      const managedUser = db
        .select({ id: authUsersTable.id })
        .from(authUsersTable)
        .where(notInArray(authUsersTable.status, ["removed"]))
        .limit(1)
        .get();
      if (managedUser)
        throw new BrowserAccessError(
          "policy-conflict",
          "Cannot clear the browser access policy while managed users exist.",
        );
      db.delete(authSubjectRoleBindingsTable).run();
      db.delete(authEmailRoleBindingsTable).run();
      db.delete(authDomainRoleBindingsTable).run();
      db.delete(authSettingsTable).run();
      db.delete(authRolesTable).run();
    },
    { behavior: "immediate" },
  );
  return true;
}

export function resolveAccessControl(
  db: LocalBaseDatabase,
  identity: BrowserIdentity,
  options: Readonly<{ claimManagedUser?: boolean }> = {},
): Readonly<{
  matchedRoles: readonly string[];
  permissions: readonly Permission[];
}> | null {
  return db.transaction(
    () => {
      const settings = db
        .select({ defaultRoleId: authSettingsTable.defaultRoleId })
        .from(authSettingsTable)
        .where(eq(authSettingsTable.id, settingsId))
        .get();
      if (!settings) return null;
      const email = identity.verifiedEmail?.toLowerCase();
      let storedIdentity = db
        .select({
          userId: authIdentitiesTable.userId,
          status: authUsersTable.status,
          email: authUserEmailsTable.email,
        })
        .from(authIdentitiesTable)
        .innerJoin(
          authUsersTable,
          eq(authIdentitiesTable.userId, authUsersTable.id),
        )
        .leftJoin(
          authUserEmailsTable,
          eq(authIdentitiesTable.userId, authUserEmailsTable.userId),
        )
        .where(
          and(
            eq(authIdentitiesTable.issuer, identity.issuer),
            eq(authIdentitiesTable.subject, identity.subject),
          ),
        )
        .get();
      if (!storedIdentity && email && options.claimManagedUser === true) {
        const invitedUser = db
          .select({ userId: authUserEmailsTable.userId })
          .from(authUserEmailsTable)
          .where(eq(authUserEmailsTable.email, email))
          .get();
        if (invitedUser) {
          const now = new Date().toISOString();
          db.insert(authIdentitiesTable)
            .values({
              id: crypto.randomUUID(),
              userId: invitedUser.userId,
              issuer: identity.issuer,
              subject: identity.subject,
              verifiedEmail: email,
              createdAt: now,
              lastSeenAt: now,
            })
            .onConflictDoNothing()
            .run();
          storedIdentity = db
            .select({
              userId: authIdentitiesTable.userId,
              status: authUsersTable.status,
              email: authUserEmailsTable.email,
            })
            .from(authIdentitiesTable)
            .innerJoin(
              authUsersTable,
              eq(authIdentitiesTable.userId, authUsersTable.id),
            )
            .leftJoin(
              authUserEmailsTable,
              eq(authIdentitiesTable.userId, authUserEmailsTable.userId),
            )
            .where(
              and(
                eq(authIdentitiesTable.issuer, identity.issuer),
                eq(authIdentitiesTable.subject, identity.subject),
              ),
            )
            .get();
          if (storedIdentity?.userId !== invitedUser.userId)
            return { matchedRoles: [], permissions: [] };
          if (storedIdentity.status === "pending") {
            db.update(authUsersTable)
              .set({ status: "active", updatedAt: now })
              .where(eq(authUsersTable.id, invitedUser.userId))
              .run();
            storedIdentity = { ...storedIdentity, status: "active" };
          }
        }
      }
      if (
        storedIdentity?.status === "pending" &&
        options.claimManagedUser === true &&
        email === storedIdentity.email
      ) {
        const now = new Date().toISOString();
        db.update(authUsersTable)
          .set({ status: "active", updatedAt: now })
          .where(eq(authUsersTable.id, storedIdentity.userId))
          .run();
        storedIdentity = { ...storedIdentity, status: "active" };
      }
      if (storedIdentity && storedIdentity.status !== "active")
        return { matchedRoles: [], permissions: [] };

      const roleIds = new Set<string>();
      if (settings.defaultRoleId) roleIds.add(settings.defaultRoleId);
      for (const binding of db
        .select({ roleId: authSubjectRoleBindingsTable.roleId })
        .from(authSubjectRoleBindingsTable)
        .where(
          and(
            eq(authSubjectRoleBindingsTable.issuer, identity.issuer),
            eq(authSubjectRoleBindingsTable.subject, identity.subject),
          ),
        )
        .all())
        roleIds.add(binding.roleId);
      if (email) {
        for (const binding of db
          .select({ roleId: authEmailRoleBindingsTable.roleId })
          .from(authEmailRoleBindingsTable)
          .where(eq(authEmailRoleBindingsTable.email, email))
          .all())
          roleIds.add(binding.roleId);
        const domain = email.split("@").at(-1);
        if (domain)
          for (const binding of db
            .select({ roleId: authDomainRoleBindingsTable.roleId })
            .from(authDomainRoleBindingsTable)
            .where(eq(authDomainRoleBindingsTable.domain, domain))
            .all())
            roleIds.add(binding.roleId);
      }
      if (storedIdentity)
        for (const assignment of db
          .select({ roleId: authUserRolesTable.roleId })
          .from(authUserRolesTable)
          .where(eq(authUserRolesTable.userId, storedIdentity.userId))
          .all())
          roleIds.add(assignment.roleId);
      if (!roleIds.size) return { matchedRoles: [], permissions: [] };
      const roles = db
        .select({ id: authRolesTable.id, name: authRolesTable.name })
        .from(authRolesTable)
        .where(inArray(authRolesTable.id, [...roleIds]))
        .all();
      const permissions = db
        .select({ permission: authRolePermissionsTable.permission })
        .from(authRolePermissionsTable)
        .where(inArray(authRolePermissionsTable.roleId, [...roleIds]))
        .all();
      return {
        matchedRoles: roles.map((role) => role.name).sort(),
        permissions: permissionsSchema.parse(
          permissions.map((permission) => permission.permission),
        ),
      };
    },
    { behavior: "immediate" },
  );
}
