import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { DatabaseSession, databasePath } from "../../db/client";
import {
  createApiKey,
  revokeApiKey,
  rotateApiKey,
  setApiKeyScopes,
} from "../../manager";
import { startGatewayFixture, TTS_MODEL } from "../../test/gateway-fixture";
import { minimalWav } from "../../test/media-fixtures";
import { defaultApiKeyScopes, type Permission } from "../auth/authorization";

const job = "/v1/videos/00000000-0000-4000-8000-000000000000";
const scopedRoutes: {
  path: string;
  method: string;
  permission: Permission;
  allowedStatus: number;
}[] = [
  {
    path: "/v1/models",
    method: "GET",
    permission: "models:read",
    allowedStatus: 200,
  },
  {
    path: "/_localbase/models",
    method: "GET",
    permission: "models:read",
    allowedStatus: 200,
  },
  {
    path: "/_localbase/models/not-a-model",
    method: "GET",
    permission: "models:read",
    allowedStatus: 404,
  },
  {
    path: "/v1/chat/completions",
    method: "POST",
    permission: "inference:chat",
    allowedStatus: 200,
  },
  {
    path: "/v1/embeddings",
    method: "POST",
    permission: "inference:embeddings",
    allowedStatus: 200,
  },
  {
    path: "/v1/images/generations",
    method: "POST",
    permission: "inference:image",
    allowedStatus: 200,
  },
  {
    path: "/v1/audio/speech",
    method: "POST",
    permission: "inference:speech",
    allowedStatus: 200,
  },
  {
    path: "/v1/audio/transcriptions",
    method: "POST",
    permission: "inference:transcription",
    allowedStatus: 200,
  },
  {
    path: "/v1/audio/translations",
    method: "POST",
    permission: "inference:transcription",
    allowedStatus: 200,
  },
  {
    path: "/v1/videos",
    method: "POST",
    permission: "inference:video",
    allowedStatus: 501,
  },
  {
    path: job,
    method: "GET",
    permission: "inference:video",
    allowedStatus: 404,
  },
  {
    path: job,
    method: "DELETE",
    permission: "inference:video",
    allowedStatus: 404,
  },
  {
    path: `${job}/content`,
    method: "GET",
    permission: "inference:video",
    allowedStatus: 404,
  },
  {
    path: `${job}/cancel`,
    method: "POST",
    permission: "inference:video",
    allowedStatus: 404,
  },
];

test(
  "enforces each stored scope independently, applies edits live, and leaves the environment key unrestricted",
  async () => {
    const environmentApiKey = "gateway-scope-test-environment";
    const gateway = await startGatewayFixture({
      auth: {},
      ttsEnabled: true,
      environmentApiKey,
    });
    const database = new DatabaseSession();
    try {
      const config = gateway.readConfig();
      const key = createApiKey(database, config, "scoped", undefined, []);
      const request = (route: (typeof scopedRoutes)[number], token: string) => {
        const headers = new Headers({ authorization: `Bearer ${token}` });
        let body: BodyInit | undefined;
        if (route.permission === "inference:transcription") {
          const form = new FormData();
          form.append(
            "file",
            new File([minimalWav], "fixture.wav", { type: "audio/wav" }),
          );
          body = form;
        } else if (route.method === "POST") {
          headers.set("content-type", "application/json");
          const payload =
            route.permission === "inference:speech"
              ? {
                  model: TTS_MODEL,
                  input: "Hello",
                  voice: "default",
                  response_format: "wav",
                }
              : route.permission === "inference:image"
                ? { prompt: "A paper kite" }
                : {
                    model: config.activeLlmModel,
                    input: "query",
                    messages: [{ role: "user", content: "hello" }],
                  };
          body = JSON.stringify(payload);
        }
        return fetch(`${gateway.baseUrl}${route.path}`, {
          method: route.method,
          headers,
          body,
        });
      };

      for (const scope of defaultApiKeyScopes) {
        setApiKeyScopes(database, config, key.record.id, [scope]);
        const upstreamBefore = gateway.upstreamRequests.length;
        for (const route of scopedRoutes.filter(
          (route) => route.permission !== scope,
        )) {
          const denied = await request(route, key.rawKey);
          expect(denied.status).toBe(403);
          expect(denied.headers.get("www-authenticate")).toBeNull();
          expect(await denied.json()).toMatchObject({
            error: { code: "insufficient_permissions" },
          });
        }
        expect(gateway.upstreamRequests).toHaveLength(upstreamBefore);
        for (const route of scopedRoutes.filter(
          (route) => route.permission === scope,
        )) {
          const allowed = await request(route, key.rawKey);
          expect(allowed.status).toBe(route.allowedStatus);
          await allowed.arrayBuffer();
        }
      }
      setApiKeyScopes(database, config, key.record.id, []);
      for (const route of scopedRoutes) {
        const denied = await request(route, key.rawKey);
        expect(denied.status).toBe(403);
        await denied.arrayBuffer();
        const allowed = await request(route, environmentApiKey);
        expect(allowed.status).toBe(route.allowedStatus);
        await allowed.arrayBuffer();
      }
    } finally {
      database.close();
      await gateway.stop();
    }
  },
  { timeout: 30_000 },
);

test("rotation, revocation, and expiry distinguish invalid credentials from insufficient scopes", async () => {
  const gateway = await startGatewayFixture({ auth: {} });
  const database = new DatabaseSession();
  try {
    const config = gateway.readConfig();
    const key = createApiKey(database, config, "reader", undefined, [
      "models:read",
    ]);
    const assertStatus = async (token: string | undefined, status: number) => {
      const response = await fetch(`${gateway.baseUrl}/v1/models`, {
        headers: token ? { authorization: `Bearer ${token}` } : {},
      });
      expect(response.status).toBe(status);
      if (status === 401) {
        expect(response.headers.get("www-authenticate")).toBe("Bearer");
        expect(await response.json()).toMatchObject({
          error: { code: "invalid_api_key" },
        });
      } else {
        await response.arrayBuffer();
      }
    };
    await assertStatus(undefined, 401);
    await assertStatus("unknown-credential", 401);
    await assertStatus(key.rawKey, 200);
    const rotated = rotateApiKey(database, config, key.record.id);
    await assertStatus(key.rawKey, 401);
    await assertStatus(rotated.rawKey, 200);
    setApiKeyScopes(database, config, key.record.id, []);
    await assertStatus(rotated.rawKey, 403);
    revokeApiKey(database, config, key.record.id);
    await assertStatus(rotated.rawKey, 401);
    setApiKeyScopes(database, config, key.record.id, ["models:read"]);
    await assertStatus(
      rotateApiKey(database, config, key.record.id).rawKey,
      401,
    );

    const expired = createApiKey(database, config, "expired", undefined, [
      "models:read",
    ]);
    const sqlite = new Database(databasePath(gateway.root));
    try {
      sqlite
        .prepare("UPDATE api_keys SET expires_at = ? WHERE id = ?")
        .run("2000-01-01T00:00:00.000Z", expired.record.id);
    } finally {
      sqlite.close();
    }
    await assertStatus(expired.rawKey, 401);
  } finally {
    database.close();
    await gateway.stop();
  }
});

test.each(["bearer", "x-api-key", "either"] as const)(
  "preserves stored and environment credentials in %s mode",
  async (mode) => {
    const environmentApiKey = "gateway-test-environment-credential";
    const gateway = await startGatewayFixture({
      auth: { mode },
      environmentApiKey,
    });
    try {
      if (!gateway.apiKey) throw new Error("Expected a stored API key.");
      for (const token of [gateway.apiKey, environmentApiKey]) {
        for (const header of ["authorization", "x-api-key"]) {
          const response = await fetch(`${gateway.baseUrl}/v1/models`, {
            headers: {
              [header]: header === "authorization" ? `Bearer ${token}` : token,
            },
          });
          const accepted =
            mode === "either" ||
            (mode === "bearer"
              ? header === "authorization"
              : header === "x-api-key");
          expect(response.status).toBe(accepted ? 200 : 401);
          await response.arrayBuffer();
        }
      }
      for (const path of ["/v1/models", "/unexposed", "/_localbase/models"]) {
        const response = await fetch(`${gateway.baseUrl}${path}`);
        expect(response.status).toBe(401);
        expect(response.headers.get("www-authenticate")).toBe("Bearer");
        expect(await response.json()).toMatchObject({
          error: { code: "invalid_api_key" },
        });
      }
    } finally {
      await gateway.stop();
    }
  },
);

test("video routes require a principal under --no-auth and keep preflight public", async () => {
  const gateway = await startGatewayFixture();
  const database = new DatabaseSession();
  try {
    const token = createApiKey(
      database,
      gateway.readConfig(),
      "video-test",
    ).rawKey;
    const restricted = createApiKey(
      database,
      gateway.readConfig(),
      "no-scopes",
      undefined,
      [],
    ).rawKey;
    const job = "/v1/videos/00000000-0000-4000-8000-000000000000";
    for (const [path, method] of [
      ["/v1/videos", "POST"],
      [job, "GET"],
      [job, "DELETE"],
      [`${job}/content`, "GET"],
      [`${job}/cancel`, "POST"],
    ]) {
      const missing = await fetch(`${gateway.baseUrl}${path}`, { method });
      expect(missing.status).toBe(401);
      await missing.arrayBuffer();
      const preflight = await fetch(`${gateway.baseUrl}${path}`, {
        method: "OPTIONS",
      });
      expect(preflight.status).toBe(204);
      const authenticated = await fetch(`${gateway.baseUrl}${path}`, {
        method,
        headers: { authorization: `Bearer ${token}` },
      });
      expect(authenticated.status).toBe(path === "/v1/videos" ? 501 : 404);
      await authenticated.arrayBuffer();
      const forbidden = await fetch(`${gateway.baseUrl}${path}`, {
        method,
        headers: { authorization: `Bearer ${restricted}` },
      });
      expect(forbidden.status).toBe(403);
      await forbidden.arrayBuffer();
    }
    const unknown = await fetch(`${gateway.baseUrl}/unexposed`);
    expect(unknown.status).toBe(404);
    await unknown.arrayBuffer();
    expect(await gateway.readImageRuntimeLaunches()).toEqual([]);
  } finally {
    database.close();
    await gateway.stop();
  }
});
