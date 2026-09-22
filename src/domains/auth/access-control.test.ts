import { expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import {
  accessControlConfigSchema,
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
