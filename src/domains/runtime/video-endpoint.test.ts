import { expect, test } from "bun:test";
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
