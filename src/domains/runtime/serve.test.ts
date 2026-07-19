import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  startGatewayFixture,
  type GatewayFixture,
} from "../../test/gateway-fixture";

type ValidationCase = {
  name: string;
  path: string;
  init: RequestInit;
  expectedPath: string;
};

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
});
