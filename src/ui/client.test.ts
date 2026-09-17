import { describe, expect, spyOn, test } from "bun:test";
import type { ModelMetadata } from "../domains/models/model-metadata";
import {
  api,
  availableModels,
  modelsSchema,
  streamText,
  readHistory,
  writeHistory,
  type Conversation,
} from "./client";

function streaming(parts: Uint8Array[]) {
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        for (const part of parts) controller.enqueue(part);
        controller.close();
      },
    }),
  );
}
describe("playground client boundaries", () => {
  test("sends credentials only in headers and preserves same-origin sessions without caching", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({}),
    );
    try {
      await api("/_localbase/models", "fixture-key");
      const [path, options] = fetchMock.mock.calls[0] ?? [];
      expect(path).toBe("/_localbase/models");
      expect(options?.credentials).toBe("same-origin");
      expect(options?.cache).toBe("no-store");
      expect(new Headers(options?.headers).get("authorization")).toBe(
        "Bearer fixture-key",
      );
      expect(new Headers(options?.headers).get("x-api-key")).toBe(
        "fixture-key",
      );
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("device-local history roundtrips text and excludes media and credentials", () => {
    const descriptor = Object.getOwnPropertyDescriptor(
      globalThis,
      "localStorage",
    );
    let saved: string | null = null;
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      value: {
        getItem: () => saved,
        setItem: (_key: string, value: string) => {
          saved = value;
        },
      },
    });
    try {
      expect(readHistory()).toBeNull();
      const conversation: Conversation = {
        id: "one",
        title: "A chat",
        workspace: "lab",
        mode: "llm",
        model: "test",
        messages: [
          {
            id: "message",
            role: "assistant",
            text: "hello",
            media: { kind: "audio", url: "blob:private-audio" },
            protocol: [
              {
                role: "assistant",
                content: null,
                tool_calls: [
                  {
                    id: "pending-tool",
                    type: "function",
                    function: { name: "generate_image", arguments: "{}" },
                  },
                ],
              },
            ],
            artifacts: [
              {
                id: "pending-tool",
                label: "Image",
                state: "working",
                detail: "Starting",
              },
            ],
          },
        ],
      };
      writeHistory([Object.assign(conversation, { apiKey: "fixture-secret" })]);
      expect(readHistory()).toEqual([
        {
          id: "one",
          title: "A chat",
          workspace: "lab",
          mode: "llm",
          model: "test",
          messages: [{ id: "message", role: "assistant", text: "hello" }],
        },
      ]);
      expect(saved).not.toContain("fixture-secret");
      expect(saved).not.toContain("blob:");
      expect(saved).not.toContain("pending-tool");
    } finally {
      if (descriptor)
        Object.defineProperty(globalThis, "localStorage", descriptor);
      else Reflect.deleteProperty(globalThis, "localStorage");
    }
  });
  test("parses fragmented UTF-8 SSE with CRLF and ignores keepalives", async () => {
    const bytes = new TextEncoder().encode(
      ': keepalive\r\n\r\ndata: {"choices":[{"delta":{"content":"Hello 🌱"}}]}\r\n\r\ndata: [DONE]\r\n\r\n',
    );
    let text = "";
    await streamText(
      streaming(Array.from(bytes, (byte) => new Uint8Array([byte]))),
      (chunk) => {
        text += chunk;
      },
    );
    expect(text).toBe("Hello 🌱");
  });
  test("reports stream errors and interrupted responses", async () => {
    const encode = (text: string) =>
      streaming([new TextEncoder().encode(text)]);
    await expect(
      streamText(
        encode('data: {"error":{"message":"Capacity exceeded"}}\n\n'),
        () => {},
      ),
    ).rejects.toThrow("Capacity exceeded");
    await expect(
      streamText(
        encode('data: {"choices":[{"delta":{"content":"partial"}}]}\n\n'),
        () => {},
      ),
    ).rejects.toThrow("before the response finished");
  });
  test("assembles fragmented tool calls by index and bounds the number of calls", async () => {
    const encode = (deltas: unknown[]) =>
      streaming([
        new TextEncoder().encode(
          deltas
            .map(
              (delta) =>
                `data: ${JSON.stringify({ choices: [{ index: 0, delta }] })}\n\n`,
            )
            .join("") + "data: [DONE]\n\n",
        ),
      ]);
    const calls = await streamText(
      encode([
        {
          tool_calls: [
            {
              index: 1,
              id: "second",
              function: { name: "generate_image", arguments: '{"model":' },
            },
          ],
        },
        {
          content: "Creating",
          tool_calls: [
            {
              index: 0,
              id: "fir",
              function: { name: "generate_", arguments: '{"prompt":' },
            },
          ],
        },
        {
          tool_calls: [
            { index: 1, function: { arguments: '"image","prompt":"sun"}' } },
            {
              index: 0,
              id: "st",
              function: { name: "image", arguments: '"rain","model":"image"}' },
            },
          ],
        },
      ]),
      () => {},
    );
    expect(calls).toEqual([
      {
        id: "first",
        type: "function",
        function: {
          name: "generate_image",
          arguments: '{"prompt":"rain","model":"image"}',
        },
      },
      {
        id: "second",
        type: "function",
        function: {
          name: "generate_image",
          arguments: '{"model":"image","prompt":"sun"}',
        },
      },
    ]);
    await expect(
      streamText(
        encode([{ tool_calls: [{ index: 4, id: "fifth" }] }]),
        () => {},
      ),
    ).rejects.toThrow();
    await expect(
      streamText(
        encode([{ tool_calls: [{ index: 0, function: { name: "unknown" } }] }]),
        () => {},
      ),
    ).rejects.toThrow("Malformed tool call");
  });
  test("parses mixed capabilities, excludes embeddings, and prefers assigned chat runtimes", () => {
    const embedding = {
      kind: "embedding",
      dimensions: { minimum: 32, maximum: 1024 },
    } satisfies ModelMetadata["catalog"]["capabilities"];
    const speech = {
      kind: "speech",
      outputFormats: ["wav"],
      voice: {
        selection: "catalog-reference",
        requestValues: ["default", "harbor"],
        defaultRequestValue: "default",
        references: [
          {
            name: "harbor",
            license: "CC0-1.0",
            provenanceUrl: "https://example.com/harbor",
          },
        ],
      },
      residency: "cold-per-request",
    } satisfies ModelMetadata["catalog"]["capabilities"];
    const common = {
      catalog: {
        name: "Test",
        kind: "llm",
        quantization: "Q4",
        inputModalities: ["text"],
        outputModalities: ["text"],
        contextWindowTokens: null,
        capabilities: null,
      },
    };
    const models = modelsSchema.parse({
      data: [
        {
          id: "embedding",
          catalog: { ...common.catalog, capabilities: embedding },
          device: {
            selected: true,
            installed: true,
            runtime: { configured: true, state: "idle" },
          },
        },
        {
          id: "speech",
          catalog: { ...common.catalog, kind: "tts", capabilities: speech },
          device: { selected: true, installed: true, runtime: null },
        },
        {
          ...common,
          id: "lazy",
          device: { selected: true, installed: true, runtime: null },
        },
        {
          ...common,
          id: "assigned",
          device: {
            selected: true,
            installed: true,
            runtime: { configured: true, state: "idle" },
          },
        },
        {
          ...common,
          id: "missing",
          device: { selected: true, installed: false, runtime: null },
        },
        {
          ...common,
          id: "unselected",
          device: { selected: false, installed: true, runtime: null },
        },
      ],
    }).data;
    expect(availableModels(models, "llm").map((m) => m.id)).toEqual([
      "assigned",
      "lazy",
    ]);
    expect(availableModels(models, "image")).toEqual([]);
    const speechModels = availableModels(models, "tts");
    expect(speechModels.map((m) => m.id)).toEqual(["speech"]);
    expect(speechModels[0]?.catalog.capabilities).toEqual({
      kind: "speech",
      voice: {
        requestValues: ["default", "harbor"],
        defaultRequestValue: "default",
      },
    });
    for (const capabilities of [
      { kind: "unsupported" },
      { kind: "embedding", dimensions: { minimum: 0, maximum: 1024 } },
      { kind: "embedding" },
      { kind: "speech" },
      { ...speech, voice: { ...speech.voice, requestValues: ["unknown"] } },
    ]) {
      expect(
        modelsSchema.safeParse({
          data: [
            {
              ...common,
              id: "invalid",
              catalog: { ...common.catalog, capabilities },
              device: { selected: true, installed: true, runtime: null },
            },
          ],
        }).success,
      ).toBe(false);
    }
  });
});
