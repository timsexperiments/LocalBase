import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { z } from "zod";
import { DatabaseSession } from "../../db/client";
import { defaultConfig } from "../../manager";
import { permissionSchema, principalSchema } from "./authorization";
import { createAuthManagement } from "./management-http";

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
      action: "configure-oidc",
      provider: {
        kind: "oidc",
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
  expect(
    (
      await handle(request("/_localbase/access-management"), {
        kind: "anonymous",
      })
    )?.status,
  ).toBe(401);
  expect(
    (
      await handle(
        request("/_localbase/api-keys", { action: "unknown" }),
        administrator,
      )
    )?.status,
  ).toBe(400);
});
