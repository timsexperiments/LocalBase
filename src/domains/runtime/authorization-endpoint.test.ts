import { expect, test } from "bun:test";
import { DatabaseSession } from "../../db/client";
import { createApiKey } from "../../manager";
import { startGatewayFixture } from "../../test/gateway-fixture";

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
