import { expect, test } from "bun:test";
import { DatabaseSession } from "../../db/client";
import { resolveApiKey, revokeApiKey } from "../../manager";
import { startGatewayFixture, VIDEO_MODEL } from "../../test/gateway-fixture";

test("rejects an unqualified video request before zero-byte fixture artifacts can launch", async () => {
  const gateway = await startGatewayFixture({
    auth: {},
    videoEnabled: true,
  });
  try {
    if (!gateway.apiKey) throw new Error("Expected fixture API key.");
    const response = await fetch(`${gateway.baseUrl}/v1/videos`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${gateway.apiKey}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({
        model: VIDEO_MODEL,
        prompt: "A paper kite over a field.",
        width: 304,
        height: 320,
        frames: 33,
        fps: 16,
        input: { kind: "text" },
      }),
    });

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({
      error: { code: "validation_failed" },
    });
    expect(await gateway.readImageRuntimeLaunches()).toEqual([]);
  } finally {
    await gateway.stop();
  }
});

test("rejects a revoked credential before a video status route is exposed", async () => {
  const gateway = await startGatewayFixture({ auth: {} });
  const database = new DatabaseSession();
  try {
    if (!gateway.apiKey) throw new Error("Expected fixture API key.");
    const config = gateway.readConfig();
    const key = resolveApiKey(database, config, gateway.apiKey);
    if (!key) throw new Error("Expected active fixture API key.");
    revokeApiKey(database, config, key.id);

    const response = await fetch(
      `${gateway.baseUrl}/v1/videos/00000000-0000-4000-8000-000000000000`,
      { headers: { authorization: `Bearer ${gateway.apiKey}` } },
    );

    expect(response.status).toBe(401);
    expect(await response.json()).toMatchObject({
      error: { code: "invalid_api_key" },
    });
  } finally {
    database.close();
    await gateway.stop();
  }
});
