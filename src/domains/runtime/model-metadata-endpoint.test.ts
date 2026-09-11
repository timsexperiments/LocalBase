import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { CATALOG } from "../../catalog";
import {
  modelMetadataListSchema,
  modelMetadataSchema,
} from "../models/model-metadata";
import {
  startGatewayFixture,
  type GatewayFixture,
} from "../../test/gateway-fixture";

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const alternateModelId = "qwen2.5-coder-3b-instruct-q4_k_m";

describe("authenticated model metadata endpoints", () => {
  let gateway: GatewayFixture | undefined;

  beforeAll(async () => {
    gateway = await startGatewayFixture({ auth: { mode: "either" } });
  });

  afterAll(async () => {
    await gateway?.stop();
  });

  function activeGateway(): GatewayFixture {
    if (!gateway) throw new Error("Gateway fixture did not start.");
    return gateway;
  }

  function authHeaders(): HeadersInit {
    const fixture = activeGateway();
    if (!fixture.apiKey) throw new Error("Expected gateway API key.");
    return { Authorization: `Bearer ${fixture.apiKey}` };
  }

  test("requires the gateway API key", async () => {
    const response = await fetch(
      `${activeGateway().baseUrl}/_localbase/models`,
    );
    expect(response.status).toBe(401);
  });

  test("returns strict catalog and device metadata without starting runtimes", async () => {
    const fixture = activeGateway();
    expect(await fixture.readLlmRuntimeLaunches()).toEqual([]);
    expect(await fixture.readSttRuntimeLaunches()).toEqual([]);
    expect(await fixture.readImageRuntimeLaunches()).toEqual([]);

    const response = await fetch(`${fixture.baseUrl}/_localbase/models`, {
      headers: authHeaders(),
    });
    expect(response.status).toBe(200);
    const body = modelMetadataListSchema.parse(await response.json());
    const llm = body.data.find((model) => model.id === modelId);
    if (!llm) throw new Error(`Expected metadata for ${modelId}.`);

    expect(body.data).toHaveLength(CATALOG.length);
    expect(llm).toMatchObject({
      catalog: {
        capabilities: null,
        contextWindowTokens: null,
        maxOutputTokens: null,
      },
      device: {
        selected: true,
        installed: true,
        runtime: { configured: true, state: "idle" },
        warm: null,
        slots: null,
        readiness: null,
        queue: null,
      },
    });
    expect(JSON.stringify(body)).not.toContain(fixture.root);
    if (fixture.apiKey)
      expect(JSON.stringify(body)).not.toContain(fixture.apiKey);

    expect(await fixture.readLlmRuntimeLaunches()).toEqual([]);
    expect(await fixture.readSttRuntimeLaunches()).toEqual([]);
    expect(await fixture.readImageRuntimeLaunches()).toEqual([]);
  });

  test("returns one catalog model and rejects unknown IDs", async () => {
    const fixture = activeGateway();
    const detail = await fetch(
      `${fixture.baseUrl}/_localbase/models/${encodeURIComponent(modelId)}`,
      { headers: authHeaders() },
    );
    expect(detail.status).toBe(200);
    expect(modelMetadataSchema.parse(await detail.json()).id).toBe(modelId);

    const unknown = await fetch(
      `${fixture.baseUrl}/_localbase/models/not-a-catalog-model`,
      { headers: authHeaders() },
    );
    expect(unknown.status).toBe(404);
    await expect(unknown.json()).resolves.toMatchObject({
      error: { code: "model_not_found", param: "model" },
    });
  });

  test("reports the applied snapshot without refreshing a pending model change", async () => {
    const fixture = activeGateway();
    const pendingConfig = fixture.readConfig();
    pendingConfig.selectedLlmModels = [alternateModelId];
    pendingConfig.activeLlmModel = alternateModelId;
    fixture.saveConfig(pendingConfig);

    const response = await fetch(`${fixture.baseUrl}/_localbase/models`, {
      headers: authHeaders(),
    });
    expect(response.status).toBe(200);
    const metadata = modelMetadataListSchema.parse(await response.json());
    const applied = metadata.data.find((model) => model.id === modelId);
    const pending = metadata.data.find(
      (model) => model.id === alternateModelId,
    );
    if (!applied || !pending) throw new Error("Expected both LLM models.");

    expect(applied.device).toMatchObject({
      selected: true,
      runtime: { configured: true, state: "idle" },
    });
    expect(pending.device).toMatchObject({
      selected: false,
      runtime: null,
    });
    expect(await fixture.readLlmRuntimeLaunches()).toEqual([]);
  });
});
