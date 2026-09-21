import { expect, test } from "bun:test";
import { DatabaseSession } from "../../db/client";
import { createApiKey } from "../../manager";
import { startGatewayFixture } from "../../test/gateway-fixture";
import {
  keyManagementReadResponseSchema,
  keyManagementSecretResponseSchema,
} from "../auth/management-contract";

function headers(key: string): HeadersInit {
  return {
    authorization: `Bearer ${key}`,
    "content-type": "application/json",
  };
}

test("auth management endpoints enforce their scopes even when inference auth is disabled", async () => {
  const gateway = await startGatewayFixture();
  const database = new DatabaseSession();
  try {
    const config = gateway.readConfig();
    const reader = createApiKey(database, config, "auth-reader", undefined, [
      "access:read",
      "keys:read",
    ]);
    const manager = createApiKey(database, config, "auth-manager", undefined, [
      "access:read",
      "access:manage",
      "keys:read",
      "keys:manage",
    ]);
    const accessUrl = `${gateway.baseUrl}/_localbase/access-management`;
    const keysUrl = `${gateway.baseUrl}/_localbase/api-keys`;

    for (const url of [accessUrl, keysUrl]) {
      expect((await fetch(url)).status).toBe(401);
      expect(
        (
          await fetch(url, {
            headers: { authorization: "Bearer unknown" },
          })
        ).status,
      ).toBe(401);
      expect(
        (await fetch(url, { headers: headers(reader.rawKey) })).status,
      ).toBe(200);
    }

    const denied = await fetch(keysUrl, {
      method: "POST",
      headers: headers(reader.rawKey),
      body: JSON.stringify({
        action: "create",
        name: "client",
        scopes: ["inference:chat"],
      }),
    });
    expect(denied.status).toBe(403);

    const created = await fetch(keysUrl, {
      method: "POST",
      headers: headers(manager.rawKey),
      body: JSON.stringify({
        action: "create",
        name: "client",
        scopes: ["inference:chat"],
      }),
    });
    expect(created.status).toBe(201);
    const body = keyManagementSecretResponseSchema.parse(await created.json());
    expect(body).toMatchObject({
      key: { name: "client", scopes: ["inference:chat"] },
      secret: expect.stringMatching(/^lb_/),
    });

    const listed = await fetch(keysUrl, { headers: headers(reader.rawKey) });
    const listedText = await listed.text();
    const listedBody = keyManagementReadResponseSchema.parse(
      JSON.parse(listedText),
    );
    expect(listedText.includes(body.secret)).toBe(false);
    expect(listedText.includes("keyHash")).toBe(false);
    expect(listedBody.keys).toEqual(
      expect.arrayContaining([expect.objectContaining({ id: body.key.id })]),
    );

    const configured = await fetch(accessUrl, {
      method: "POST",
      headers: headers(manager.rawKey),
      body: JSON.stringify({
        action: "upsert-oidc",
        registration: {
          kind: "oidc",
          id: "primary",
          name: "Primary",
          issuer: "https://identity.example.com",
          clientId: "localbase",
          clientAuthentication: {
            kind: "client-secret-basic",
            clientSecret: "never-return-this",
          },
        },
        origin: "https://localbase.example.com",
        permissions: ["access:read"],
      }),
    });
    expect(configured.status).toBe(200);
    expect(await configured.text()).not.toContain("never-return-this");
  } finally {
    database.close();
    await gateway.stop();
  }
}, 30_000);
