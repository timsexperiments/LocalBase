import { expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { eq } from "drizzle-orm";
import { DatabaseSession } from "../../db/client";
import { authIdentitiesTable, authUsersTable } from "../../db/schema";
import {
  accessControlConfigSchema,
  applyAccessControl,
  resolveAccessControl,
} from "./access-control";
import {
  disableManagedUser,
  enableManagedUser,
  inviteManagedUser,
  listManagedUsers,
  removeManagedUser,
  replaceManagedUserRoles,
} from "./users";

async function withDatabase(
  run: (database: DatabaseSession, root: string) => void,
): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), "localbase-managed-users-"));
  const database = new DatabaseSession();
  try {
    run(database, root);
  } finally {
    database.close();
    await rm(root, { recursive: true, force: true });
  }
}

const policy = accessControlConfigSchema.parse({
  roles: [
    {
      name: "admin",
      description: "Administrators",
      permissions: ["access:manage", "models:manage"],
    },
    {
      name: "member",
      description: "Members",
      permissions: ["inference:chat", "models:read"],
    },
  ],
  bindings: [
    {
      kind: "email-domain" as const,
      role: "member",
      domain: "example.com",
    },
    {
      kind: "email" as const,
      role: "admin",
      email: "owner@example.com",
    },
  ],
  defaultRole: null,
});

test("provisions, activates, manages, and removes browser users by email", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    expect(() =>
      inviteManagedUser(db, { email: "person@example.com", roles: ["member"] }),
    ).toThrow("Configure a browser access policy first.");
    applyAccessControl(db, policy);

    expect(
      inviteManagedUser(db, {
        email: "Person@Example.com",
        roles: ["admin"],
      }),
    ).toMatchObject({ email: "person@example.com", status: "pending" });
    expect(() =>
      inviteManagedUser(db, {
        email: "person@example.com",
        roles: ["missing"],
      }),
    ).toThrow("Unknown browser access role: missing.");

    const identity = {
      issuer: "https://identity.example.com",
      subject: "person-subject",
      verifiedEmail: "PERSON@example.com",
    };
    expect(resolveAccessControl(db, identity)).toEqual({
      matchedRoles: ["member"],
      permissions: ["inference:chat", "models:read"],
    });
    expect(
      db.select({ status: authUsersTable.status }).from(authUsersTable).get(),
    ).toEqual({ status: "pending" });
    expect(db.select().from(authIdentitiesTable).all()).toHaveLength(0);
    expect(
      resolveAccessControl(db, identity, { claimManagedUser: true }),
    ).toEqual({
      matchedRoles: ["admin", "member"],
      permissions: [
        "inference:chat",
        "models:read",
        "models:manage",
        "access:manage",
      ],
    });
    expect(
      db.select({ status: authUsersTable.status }).from(authUsersTable).get(),
    ).toEqual({ status: "active" });
    expect(db.select().from(authIdentitiesTable).all()).toHaveLength(1);
    expect(resolveAccessControl(db, identity)?.matchedRoles).toEqual([
      "admin",
      "member",
    ]);
    expect(db.select().from(authIdentitiesTable).all()).toHaveLength(1);
    expect(
      resolveAccessControl(
        db,
        {
          issuer: "https://second.example.com",
          subject: "other-subject",
          verifiedEmail: "person@example.com",
        },
        { claimManagedUser: true },
      ),
    ).toEqual({
      matchedRoles: ["admin", "member"],
      permissions: [
        "inference:chat",
        "models:read",
        "models:manage",
        "access:manage",
      ],
    });
    expect(db.select().from(authIdentitiesTable).all()).toHaveLength(2);

    expect(
      replaceManagedUserRoles(db, {
        email: "person@example.com",
        roles: ["member"],
      }),
    ).toMatchObject({ roles: ["member"] });
    expect(
      disableManagedUser(db, { email: "person@example.com" }),
    ).toMatchObject({
      status: "disabled",
    });
    expect(resolveAccessControl(db, identity)).toEqual({
      matchedRoles: [],
      permissions: [],
    });
    expect(
      enableManagedUser(db, { email: "person@example.com" }),
    ).toMatchObject({
      status: "active",
    });
    expect(resolveAccessControl(db, identity)?.matchedRoles).toEqual([
      "member",
    ]);
    expect(listManagedUsers(db)).toMatchObject([
      { email: "person@example.com", status: "active", roles: ["member"] },
    ]);
    expect(
      removeManagedUser(db, { email: "person@example.com" }),
    ).toMatchObject({
      email: "person@example.com",
    });
    expect(listManagedUsers(db)).toEqual([]);
    expect(
      db
        .select()
        .from(authIdentitiesTable)
        .where(eq(authIdentitiesTable.subject, "person-subject"))
        .all(),
    ).toEqual([]);
  });
});

test("requires known roles when replacing a managed user's role set", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    applyAccessControl(db, policy);
    inviteManagedUser(db, { email: "person@example.com", roles: ["member"] });
    expect(() =>
      replaceManagedUserRoles(db, {
        email: "person@example.com",
        roles: ["missing"],
      }),
    ).toThrow("Unknown browser access role: missing.");
    expect(listManagedUsers(db)[0]?.roles).toEqual(["member"]);
  });
});

test("keeps policy roles that are assigned to managed users", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    applyAccessControl(db, policy);
    inviteManagedUser(db, { email: "person@example.com", roles: ["member"] });

    expect(() =>
      applyAccessControl(
        db,
        accessControlConfigSchema.parse({
          roles: [policy.roles[0]],
          bindings: [policy.bindings[1]],
          defaultRole: null,
        }),
      ),
    ).toThrow(
      "Cannot remove browser access role member while it is assigned to a managed user.",
    );
    expect(listManagedUsers(db)[0]?.roles).toEqual(["member"]);
  });
});
