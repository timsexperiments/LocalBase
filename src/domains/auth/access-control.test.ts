import { expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { eq } from "drizzle-orm";
import { DatabaseSession } from "../../db/client";
import {
  authIdentitiesTable,
  authRolesTable,
  authRolePermissionsTable,
  authUserRolesTable,
  authUsersTable,
} from "../../db/schema";
import {
  accessControlConfigSchema,
  accessControlRevision,
  applyAccessControl,
  clearAccessControl,
  loadAccessControl,
  resolveAccessControl,
  type AccessControlConfig,
} from "./access-control";

const config: AccessControlConfig = accessControlConfigSchema.parse({
  roles: [
    {
      name: "admin",
      description: "LocalBase administrators",
      permissions: ["access:manage", "models:manage"],
    },
    {
      name: "member",
      description: "Organization members",
      permissions: ["inference:chat", "models:read"],
    },
    {
      name: "video-user",
      description: "Video generation users",
      permissions: ["inference:video", "models:read"],
    },
  ],
  bindings: [
    {
      kind: "email-domain",
      role: "member",
      domain: "example.com",
    },
    { kind: "email", role: "admin", email: "admin@example.com" },
    {
      kind: "subject",
      role: "video-user",
      issuer: "https://github.com",
      subject: "12345",
    },
  ],
  defaultRole: null,
});

test("policy revisions ignore storage ordering but detect semantic changes", () => {
  const reordered = {
    ...config,
    roles: [...config.roles].reverse().map((role) => ({
      ...role,
      permissions: [...role.permissions].reverse(),
    })),
    bindings: [...config.bindings].reverse(),
  };
  expect(accessControlRevision(reordered)).toBe(accessControlRevision(config));
  expect(
    accessControlRevision({
      ...config,
      roles: config.roles.map((role) =>
        role.name === "member" ? { ...role, description: "Changed" } : role,
      ),
    }),
  ).not.toBe(accessControlRevision(config));
});

test("policy revisions use a total ordering for Unicode subjects", () => {
  const composed = {
    kind: "subject" as const,
    role: "admin",
    issuer: "https://example.com",
    subject: "é",
  };
  const decomposed = { ...composed, subject: "e\u0301" };
  const policy = {
    ...config,
    bindings: [composed, decomposed],
  };

  expect(accessControlRevision(policy)).toBe(
    accessControlRevision({
      ...policy,
      bindings: [...policy.bindings].reverse(),
    }),
  );
});

async function withDatabase(
  run: (database: DatabaseSession, root: string) => void,
): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), "localbase-access-control-"));
  const database = new DatabaseSession();
  try {
    run(database, root);
  } finally {
    database.close();
    await rm(root, { recursive: true, force: true });
  }
}

test("resolves database roles from verified identity attributes", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    expect(
      resolveAccessControl(db, {
        issuer: "https://github.com",
        subject: "unknown",
      }),
    ).toBeNull();
    expect(applyAccessControl(db, config)).toEqual(config);
    expect(loadAccessControl(db)).toEqual(config);

    expect(
      resolveAccessControl(db, {
        issuer: "https://accounts.example.com",
        subject: "admin-subject",
        verifiedEmail: "ADMIN@example.com",
      }),
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
      resolveAccessControl(db, {
        issuer: "https://github.com",
        subject: "12345",
      }),
    ).toEqual({
      matchedRoles: ["video-user"],
      permissions: ["inference:video", "models:read"],
    });
    expect(
      resolveAccessControl(db, {
        issuer: "https://github.com",
        subject: "unmatched",
      }),
    ).toEqual({ matchedRoles: [], permissions: [] });
  });
});

test("uses a configurable default role and applies changes live", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    applyAccessControl(db, { ...config, defaultRole: "member" });
    const identity = {
      issuer: "https://github.com",
      subject: "unmatched",
    };
    expect(resolveAccessControl(db, identity)).toEqual({
      matchedRoles: ["member"],
      permissions: ["inference:chat", "models:read"],
    });

    applyAccessControl(db, {
      ...config,
      defaultRole: "video-user",
    });
    expect(resolveAccessControl(db, identity)).toEqual({
      matchedRoles: ["video-user"],
      permissions: ["inference:video", "models:read"],
    });
  });
});

test("grants assignments only to active stored users", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    applyAccessControl(db, config);
    const admin = db
      .select({ id: authRolesTable.id })
      .from(authRolesTable)
      .where(eq(authRolesTable.name, "admin"))
      .get();
    expect(admin).toBeDefined();
    if (!admin) return;
    const now = new Date().toISOString();
    db.insert(authUsersTable)
      .values({ id: "user", status: "active", createdAt: now, updatedAt: now })
      .run();
    db.insert(authIdentitiesTable)
      .values({
        id: "identity",
        userId: "user",
        issuer: "https://identity.example.com",
        subject: "assigned-user",
        createdAt: now,
        lastSeenAt: now,
      })
      .run();
    db.insert(authUserRolesTable)
      .values({ userId: "user", roleId: admin.id })
      .run();
    const identity = {
      issuer: "https://identity.example.com",
      subject: "assigned-user",
    };
    expect(resolveAccessControl(db, identity)?.matchedRoles).toEqual(["admin"]);

    db.update(authUsersTable)
      .set({ status: "disabled", updatedAt: new Date().toISOString() })
      .where(eq(authUsersTable.id, "user"))
      .run();
    expect(resolveAccessControl(db, identity)).toEqual({
      matchedRoles: [],
      permissions: [],
    });
    expect(() => clearAccessControl(db)).toThrow(
      "Cannot clear the browser access policy while managed users exist.",
    );
    expect(db.select().from(authUserRolesTable).all()).toHaveLength(1);
    db.delete(authUsersTable).where(eq(authUsersTable.id, "user")).run();
    expect(clearAccessControl(db)).toBe(true);
  });
});

test("applies configurations transactionally and clears them", async () => {
  await withDatabase((database, root) => {
    const db = database.get(root);
    applyAccessControl(db, config);
    expect(() =>
      applyAccessControl(db, {
        ...config,
        bindings: [config.bindings[1], config.bindings[1]],
      }),
    ).toThrow();
    expect(loadAccessControl(db)).toEqual(config);
    expect(clearAccessControl(db)).toBe(true);
    expect(clearAccessControl(db)).toBe(false);
    expect(loadAccessControl(db)).toBeNull();
    expect(db.select().from(authRolePermissionsTable).all()).toEqual([]);
  });
});

test("rejects unknown permissions, roles, defaults, and unreachable administrators", () => {
  for (const input of [
    { ...config, roles: [{ ...config.roles[0], permissions: ["unknown"] }] },
    {
      ...config,
      bindings: [{ kind: "email", role: "missing", email: "a@example.com" }],
    },
    { ...config, defaultRole: "missing" },
    {
      ...config,
      bindings: [config.bindings[1], config.bindings[1]],
    },
    {
      roles: [
        {
          name: "member",
          description: "Members",
          permissions: ["inference:chat"],
        },
      ],
      bindings: [],
      defaultRole: "member",
    },
  ])
    expect(accessControlConfigSchema.safeParse(input).success).toBe(false);
});
