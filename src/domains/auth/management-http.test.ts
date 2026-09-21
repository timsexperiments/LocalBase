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
  const stored = await loadBrowserAccessConfig(root);
  expect(
    stored?.provider.kind === "oidc"
      ? stored.provider.registrations.map(({ id }) => id)
      : [],
  ).toEqual(["primary", "secondary"]);

  const removed = await handle(
    request("/_localbase/access-management", {
      action: "remove-oidc",
      registrationId: "secondary",
    }),
    administrator,
  );
  expect(removed?.status).toBe(200);
  expect(await removed?.json()).toMatchObject({
    removedRegistrationId: "secondary",
    config: { provider: { registrations: [{ id: "primary" }] } },
  });

  const applied = await handle(
    request("/_localbase/access-management", {
      action: "apply-policy",
      policy: {
        roles: { admin: ["access:read", "access:manage"] },
        bindings: [
          {
            role: "admin",
            match: {
              kind: "subject",
              issuer: "https://identity.example.com",
              subject: "owner",
            },
          },
        ],
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
    roles: { admin: ["access:read", "access:manage"] },
    bindings: [
      {
        role: "admin",
        match: {
          kind: "email" as const,
          email: "owner@example.com",
        },
      },
    ],
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
    policy,
  });
});
