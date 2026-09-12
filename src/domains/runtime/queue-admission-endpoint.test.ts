import { expect, test } from "bun:test";
import {
  modelMetadataListSchema,
  type ModelMetadata,
} from "../models/model-metadata";
import {
  startGatewayFixture,
  type GatewayFixture,
  waitForLogEvent,
} from "../../test/gateway-fixture";
import { gatewayReadinessSchema } from "./readiness";

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";

function headers(apiKey: string, streamId: string): HeadersInit {
  return {
    Authorization: `Bearer ${apiKey}`,
    "content-type": "application/json",
    "x-test-upstream": "controlled-stream",
    "x-test-stream-id": streamId,
  };
}

function chatRequest(
  gateway: GatewayFixture,
  apiKey: string,
  streamId: string,
) {
  return fetch(`${gateway.baseUrl}/v1/chat/completions`, {
    method: "POST",
    headers: headers(apiKey, streamId),
    body: JSON.stringify({
      model: modelId,
      messages: [{ role: "user", content: "hold this request" }],
    }),
  });
}

function modelRuntime(metadata: unknown): ModelMetadata["device"]["runtime"] {
  const model = modelMetadataListSchema
    .parse(metadata)
    .data.find((candidate) => candidate.id === modelId);
  if (!model) throw new Error(`Expected metadata for ${modelId}.`);
  return model.device.runtime;
}

test("projects real HTTP queue saturation into readiness and authenticated metadata", async () => {
  const gateway = await startGatewayFixture({
    auth: { mode: "either" },
    inferenceQueueCapacity: 1,
    sttEnabled: false,
    imageEnabled: false,
  });
  try {
    if (!gateway.apiKey) throw new Error("Expected gateway API key.");
    const config = gateway.readConfig();
    config.parallel = 2;
    gateway.saveConfig(config);

    const first = await chatRequest(gateway, gateway.apiKey, "queue-first");
    expect(first.status).toBe(200);
    await gateway.waitForUpstreamRequest("queue-first");

    const second = await chatRequest(gateway, gateway.apiKey, "queue-second");
    expect(second.status).toBe(200);
    await gateway.waitForUpstreamRequest("queue-second");

    const extraRequests = [
      {
        streamId: "queue-extra-a",
        response: chatRequest(gateway, gateway.apiKey, "queue-extra-a"),
      },
      {
        streamId: "queue-extra-b",
        response: chatRequest(gateway, gateway.apiKey, "queue-extra-b"),
      },
    ];
    const saturatedResponse = await Promise.race(
      extraRequests.map(({ response }) => response),
    );
    const saturatedRequestId = saturatedResponse.headers.get(
      "x-localbase-request-id",
    );
    expect(saturatedRequestId).toMatch(/^lbreq_/);
    expect(saturatedResponse.status).toBe(429);
    expect(saturatedResponse.headers.get("retry-after")).toBe("1");
    await expect(saturatedResponse.json()).resolves.toMatchObject({
      error: { code: "inference_queue_full" },
    });

    const saturatedMetadata = await fetch(
      `${gateway.baseUrl}/_localbase/models`,
      {
        headers: { Authorization: `Bearer ${gateway.apiKey}` },
      },
    );
    expect(saturatedMetadata.status).toBe(200);
    const saturated = modelRuntime(await saturatedMetadata.json());
    expect(saturated).toMatchObject({
      configured: true,
      state: "running",
      effectiveSlots: 2,
      availableCapacity: 0,
      queueDepth: 1,
    });
    expect(
      gateway.upstreamRequests.filter((request) =>
        request.headers.get("x-test-stream-id")?.startsWith("queue-"),
      ),
    ).toHaveLength(2);
    expect(
      await waitForLogEvent(
        gateway,
        (event) =>
          event.eventName === "inference.admission-rejected" &&
          event.requestId === saturatedRequestId,
      ),
    ).toMatchObject({
      runtime: "llm",
      attributes: {
        modality: "llm",
        error_code: "inference_queue_full",
        source: "capacity",
        http_status: 429,
      },
    });

    const unready = await fetch(`${gateway.baseUrl}/health/ready`);
    expect(unready.status).toBe(503);
    expect(gatewayReadinessSchema.parse(await unready.json())).toEqual({
      status: "unready",
      reason: "no_request_admission",
      modalities: [],
    });

    gateway.closeControlledStream("queue-first");
    await first.text();
    const extraResponses = await Promise.all(
      extraRequests.map(async ({ streamId, response }) => ({
        streamId,
        response: await response,
      })),
    );
    const admitted = extraResponses.find(
      ({ response }) => response.status === 200,
    );
    expect(
      extraResponses.filter(({ response }) => response.status === 200),
    ).toHaveLength(1);
    expect(
      extraResponses.filter(({ response }) => response.status === 429),
    ).toHaveLength(1);
    if (!admitted)
      throw new Error("Expected one queued request to be admitted.");
    await gateway.waitForUpstreamRequest(admitted.streamId);

    const availableMetadata = await fetch(
      `${gateway.baseUrl}/_localbase/models`,
      {
        headers: { Authorization: `Bearer ${gateway.apiKey}` },
      },
    );
    expect(availableMetadata.status).toBe(200);
    const available = modelRuntime(await availableMetadata.json());
    expect(available).toMatchObject({
      effectiveSlots: 2,
      availableCapacity: 1,
      queueDepth: 0,
    });
    const ready = await fetch(`${gateway.baseUrl}/health/ready`);
    expect(ready.status).toBe(200);
    expect(gatewayReadinessSchema.parse(await ready.json())).toMatchObject({
      status: "ready",
      modalities: ["llm"],
    });

    gateway.closeControlledStream("queue-second");
    gateway.closeControlledStream(admitted.streamId);
    await Promise.all([second.text(), admitted.response.text()]);
  } finally {
    await gateway.stop();
  }
});
