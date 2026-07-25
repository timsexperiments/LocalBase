import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createOpenAICompatible } from "@ai-sdk/openai-compatible";
import { generateText } from "ai";
import { join } from "node:path";
import { loadConfig, saveConfig } from "../../manager";
import { DatabaseSession } from "../../db/client";
import { withDatabase } from "../../db/client";
import {
  deleteModelSystemPrompt,
  saveModelSystemPrompt,
} from "./model-system-prompts";
import { DEFAULT_SYSTEM_PROMPT } from "./commands/prompt";
import {
  startGatewayFixture,
  type GatewayFixture,
} from "../../test/gateway-fixture";
import { httpBaseUrl } from "./commands/serve";

type ValidationCase = {
  name: string;
  path: string;
  init: RequestInit;
  expectedPath: string;
};

test("formats IPv4, hostnames, and IPv6 literals as HTTP base URLs", () => {
  expect(httpBaseUrl("127.0.0.1", 2273)).toBe("http://127.0.0.1:2273");
  expect(httpBaseUrl("localhost", 2273)).toBe("http://localhost:2273");
  expect(httpBaseUrl("::1", 2273)).toBe("http://[::1]:2273");
});

describe("API gateway integration", () => {
  let gateway: GatewayFixture;

  beforeAll(
    async () => {
      gateway = await startGatewayFixture();
    },
    { timeout: 30_000 },
  );

  afterAll(
    async () => {
      await gateway?.stop();
    },
    { timeout: 10_000 },
  );

  const request = (path: string, init?: RequestInit) =>
    fetch(`${gateway.baseUrl}${path}`, init);

  function loadGatewayConfig() {
    const database = new DatabaseSession();
    try {
      return loadConfig(database, gateway.root);
    } finally {
      database.close();
    }
  }

  function saveGatewayConfig(config: ReturnType<typeof loadConfig>): void {
    const database = new DatabaseSession();
    try {
      saveConfig(database, config);
    } finally {
      database.close();
    }
  }

  function saveGatewayModelPrompt(modelId: string, prompt: string): void {
    const database = new DatabaseSession();
    try {
      withDatabase(database, gateway.root, (connection) => {
        saveModelSystemPrompt(connection, { modelId, prompt });
      });
    } finally {
      database.close();
    }
  }

  function deleteGatewayModelPrompt(modelId: string): void {
    const database = new DatabaseSession();
    try {
      withDatabase(database, gateway.root, (connection) => {
        deleteModelSystemPrompt(connection, modelId);
      });
    } finally {
      database.close();
    }
  }

  async function expectValidationFailure(
    path: string,
    init: RequestInit,
    expectedPath: string,
  ): Promise<void> {
    const response = await request(path, init);
    expect(response.status).toBe(400);

    const body = (await response.json()) as {
      error: { code: string; message: string; type: string };
    };
    expect(body.error).toMatchObject({
      type: "invalid_request_error",
      code: "validation_failed",
    });
    expect(body.error.message).toContain(`${expectedPath}:`);
  }

  test("GET /health reports the enabled modalities", async () => {
    const response = await request("/health");
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      status: "ok",
      enabled: { llm: true, stt: true, image: true },
    });
  });

  test("GET /v1/models lists the configured active model", async () => {
    const response = await request("/v1/models");
    expect(response.status).toBe(200);

    const body = (await response.json()) as {
      object: string;
      data: Array<{
        id: string;
        object: string;
        created: number;
        owned_by: string;
      }>;
    };
    expect(body.object).toBe("list");
    expect(body.data).toContainEqual({
      id: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      object: "model",
      created: 1670000000,
      owned_by: "local-base",
    });
  });

  test("proxies normalized chat requests without gateway credentials", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: "Bearer gateway-secret",
        "x-api-key": "gateway-key",
        "x-test-header": "retained",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "developer", content: "hello" }],
        provider_option: "preserved",
      }),
    });
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      object: "chat.completion",
      choices: [{ message: { content: "ok" } }],
    });

    const upstream = gateway.upstreamRequests.at(-1);
    expect(upstream?.headers.get("authorization")).toBeNull();
    expect(upstream?.headers.get("x-api-key")).toBeNull();
    expect(upstream?.headers.get("x-test-header")).toBe("retained");
    expect(JSON.parse(upstream?.body ?? "{}")).toMatchObject({
      model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      provider_option: "preserved",
      messages: [{ role: "system", content: "hello" }],
    });
  });

  test("injects the model prompt for the canonical LLM serving each request", async () => {
    const firstModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const secondModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    const original = loadGatewayConfig();
    const configured = {
      ...original,
      activeLlmModel: firstModel,
      selectedLlmModels: [firstModel, secondModel],
      systemPrompt: "Global fallback",
    };
    saveGatewayConfig(configured);
    await Bun.write(
      join(configured.llmModelsDir, `${secondModel}.gguf`),
      "test model placeholder",
    );
    saveGatewayModelPrompt(firstModel, "First model prompt");
    saveGatewayModelPrompt(secondModel, "Second model prompt");

    try {
      for (const model of [firstModel, `openai/${secondModel}`]) {
        const response = await request("/v1/chat/completions", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            model,
            messages: [{ role: "user", content: "hello" }],
          }),
        });
        expect(response.status).toBe(200);
      }

      const firstRequest = JSON.parse(
        gateway.upstreamRequests.at(-2)?.body ?? "{}",
      );
      const secondRequest = JSON.parse(
        gateway.upstreamRequests.at(-1)?.body ?? "{}",
      );
      expect(firstRequest.messages[0]).toEqual({
        role: "system",
        content: "First model prompt",
      });
      expect(secondRequest.messages[0]).toEqual({
        role: "system",
        content: "Second model prompt",
      });
    } finally {
      deleteGatewayModelPrompt(firstModel);
      deleteGatewayModelPrompt(secondModel);
      saveGatewayConfig(original);
    }
  });

  test("preserves client system and developer messages without injecting prompts", async () => {
    const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const original = loadGatewayConfig();
    saveGatewayConfig({ ...original, systemPrompt: "Global fallback" });
    saveGatewayModelPrompt(modelId, "Model override");

    try {
      const response = await request("/v1/chat/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: modelId,
          messages: [
            { role: "system", content: "Client system" },
            { role: "developer", content: "Client developer" },
            { role: "user", content: "hello" },
          ],
        }),
      });
      expect(response.status).toBe(200);

      const body = JSON.parse(gateway.upstreamRequests.at(-1)?.body ?? "{}");
      expect(body.messages).toEqual([
        { role: "system", content: "Client system" },
        { role: "system", content: "Client developer" },
        { role: "user", content: "hello" },
      ]);
    } finally {
      deleteGatewayModelPrompt(modelId);
      saveGatewayConfig(original);
    }
  });

  test("falls back from a model override to global and built-in prompts", async () => {
    const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const original = loadGatewayConfig();
    deleteGatewayModelPrompt(modelId);
    saveGatewayConfig({ ...original, systemPrompt: "Global fallback" });

    try {
      for (const systemPrompt of ["Global fallback", ""]) {
        const current = loadGatewayConfig();
        saveGatewayConfig({ ...current, systemPrompt });
        const response = await request("/v1/chat/completions", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            model: modelId,
            messages: [{ role: "user", content: "hello" }],
          }),
        });
        expect(response.status).toBe(200);
      }

      const globalRequest = JSON.parse(
        gateway.upstreamRequests.at(-2)?.body ?? "{}",
      );
      const builtInRequest = JSON.parse(
        gateway.upstreamRequests.at(-1)?.body ?? "{}",
      );
      expect(globalRequest.messages[0]).toEqual({
        role: "system",
        content: "Global fallback",
      });
      expect(builtInRequest.messages[0]).toEqual({
        role: "system",
        content: DEFAULT_SYSTEM_PROMPT,
      });
    } finally {
      saveGatewayConfig(original);
    }
  });

  test("does not modify legacy text-completion prompts", async () => {
    const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const original = loadGatewayConfig();
    saveGatewayConfig({ ...original, systemPrompt: "Global fallback" });
    saveGatewayModelPrompt(modelId, "Model override");

    try {
      const response = await request("/v1/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: modelId, prompt: "Raw completion" }),
      });
      expect(response.status).toBe(200);
      expect(JSON.parse(gateway.upstreamRequests.at(-1)?.body ?? "{}")).toEqual(
        expect.objectContaining({ model: modelId, prompt: "Raw completion" }),
      );
    } finally {
      deleteGatewayModelPrompt(modelId);
      saveGatewayConfig(original);
    }
  });

  test("accepts an AI SDK system prompt without injecting LocalBase fallbacks", async () => {
    const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const original = loadGatewayConfig();
    saveGatewayConfig({ ...original, systemPrompt: "Global fallback" });
    saveGatewayModelPrompt(modelId, "Model override");
    const localbase = createOpenAICompatible({
      baseURL: `${gateway.baseUrl}/v1`,
      name: "localbase-test",
    });

    try {
      const result = await generateText({
        model: localbase.chatModel(modelId),
        system: "AI SDK system instruction",
        prompt: "hello",
      });
      expect(result.text).toBe("ok");

      const body = JSON.parse(gateway.upstreamRequests.at(-1)?.body ?? "{}");
      expect(body.messages).toEqual(
        expect.arrayContaining([
          { role: "system", content: "AI SDK system instruction" },
          { role: "user", content: "hello" },
        ]),
      );
      expect(body.messages).not.toContainEqual({
        role: "system",
        content: "Global fallback",
      });
      expect(body.messages).not.toContainEqual({
        role: "system",
        content: "Model override",
      });
    } finally {
      deleteGatewayModelPrompt(modelId);
      saveGatewayConfig(original);
    }
  });

  test("returns a 502 OpenAI error for malformed successful upstream responses", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "malformed",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(502);
    expect(await response.json()).toMatchObject({
      error: { type: "server_error", code: "upstream_error" },
    });
  });

  test("passes SSE responses through without buffering or schema gating", async () => {
    const response = await request("/v1/chat/completions", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "x-test-upstream": "stream",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        stream: true,
        messages: [{ role: "user", content: "hello" }],
      }),
    });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("text/event-stream");
    expect(await response.text()).toContain("[DONE]");
  });

  test("rejects removed raw backend namespaces", async () => {
    for (const pathname of ["/llm/health", "/stt/health", "/image/health"]) {
      const response = await request(pathname);
      expect(response.status).toBe(404);
    }
  });

  test("does not switch models for invalid requests and serializes valid switches", async () => {
    const initialConfig = loadGatewayConfig();
    const secondModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    initialConfig.selectedLlmModels = [
      initialConfig.activeLlmModel,
      secondModel,
    ];
    saveGatewayConfig(initialConfig);
    await Bun.write(
      join(initialConfig.llmModelsDir, `${secondModel}.gguf`),
      "test model placeholder",
    );

    const invalid = await request("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: secondModel }),
    });
    expect(invalid.status).toBe(400);
    expect(loadGatewayConfig().activeLlmModel).toBe(
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
    );

    const responses = await Promise.all(
      [secondModel, "qwen2.5-coder-1.5b-instruct-q4_k_m", secondModel].map(
        (model) =>
          request("/v1/chat/completions", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              model,
              messages: [{ role: "user", content: "hello" }],
            }),
          }),
      ),
    );
    expect(responses.map((response) => response.status)).toEqual([
      200, 200, 200,
    ]);
    expect([secondModel, "qwen2.5-coder-1.5b-instruct-q4_k_m"]).toContain(
      loadGatewayConfig().activeLlmModel,
    );
  });

  const jsonValidationCases: ValidationCase[] = [
    {
      name: "chat completions require messages",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        }),
      },
      expectedPath: "messages",
    },
    {
      name: "chat completions validate nested message roles",
      path: "/v1/chat/completions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
          messages: [{ role: "invalid-role", content: "hello" }],
        }),
      },
      expectedPath: "messages.0.role",
    },
    {
      name: "image generations require a prompt",
      path: "/v1/images/generations",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ n: 1 }),
      },
      expectedPath: "prompt",
    },
    {
      name: "embeddings require input",
      path: "/v1/embeddings",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: "embeddings-model" }),
      },
      expectedPath: "input",
    },
  ];

  for (const validationCase of jsonValidationCases) {
    test(validationCase.name, async () => {
      await expectValidationFailure(
        validationCase.path,
        validationCase.init,
        validationCase.expectedPath,
      );
    });
  }

  test("audio transcriptions require a multipart file", async () => {
    const formData = new FormData();
    formData.append("model", "whisper-large-v3-turbo");

    await expectValidationFailure(
      "/v1/audio/transcriptions",
      { method: "POST", body: formData },
      "file",
    );
  });

  test("audio transcriptions reject repeated scalar multipart fields", async () => {
    const formData = new FormData();
    formData.append("file", new Blob(["audio"]), "audio.wav");
    formData.append("model", "whisper-a");
    formData.append("model", "whisper-b");

    await expectValidationFailure(
      "/v1/audio/transcriptions",
      { method: "POST", body: formData },
      "model",
    );
  });
});
