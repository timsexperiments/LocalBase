import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { z } from "zod";
import { DatabaseSession } from "../../db/client";
import { defaultConfig } from "../../manager";
import { permissionSchema, principalSchema } from "./authorization";
import { createAuthManagement } from "./management-http";
import { loadBrowserAccessConfig } from "./browser-access";
import { loadAccessControl } from "./access-control";
import { withRootOperation } from "../service/ownership";
import {
  accessManagementMutationResponseSchema,
  accessManagementReadResponseSchema,
} from "./management-contract";

let root: string;
let database: DatabaseSession;

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "local-base-auth-management-"));
  database = new DatabaseSession();
});

afterEach(() => {
  database.close();
  rmSync(root, { recursive: true, force: true });
});

const administrator = principalSchema.parse({
  kind: "environment",
  permissions: permissionSchema.options,
});

function request(path: string, body?: unknown): Request {
  return new Request(`http://127.0.0.1${path}`, {
    method: body === undefined ? "GET" : "POST",
    headers:
      body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

async function jsonResponse(response: Response | null): Promise<unknown> {
  if (!response) throw new Error("Expected a management response.");
  return await response.json();
}

test("manages browser access without returning OIDC secrets", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  const configured = await handle(
    request("/_localbase/access-management", {
      action: "upsert-oidc",
      registration: {
        kind: "oidc",
        id: "primary",
        name: "Primary",
        issuer: "https://identity.example.com",
        clientId: "localbase",
        clientAuthentication: {
          kind: "client-secret-basic",
          clientSecret: "private-secret",
        },
      },
      origin: "https://localbase.example.com",
      permissions: ["access:read", "access:manage"],
    }),
    administrator,
  );
  expect(configured?.status).toBe(200);
  expect(await configured?.text()).not.toContain("private-secret");

  const second = await handle(
    request("/_localbase/access-management", {
      action: "upsert-oidc",
      registration: {
        kind: "oidc",
        id: "secondary",
        name: "Secondary",
        issuer: "https://login.example.org",
        clientId: "secondary-client",
        clientAuthentication: { kind: "none" },
      },
      origin: "https://localbase.example.com",
      permissions: ["access:read", "access:manage"],
    }),
    administrator,
  );
  expect(second?.status).toBe(200);
  const github = await handle(
    request("/_localbase/access-management", {
      action: "upsert-github",
      registration: {
        kind: "github-oauth",
        id: "github",
        name: "GitHub",
        clientId: "github-client",
        clientSecret: "github-secret",
      },
      origin: "https://localbase.example.com",
      permissions: ["access:read", "access:manage"],
    }),
    administrator,
  );
  expect(github?.status).toBe(200);
  expect(await github?.text()).not.toContain("github-secret");
  const stored = await loadBrowserAccessConfig(root);
  expect(
    stored?.provider.kind === "direct"
      ? stored.provider.registrations.map(({ id }) => id)
      : [],
  ).toEqual(["github", "primary", "secondary"]);

  const removed = await handle(
    request("/_localbase/access-management", {
      action: "remove-registration",
      registrationId: "secondary",
    }),
    administrator,
  );
  expect(removed?.status).toBe(200);
  expect(await removed?.json()).toMatchObject({
    removedRegistrationId: "secondary",
    config: {
      provider: {
        registrations: [
          { id: "github", kind: "github-oauth" },
          { id: "primary", kind: "oidc" },
        ],
      },
    },
  });

  const applied = await handle(
    request("/_localbase/access-management", {
      action: "apply-policy",
      policy: {
        roles: [
          {
            name: "admin",
            description: "Administrators",
            permissions: ["access:read", "access:manage"],
          },
        ],
        bindings: [
          {
            kind: "subject",
            role: "admin",
            issuer: "https://identity.example.com",
            subject: "owner",
          },
        ],
        defaultRole: null,
      },
    }),
    administrator,
  );
  expect(applied?.status).toBe(200);
  const tested = await handle(
    request("/_localbase/access-management", {
      action: "test-policy",
      identity: {
        issuer: "https://identity.example.com",
        subject: "owner",
      },
    }),
    administrator,
  );
  expect(await tested?.json()).toMatchObject({
    matchedRoles: ["admin"],
    permissions: ["access:read", "access:manage"],
  });
});

test("manages browser users through the access-management contract", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  const reader = principalSchema.parse({
    kind: "browser-session",
    ownerId: "browser:reader",
    permissions: ["access:read"],
  });
  const initial = accessManagementReadResponseSchema.parse(
    await jsonResponse(
      await handle(request("/_localbase/access-management"), reader),
    ),
  );
  expect(initial.users).toEqual([]);
  expect(initial.roles).toEqual([]);
  const missingProvider = await handle(
    request("/_localbase/access-management", {
      action: "invite-user",
      email: "person@example.com",
      roles: ["member"],
    }),
    administrator,
  );
  expect(missingProvider?.status).toBe(409);
  expect(await missingProvider?.json()).toMatchObject({
    error: { code: "provider_not_configured" },
  });
  expect(
    (
      await handle(
        request("/_localbase/access-management", {
          action: "invite-user",
          email: "person@example.com",
          roles: ["member"],
        }),
        reader,
      )
    )?.status,
  ).toBe(403);

  const configured = await handle(
    request("/_localbase/access-management", {
      action: "configure-cloudflare",
      provider: {
        kind: "cloudflare-access",
        teamDomain: "localbase.cloudflareaccess.com",
        audience: "localbase",
      },
      origin: "https://localbase.example.com",
      permissions: ["access:read", "access:manage"],
    }),
    administrator,
  );
  expect(configured?.status).toBe(200);

  const missingPolicy = await handle(
    request("/_localbase/access-management", {
      action: "invite-user",
      email: "person@example.com",
      roles: ["member"],
    }),
    administrator,
  );
  expect(missingPolicy?.status).toBe(409);
  expect(await missingPolicy?.json()).toMatchObject({
    error: { code: "policy_not_configured" },
  });

  const policy = {
    roles: [
      {
        name: "admin",
        description: "Administrators",
        permissions: ["access:read", "access:manage"] as const,
      },
      {
        name: "member",
        description: "Members",
        permissions: ["inference:chat"] as const,
      },
    ],
    bindings: [
      {
        kind: "subject" as const,
        role: "admin",
        issuer: "https://identity.example.com",
        subject: "owner",
      },
      {
        kind: "email" as const,
        role: "member",
        email: "person@example.com",
      },
    ],
    defaultRole: null,
  };
  expect(
    (
      await handle(
        request("/_localbase/access-management", {
          action: "apply-policy",
          policy,
        }),
        administrator,
      )
    )?.status,
  ).toBe(200);

  const invited = await handle(
    request("/_localbase/access-management", {
      action: "invite-user",
      email: "Person@Example.com",
      roles: ["member"],
    }),
    administrator,
  );
  expect(invited?.status).toBe(201);
  const invitedBody = accessManagementMutationResponseSchema.parse(
    await invited?.json(),
  );
  expect(invitedBody).toMatchObject({
    user: { email: "person@example.com", status: "pending", roles: ["member"] },
    signInUrl: "https://localbase.example.com/app",
  });

  const duplicate = await handle(
    request("/_localbase/access-management", {
      action: "invite-user",
      email: "person@example.com",
      roles: ["member"],
    }),
    administrator,
  );
  expect(duplicate?.status).toBe(409);
  expect(await duplicate?.json()).toMatchObject({
    error: { code: "managed_user_exists" },
  });

  const unknownRole = await handle(
    request("/_localbase/access-management", {
      action: "replace-user-roles",
      email: "person@example.com",
      roles: ["missing"],
    }),
    administrator,
  );
  expect(unknownRole?.status).toBe(400);
  expect(await unknownRole?.json()).toMatchObject({
    error: { code: "role_not_found" },
  });

  const unknownUser = await handle(
    request("/_localbase/access-management", {
      action: "disable-user",
      email: "missing@example.com",
    }),
    administrator,
  );
  expect(unknownUser?.status).toBe(404);
  expect(await unknownUser?.json()).toMatchObject({
    error: { code: "managed_user_not_found" },
  });

  const testedPending = await handle(
    request("/_localbase/access-management", {
      action: "test-policy",
      identity: {
        issuer: "https://identity.example.com",
        subject: "pending-user",
        verifiedEmail: "PERSON@example.com",
      },
    }),
    administrator,
  );
  expect(await testedPending?.json()).toMatchObject({
    matchedRoles: ["member"],
    permissions: ["inference:chat"],
  });
  expect(
    accessManagementReadResponseSchema.parse(
      await jsonResponse(
        await handle(request("/_localbase/access-management"), reader),
      ),
    ).users[0],
  ).toMatchObject({ status: "pending" });

  const assignedRoleConflict = await handle(
    request("/_localbase/access-management", {
      action: "apply-policy",
      policy: {
        roles: [policy.roles[0]],
        bindings: [policy.bindings[0]],
        defaultRole: null,
      },
    }),
    administrator,
  );
  expect(assignedRoleConflict?.status).toBe(409);
  expect(await assignedRoleConflict?.json()).toMatchObject({
    error: { code: "policy_conflict" },
  });

  const disabled = await handle(
    request("/_localbase/access-management", {
      action: "disable-user",
      email: "PERSON@example.com",
    }),
    administrator,
  );
  expect(disabled?.status).toBe(200);
  expect((await disabled?.json()).user.status).toBe("disabled");

  const enabled = await handle(
    request("/_localbase/access-management", {
      action: "enable-user",
      email: "person@example.com",
    }),
    administrator,
  );
  expect(enabled?.status).toBe(200);
  expect((await enabled?.json()).user.status).toBe("active");

  const replaced = await handle(
    request("/_localbase/access-management", {
      action: "replace-user-roles",
      email: "person@example.com",
      roles: ["admin"],
    }),
    administrator,
  );
  expect((await replaced?.json()).user.roles).toEqual(["admin"]);

  const missingUser = await handle(
    request("/_localbase/access-management", {
      action: "disable-user",
      email: "missing@example.com",
    }),
    administrator,
  );
  expect(missingUser?.status).toBe(404);
  expect(await missingUser?.json()).toMatchObject({
    error: { code: "managed_user_not_found" },
  });

  const removed = await handle(
    request("/_localbase/access-management", {
      action: "remove-user",
      email: "person@example.com",
    }),
    administrator,
  );
  expect(removed?.status).toBe(200);
  expect((await removed?.json()).user.email).toBe("person@example.com");
  expect(
    accessManagementReadResponseSchema.parse(
      await jsonResponse(
        await handle(request("/_localbase/access-management"), reader),
      ),
    ).users,
  ).toEqual([]);
});

test("keeps access-management reads available during unrelated root operations", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  const reader = principalSchema.parse({
    kind: "browser-session",
    ownerId: "browser:reader",
    permissions: ["access:read"],
  });
  let release!: () => void;
  let acquired!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  const ready = new Promise<void>((resolve) => {
    acquired = resolve;
  });
  const holding = withRootOperation(root, "install model", async () => {
    acquired();
    await blocked;
  });

  try {
    await ready;
    const response = await handle(
      request("/_localbase/access-management"),
      reader,
    );
    expect(response?.status).toBe(200);
    expect(
      accessManagementReadResponseSchema.parse(await response?.json()),
    ).toMatchObject({ users: [], roles: [] });
  } finally {
    release();
    await holding;
  }
});

test("keeps key secrets one-time and enforces separate read and manage scopes", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  const reader = principalSchema.parse({
    kind: "browser-session",
    ownerId: "browser:reader",
    permissions: ["keys:read"],
  });
  expect(
    (
      await handle(
        request("/_localbase/api-keys", {
          action: "create",
          name: "client",
          scopes: ["inference:chat"],
        }),
        reader,
      )
    )?.status,
  ).toBe(403);

  const created = await handle(
    request("/_localbase/api-keys", {
      action: "create",
      name: "client",
      scopes: ["inference:chat"],
    }),
    administrator,
  );
  const createdBody = z
    .object({
      key: z.object({ id: z.string() }).passthrough(),
      secret: z.string(),
    })
    .parse(await created?.json());
  expect(created?.status).toBe(201);
  expect(createdBody.secret).toMatch(/^lb_/);

  const listed = await handle(request("/_localbase/api-keys"), reader);
  const listedText = await listed?.text();
  expect(listed?.status).toBe(200);
  expect(listedText).not.toContain(createdBody.secret);
  expect(listedText).not.toContain("keyHash");
  expect(listedText).toContain(createdBody.key.id);
});

test("denies anonymous and malformed management requests", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  const anonymous = await handle(request("/_localbase/access-management"), {
    kind: "anonymous",
  });
  expect(anonymous?.status).toBe(401);
  expect(await anonymous?.json()).toEqual({
    error: {
      code: "invalid_api_key",
      message: "Authentication is required.",
    },
  });
  expect(
    (
      await handle(
        request("/_localbase/api-keys", { action: "unknown" }),
        administrator,
      )
    )?.status,
  ).toBe(400);
});

test("cancels a pending request body and rejects whitespace-only key names", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  let started!: () => void;
  const reading = new Promise<void>((resolve) => {
    started = resolve;
  });
  const controller = new AbortController();
  const pending = handle(
    new Request("http://127.0.0.1/_localbase/api-keys", {
      method: "POST",
      body: new ReadableStream({ pull: started }),
      signal: controller.signal,
    }),
    administrator,
  );
  await reading;
  controller.abort();
  expect((await pending)?.status).toBe(499);

  const invalidName = await handle(
    request("/_localbase/api-keys", {
      action: "create",
      name: "  ",
      scopes: ["inference:chat"],
    }),
    administrator,
  );
  expect(invalidName?.status).toBe(400);
});

test("serializes access mutations without losing provider or policy changes", async () => {
  const handle = createAuthManagement({
    root,
    database,
    configuration: () => defaultConfig(root),
  });
  const configure = (teamDomain: string) =>
    handle(
      request("/_localbase/access-management", {
        action: "configure-cloudflare",
        provider: {
          kind: "cloudflare-access",
          teamDomain,
          audience: "localbase",
        },
        origin: "https://localbase.example.com",
        permissions: ["access:read", "access:manage"],
      }),
      administrator,
    );
  expect((await configure("first.cloudflareaccess.com"))?.status).toBe(200);
  const policy = {
    roles: [
      {
        name: "admin",
        description: "Administrators",
        permissions: ["access:read", "access:manage"] as const,
      },
    ],
    bindings: [
      {
        kind: "email" as const,
        role: "admin",
        email: "owner@example.com",
      },
    ],
    defaultRole: null,
  };
  const [, applied] = await Promise.all([
    configure("second.cloudflareaccess.com"),
    handle(
      request("/_localbase/access-management", {
        action: "apply-policy",
        policy,
      }),
      administrator,
    ),
  ]);
  expect(applied?.status).toBe(200);
  expect(await loadBrowserAccessConfig(root)).toMatchObject({
    provider: { teamDomain: "second.cloudflareaccess.com" },
  });
  expect(loadAccessControl(database.get(root))).toEqual(policy);
});
