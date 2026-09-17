import { describe, expect, spyOn, test } from "bun:test";
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
        mode: "llm",
        model: "test",
        messages: [
          {
            id: "message",
            role: "assistant",
            text: "hello",
            media: { kind: "audio", url: "blob:private-audio" },
          },
        ],
      };
      writeHistory([Object.assign(conversation, { apiKey: "fixture-secret" })]);
      expect(readHistory()).toEqual([
        {
          id: "one",
          title: "A chat",
          mode: "llm",
          model: "test",
          messages: [{ id: "message", role: "assistant", text: "hello" }],
        },
      ]);
      expect(saved).not.toContain("fixture-secret");
      expect(saved).not.toContain("blob:");
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
  test("keeps lazy models and prefers the assigned runtime without claiming readiness", () => {
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
  });
});
