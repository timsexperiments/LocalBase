import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  startGatewayFixture,
  type GatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";
import { modelMetadataListSchema } from "../models/model-metadata";
import { gatewayReadinessSchema } from "./readiness";

const activeModelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const replacementModelId = "qwen2.5-coder-3b-instruct-q4_k_m";

describe("gateway readiness endpoint", () => {
  let gateway: GatewayFixture;

  beforeAll(async () => {
    gateway = await startGatewayFixture({ auth: { mode: "either" } });
  }, 30_000);

  afterAll(async () => {
    await gateway?.stop();
  }, 10_000);

  test("is public, strict, and does not launch a runtime", async () => {
    const before = await Promise.all([
      gateway.readLlmRuntimeLaunches(),
      gateway.readSttRuntimeLaunches(),
      gateway.readImageRuntimeLaunches(),
    ]);
    expect(before).toEqual([[], [], []]);

    const response = await fetch(`${gateway.baseUrl}/health/ready`);
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toBe(
      "application/json; charset=utf-8",
    );
    const body = gatewayReadinessSchema.parse(await response.json());
    expect(body).toEqual({
      status: "ready",
      reason: "request_admission_available",
      modalities: ["llm", "stt", "image"],
    });
    expect(response.headers.get("content-length")).toBe(
      String(new TextEncoder().encode(JSON.stringify(body)).byteLength),
    );

    const serialized = JSON.stringify(body);
    expect(serialized).not.toContain(gateway.root);
    expect(serialized).not.toContain("qwen2.5-coder-1.5b-instruct-q4_k_m");
    expect(serialized).not.toContain("lb_");
    await expect(
      Promise.all([
        gateway.readLlmRuntimeLaunches(),
        gateway.readSttRuntimeLaunches(),
        gateway.readImageRuntimeLaunches(),
      ]),
    ).resolves.toEqual([[], [], []]);
  });

  test("returns the same readiness status and headers for HEAD", async () => {
    const get = await fetch(`${gateway.baseUrl}/health/ready`);
    const head = await fetch(`${gateway.baseUrl}/health/ready`, {
      method: "HEAD",
    });

    expect(head.status).toBe(get.status);
    expect(head.headers.get("content-type")).toBe(
      get.headers.get("content-type"),
    );
    expect(head.headers.get("content-length")).toBe(
      get.headers.get("content-length"),
    );
    expect(await head.text()).toBe("");
  });

  test("rejects non-probe methods without authenticating or launching", async () => {
    const response = await fetch(`${gateway.baseUrl}/health/ready`, {
      method: "POST",
    });

    expect(response.status).toBe(405);
    expect(response.headers.get("allow")).toBe("GET, HEAD");
    await expect(response.json()).resolves.toMatchObject({
      error: { code: "method_not_allowed" },
    });
    await expect(gateway.readLlmRuntimeLaunches()).resolves.toEqual([]);
  });

  test("stays ready while a controlled replacement drains with queue capacity", async () => {
    if (!gateway.apiKey) throw new Error("Expected gateway API key.");
    const streamId = "readiness-draining-replacement";
    const headers = { Authorization: `Bearer ${gateway.apiKey}` };
    const active = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
      method: "POST",
      headers: {
        ...headers,
        "content-type": "application/json",
        "x-test-upstream": "controlled-stream",
        "x-test-stream-id": streamId,
      },
      body: JSON.stringify({
        model: activeModelId,
        messages: [{ role: "user", content: "hold this stream" }],
      }),
    });
    expect(active.status).toBe(200);
    if (!active.body) throw new Error("Expected a streaming response.");
    const reader = active.body.getReader();
    expect((await reader.read()).done).toBe(false);
    await gateway.waitForUpstreamRequest(streamId);

    const pending = gateway.readConfig();
    pending.selectedLlmModels = [replacementModelId];
    pending.activeLlmModel = replacementModelId;
    await writeCompleteCatalogArtifact(
      pending.llmModelsDir,
      replacementModelId,
    );
    gateway.saveConfig(pending);
    const replacement = fetch(`${gateway.baseUrl}/v1/chat/completions`, {
      method: "POST",
      headers: { ...headers, "content-type": "application/json" },
      body: JSON.stringify({
        model: replacementModelId,
        messages: [{ role: "user", content: "switch models" }],
      }),
    });

    try {
      const deadline = Date.now() + 3_000;
      let observedDrain = false;
      while (Date.now() < deadline) {
        const metadataResponse = await fetch(
          `${gateway.baseUrl}/_localbase/models`,
          { headers },
        );
        expect(metadataResponse.status).toBe(200);
        const metadata = modelMetadataListSchema.parse(
          await metadataResponse.json(),
        );
        const activeModel = metadata.data.find(
          (model) => model.id === activeModelId,
        );
        if (activeModel?.device.runtime?.state !== "draining") {
          await Bun.sleep(10);
          continue;
        }
        const readinessResponse = await fetch(
          `${gateway.baseUrl}/health/ready`,
        );
        expect(readinessResponse.status).toBe(200);
        const readiness = gatewayReadinessSchema.parse(
          await readinessResponse.json(),
        );
        expect(readiness.status).toBe("ready");
        expect(readiness.modalities).toContain("llm");
        observedDrain = true;
        break;
      }
      expect(observedDrain).toBe(true);
    } finally {
      gateway.closeControlledStream(streamId);
      while (!(await reader.read()).done) {}
    }

    const replacementResponse = await replacement;
    expect(replacementResponse.status).toBe(200);
    await replacementResponse.text();
  }, 15_000);
});
