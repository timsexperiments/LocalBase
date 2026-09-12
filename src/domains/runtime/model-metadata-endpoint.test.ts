import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { CATALOG } from "../../catalog";
import { DatabaseSession } from "../../db/client";
import { createApiKey } from "../../manager";
import {
  modelMetadataListSchema,
  modelMetadataSchema,
} from "../models/model-metadata";
import {
  startGatewayFixture,
  type GatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const alternateModelId = "qwen2.5-coder-3b-instruct-q4_k_m";

function apiKeyHeaders(apiKey: string): Record<string, string> {
  return { Authorization: `Bearer ${apiKey}` };
}

function metadataById(
  metadata: ReturnType<typeof modelMetadataListSchema.parse>,
  id: string,
) {
  return metadata.data.find((model) => model.id === id);
}

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
    return apiKeyHeaders(fixture.apiKey);
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
        runtime: {
          configured: true,
          state: "idle",
          executionSlots: null,
          activeAdmissions: 0,
          availableExecutionSlots: null,
          immediateDispatchAvailable: true,
          queuedRequests: 0,
          waitingCapacity: 16,
          availableWaitingCapacity: 16,
        },
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
      runtime: {
        configured: true,
        state: "idle",
        executionSlots: null,
        activeAdmissions: 0,
        availableExecutionSlots: null,
        immediateDispatchAvailable: true,
        queuedRequests: 0,
        waitingCapacity: 16,
        availableWaitingCapacity: 16,
      },
    });
    expect(pending.device).toMatchObject({
      selected: false,
      runtime: null,
    });
    expect(await fixture.readLlmRuntimeLaunches()).toEqual([]);
  });
});

test("requires an API key when inference authentication is disabled", async () => {
  const gateway = await startGatewayFixture();
  try {
    const config = gateway.readConfig();
    const database = new DatabaseSession();
    const apiKey = createApiKey(database, config, "metadata").rawKey;
    database.close();

    const unauthorized = await fetch(`${gateway.baseUrl}/_localbase/models`);
    expect(unauthorized.status).toBe(401);
    const unauthorizedDetail = await fetch(
      `${gateway.baseUrl}/_localbase/models/${modelId}`,
    );
    expect(unauthorizedDetail.status).toBe(401);

    const authorized = await fetch(`${gateway.baseUrl}/_localbase/models`, {
      headers: apiKeyHeaders(apiKey),
    });
    expect(authorized.status).toBe(200);
    modelMetadataListSchema.parse(await authorized.json());
    const authorizedDetail = await fetch(
      `${gateway.baseUrl}/_localbase/models/${modelId}`,
      { headers: apiKeyHeaders(apiKey) },
    );
    expect(authorizedDetail.status).toBe(200);
    modelMetadataSchema.parse(await authorizedDetail.json());
  } finally {
    await gateway.stop();
  }
});

test(
  "reports configured selection with the draining applied runtime",
  async () => {
    const gateway = await startGatewayFixture({
      auth: { mode: "either" },
      parallel: 1,
    });
    const streamId = "metadata-applied-generation";
    let streamStarted = false;
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    let replacementResult:
      Promise<Response | { readonly error: unknown }> | undefined;
    try {
      if (!gateway.apiKey) throw new Error("Expected gateway API key.");
      const headers = apiKeyHeaders(gateway.apiKey);
      const active = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: {
          ...headers,
          "Content-Type": "application/json",
          "x-test-upstream": "controlled-stream",
          "x-test-stream-id": streamId,
        },
        body: JSON.stringify({
          model: modelId,
          messages: [{ role: "user", content: "hold this stream" }],
        }),
      });
      expect(active.status).toBe(200);
      if (!active.body) throw new Error("Expected a streaming response.");
      reader = active.body.getReader();
      expect((await reader.read()).done).toBe(false);
      await gateway.waitForUpstreamRequest(streamId);
      streamStarted = true;

      const pending = gateway.readConfig();
      pending.selectedLlmModels = [alternateModelId];
      pending.activeLlmModel = alternateModelId;
      await writeCompleteCatalogArtifact(
        pending.llmModelsDir,
        alternateModelId,
      );
      gateway.saveConfig(pending);

      replacementResult = fetch(`${gateway.baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { ...headers, "Content-Type": "application/json" },
        body: JSON.stringify({
          model: alternateModelId,
          messages: [{ role: "user", content: "switch models" }],
        }),
      }).catch((error: unknown) => ({ error }));

      const deadline = Date.now() + 3_000;
      let applied: ReturnType<typeof modelMetadataListSchema.parse> | undefined;
      while (Date.now() < deadline) {
        const response = await fetch(`${gateway.baseUrl}/_localbase/models`, {
          headers,
        });
        expect(response.status).toBe(200);
        const metadata = modelMetadataListSchema.parse(await response.json());
        const original = metadataById(metadata, modelId);
        if (original?.device.runtime?.state === "draining") {
          applied = metadata;
          break;
        }
        await Bun.sleep(10);
      }
      if (!applied) throw new Error("Expected the original runtime to drain.");

      const original = metadataById(applied, modelId);
      const replacementModel = metadataById(applied, alternateModelId);
      if (!original || !replacementModel) {
        throw new Error("Expected both LLM models in metadata.");
      }
      expect(original.device).toMatchObject({
        selected: false,
        runtime: {
          state: "draining",
          executionSlots: 1,
          activeAdmissions: 1,
          availableExecutionSlots: 0,
          immediateDispatchAvailable: false,
          queuedRequests: 1,
        },
      });
      expect(replacementModel.device).toMatchObject({
        selected: true,
        runtime: null,
      });

      gateway.closeControlledStream(streamId);
      while (!(await reader.read()).done) {}
      const replacementResponse = await replacementResult;
      if (!(replacementResponse instanceof Response)) {
        throw replacementResponse.error;
      }
      expect(replacementResponse.status).toBe(200);
      await replacementResponse.text();
    } finally {
      if (streamStarted) gateway.closeControlledStream(streamId);
      await Promise.allSettled([
        (async () => {
          if (!reader) return;
          if (!streamStarted) {
            await reader.cancel();
            return;
          }
          while (!(await reader.read()).done) {}
        })(),
        replacementResult ?? Promise.resolve(),
      ]);
      await gateway.stop();
    }
  },
  { timeout: 15_000 },
);
