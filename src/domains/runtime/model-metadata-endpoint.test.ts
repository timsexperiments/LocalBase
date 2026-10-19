import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { brotliDecompressSync, gunzipSync } from "node:zlib";
import { CATALOG } from "../../catalog";
import { DatabaseSession } from "../../db/client";
import { createApiKey } from "../../manager";
import {
  modelMetadataListSchema,
  modelMetadataSchema,
} from "../models/model-metadata";
import {
  registerGatewayFixtureCleanup,
  startGatewayFixture,
  type GatewayFixture,
  waitForLogEvent,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";

registerGatewayFixtureCleanup();

const modelId = "qwen3.5-4b-q4_k_m";
const alternateModelId = "qwen3.8-27b-q4_k_m";
const speechModelId = "qwen3-tts-1.7b-base-q4_k_m";

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
    gateway = await startGatewayFixture({
      auth: { mode: "either" },
      workingDirectory: tmpdir(),
    });
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

  test("compiled playground is public outside the checkout without bypassing API auth", async () => {
    const base = activeGateway().baseUrl;
    const shell = await fetch(`${base}/app`);
    expect(shell.status).toBe(200);
    expect(shell.headers.get("content-security-policy")).toContain(
      "frame-ancestors 'none'",
    );
    expect(shell.headers.get("cache-control")).toBe("no-cache");
    const requestId = shell.headers.get("x-localbase-request-id");
    expect(shell.headers.get("permissions-policy")).toBe(
      "camera=(), microphone=(self), geolocation=()",
    );
    const html = await shell.text();
    const requestEvent = await waitForLogEvent(
      activeGateway(),
      (event) =>
        event.eventName === "http.request" && event.requestId === requestId,
    );
    expect(requestEvent.attributes?.auth_outcome).toBe("disabled");
    const paths = [
      ...html.matchAll(/(?:src|href)="(\/app\/assets\/[^\"]+)"/g),
    ].map((match) => match[1]);
    expect(paths.length).toBe(2);
    for (const path of ["/", "/app", "/app/", ...paths]) {
      const identity = { "accept-encoding": "identity" };
      const get = await fetch(`${base}${path}`, { headers: identity });
      const head = await fetch(`${base}${path}`, {
        method: "HEAD",
        headers: identity,
      });
      expect(get.status).toBe(200);
      expect(head.status).toBe(200);
      expect(Number(head.headers.get("content-length"))).toBe(
        (await get.arrayBuffer()).byteLength,
      );
      expect(await head.text()).toBe("");
    }
    for (const path of [
      "/app/assets/missing.js",
      "/app/v1/models",
      "/app/assets/constructor",
      "/v1/models",
      "/v1/chat/completions",
      "/_localbase/models",
    ]) {
      const response = await fetch(`${base}${path}`);
      expect(response.status).toBe(401);
      expect(response.headers.get("content-type")).not.toContain("text/html");
    }
    expect((await fetch(`${base}/app`, { method: "POST" })).status).toBe(401);
    expect(
      (await fetch(`${base}/v1/models`, { headers: authHeaders() })).status,
    ).toBe(200);
    expect(
      (await fetch(`${base}/app/assets/missing.js`, { headers: authHeaders() }))
        .status,
    ).toBe(404);
  });

  test("caches hashed assets immutably and serves negotiated compression", async () => {
    const base = activeGateway().baseUrl;
    const shell = await fetch(`${base}/app`, {
      headers: { "accept-encoding": "identity" },
    });
    const etag = shell.headers.get("etag");
    expect(etag).toBeTruthy();
    const html = await shell.text();
    const revalidated = await fetch(`${base}/app`, {
      headers: { "if-none-match": etag ?? "" },
    });
    expect(revalidated.status).toBe(304);
    expect(revalidated.headers.get("cache-control")).toBe("no-cache");
    expect(revalidated.headers.get("x-frame-options")).toBe("DENY");

    const script = html.match(/src="(\/app\/assets\/[^"]+\.js)"/)?.[1];
    if (!script) throw new Error("Expected a hashed script asset.");
    const identity = await fetch(`${base}${script}`, {
      headers: { "accept-encoding": "identity" },
    });
    const identityBytes = (await identity.arrayBuffer()).byteLength;
    expect(identity.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
    expect(identity.headers.get("content-encoding")).toBeNull();
    expect(identity.headers.get("vary")).toBe("Accept-Encoding");
    for (const encoding of ["br", "gzip"] as const) {
      const response = await fetch(`${base}${script}`, {
        headers: { "accept-encoding": encoding },
        decompress: false,
      });
      expect(response.headers.get("content-encoding")).toBe(encoding);
      expect(response.headers.get("vary")).toBe("Accept-Encoding");
      expect(response.headers.get("content-security-policy")).toContain(
        "default-src 'none'",
      );
      const compressed = new Uint8Array(await response.arrayBuffer());
      const head = await fetch(`${base}${script}`, {
        method: "HEAD",
        headers: { "accept-encoding": encoding },
        decompress: false,
      });
      expect(Number(head.headers.get("content-length"))).toBe(
        compressed.byteLength,
      );
      expect(compressed.byteLength).toBeLessThan(identityBytes / 2);
      const decoded =
        encoding === "br"
          ? brotliDecompressSync(compressed)
          : gunzipSync(compressed);
      expect(decoded.byteLength).toBe(identityBytes);
    }

    const models = await fetch(`${base}/_localbase/models`, {
      headers: { ...authHeaders(), "accept-encoding": "gzip" },
      decompress: false,
    });
    expect(models.status).toBe(200);
    expect(models.headers.get("content-encoding")).toBe("gzip");
    expect(models.headers.get("vary")).toContain("Accept-Encoding");
    modelMetadataListSchema.parse(
      JSON.parse(
        gunzipSync(new Uint8Array(await models.arrayBuffer())).toString(),
      ),
    );
    const unauthenticated = await fetch(`${base}/_localbase/models`, {
      headers: { "accept-encoding": "gzip" },
      decompress: false,
    });
    expect(unauthenticated.status).toBe(401);
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
    expect(body.host.memory).toEqual({
      kind: "discrete",
      system: {
        capacityBytes: 64 * 1024 ** 3,
        availableBytes: 32 * 1024 ** 3,
      },
      accelerators: [
        {
          capacityBytes: 32 * 1024 ** 3,
          availableBytes: 24 * 1024 ** 3,
        },
      ],
    });
    const llm = body.data.find((model) => model.id === modelId);
    const unknownContext = metadataById(body, alternateModelId);
    const speech = metadataById(body, speechModelId);
    if (!llm || !unknownContext || !speech)
      throw new Error("Expected catalog metadata fixtures.");

    expect(body.data).toHaveLength(CATALOG.length);
    expect(llm).toMatchObject({
      catalog: {
        capabilities: null,
        features: ["text-generation", "tool-calling"],
        inputModalities: ["text"],
        outputModalities: ["text"],
        contextWindowTokens: 262_144,
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
    expect(unknownContext.catalog.features).toContain("tool-calling");
    expect(unknownContext.catalog.contextWindowTokens).toBeNull();
    expect(unknownContext.catalog.maxOutputTokens).toBeNull();
    expect(speech.catalog).toMatchObject({
      inputModalities: ["text"],
      outputModalities: ["audio"],
      contextWindowTokens: null,
      maxOutputTokens: null,
      capabilities: {
        kind: "speech",
        outputFormats: ["wav"],
        voice: {
          selection: "catalog-reference",
          requestValues: ["default", "harbor", "willow"],
          defaultRequestValue: "default",
          references: [
            {
              name: "harbor",
              license: "CC0-1.0",
              provenanceUrl:
                "https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations",
            },
            {
              name: "willow",
              license: "CC0-1.0",
              provenanceUrl:
                "https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations",
            },
          ],
        },
        residency: "cold-per-request",
      },
    });
    expect(JSON.stringify(body)).not.toContain(fixture.root);
    if (fixture.apiKey)
      expect(JSON.stringify(body)).not.toContain(fixture.apiKey);

    expect(await fixture.readLlmRuntimeLaunches()).toEqual([]);
    expect(await fixture.readSttRuntimeLaunches()).toEqual([]);
    expect(await fixture.readImageRuntimeLaunches()).toEqual([]);
  });

  test("filters and sorts the list with optional catalog query parameters", async () => {
    const fixture = activeGateway();
    const all = modelMetadataListSchema.parse(
      await (
        await fetch(`${fixture.baseUrl}/_localbase/models`, {
          headers: authHeaders(),
        })
      ).json(),
    );
    const response = await fetch(
      `${fixture.baseUrl}/_localbase/models?kind=image&capability=text-to-image&sort=size&reverse=true&fit=bogus`,
      { headers: authHeaders() },
    );
    expect(response.status).toBe(200);
    const body = modelMetadataListSchema.parse(await response.json());
    expect(body.host).toEqual(all.host);
    expect(body.data.length).toBeGreaterThan(0);
    expect(body.data.length).toBeLessThan(all.data.length);
    expect(body.data.every(({ catalog }) => catalog.kind === "image")).toBe(
      true,
    );
    const sizes = body.data.map(
      ({ catalog }) => catalog.memory.storageEstimateGb,
    );
    expect(sizes).toEqual([...sizes].sort((a, b) => b - a));

    const installed = modelMetadataListSchema.parse(
      await (
        await fetch(`${fixture.baseUrl}/_localbase/models?installed=true`, {
          headers: authHeaders(),
        })
      ).json(),
    );
    expect(installed.data.map(({ id }) => id)).toEqual(
      all.data.filter(({ device }) => device.installed).map(({ id }) => id),
    );
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
