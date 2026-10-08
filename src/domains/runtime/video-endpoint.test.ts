import { expect, test } from "bun:test";
import { DatabaseSession } from "../../db/client";
import { resolveApiKey, revokeApiKey } from "../../manager";
import {
  registerGatewayFixtureCleanup,
  startGatewayFixture,
  VIDEO_MODEL,
} from "../../test/gateway-fixture";

registerGatewayFixtureCleanup();

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

const JOB_PATH = "/v1/videos/00000000-0000-4000-8000-000000000000";

test("serves video routes without a key when gateway auth is disabled", async () => {
  const gateway = await startGatewayFixture({ videoEnabled: true });
  try {
    const status = await fetch(`${gateway.baseUrl}${JOB_PATH}`);
    expect(status.status).toBe(404);
    expect(await status.json()).toMatchObject({
      error: { code: "video_job_not_found" },
    });

    const created = await fetch(`${gateway.baseUrl}/v1/videos`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: VIDEO_MODEL,
        prompt: "A paper kite over a field.",
        width: 512,
        height: 512,
        frames: 49,
        fps: 24,
        input: { kind: "text" },
      }),
    });
    expect(created.status).toBe(400);
    const body = (await created.json()) as { error: { message: string } };
    expect(body.error.message).toContain("accepts exactly");
    expect(body.error.message).toContain("got 512x512, 49 frames at 24 fps");
  } finally {
    await gateway.stop();
  }
});

test("still rejects keyless video requests when gateway auth is enabled", async () => {
  const gateway = await startGatewayFixture({ auth: {}, videoEnabled: true });
  try {
    const response = await fetch(`${gateway.baseUrl}${JOB_PATH}`);
    expect(response.status).toBe(401);
  } finally {
    await gateway.stop();
  }
});
