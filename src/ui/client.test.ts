import { describe, expect, spyOn, test } from "bun:test";
import type { ModelMetadata } from "../domains/models/model-metadata";
import {
  api,
  consumeFragmentKey,
  createUiId,
  readSession,
  sessionConnection,
  SessionRequiredError,
  availableModels,
  catalogModels,
  modelMemorySummary,
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
  test("consumes a fragment key once, clearing it before authenticated requests", async () => {
    const location = new URL(
      "http://192.168.1.2:8080/app?view=chat#key=lb_fixture%2Dkey",
    );
    const state = { view: "chat" };
    const history = {
      state,
      replaceState(data: unknown, _unused: string, url?: string | URL | null) {
        expect(data).toBe(state);
        expect(url).toBe("/app?view=chat");
        location.href = new URL(String(url), location).href;
      },
    };
    const replace = spyOn(history, "replaceState");
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({ authenticated: false, mode: "api-key" }),
    );
    try {
      const key = consumeFragmentKey({ location, history });
      expect(key).toBe("lb_fixture-key");
      expect(location.hash).toBe("");
      expect(fetchMock).not.toHaveBeenCalled();
      expect(consumeFragmentKey({ location, history })).toBe("");
      expect(replace).toHaveBeenCalledTimes(1);
      const connection = sessionConnection(await readSession(), key);
      expect(connection).toEqual({ kind: "api-key", key: "lb_fixture-key" });
      if (!connection) throw new Error("Missing fixture connection");
      await api("/_localbase/models", connection);
      const [path, options] = fetchMock.mock.calls.at(-1) ?? [];
      expect(path).toBe("/_localbase/models");
      expect(new Headers(options?.headers).get("authorization")).toBe(
        "Bearer lb_fixture-key",
      );
    } finally {
      replace.mockRestore();
      fetchMock.mockRestore();
    }
  });
  test("clears malformed and duplicate fragments without logging credentials", () => {
    const logs = [
      spyOn(console, "log"),
      spyOn(console, "warn"),
      spyOn(console, "error"),
    ];
    try {
      for (const fragment of [
        "#key=",
        "#key",
        "#other=fixture-key",
        "#key=%",
        "#key=%FF",
        "#key=%20",
        "#key=fixture%0Akey",
        "#key=fixture-key&key=other",
        "#key=fixture-key&key=fixture-key",
        "#key=fixture-key&other=value",
      ]) {
        const location = new URL(`http://192.168.1.2:8080/app${fragment}`);
        const history = {
          state: null,
          replaceState(
            _data: unknown,
            _unused: string,
            url?: string | URL | null,
          ) {
            location.href = new URL(String(url), location).href;
          },
        };
        expect(consumeFragmentKey({ location, history })).toBe("");
        expect(location.hash).toBe("");
      }
      for (const log of logs) expect(log).not.toHaveBeenCalled();
    } finally {
      for (const log of logs) log.mockRestore();
    }
  });
  test("creates secure UUIDs when HTTP browsers omit randomUUID", () => {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, "crypto");
    const source = crypto;
    let calls = 0;
    Object.defineProperty(globalThis, "crypto", {
      configurable: true,
      value: {
        getRandomValues(bytes: Uint8Array) {
          calls++;
          return source.getRandomValues(bytes);
        },
      },
    });
    try {
      const ids = Array.from({ length: 3 }, () => createUiId());
      for (const id of ids)
        expect(id).toMatch(
          /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
        );
      expect(new Set(ids).size).toBe(3);
      expect(calls).toBe(3);
    } finally {
      if (descriptor) Object.defineProperty(globalThis, "crypto", descriptor);
    }
  });
  test("rejects external or noncanonical paths before sending credentials", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({}),
    );
    try {
      for (const path of [
        "https://example.com",
        "//example.com",
        "/\n/example.com",
        "/../v1/chat/completions",
        "/%2e%2e/v1/videos",
      ]) {
        await expect(api(path, { kind: "session" })).rejects.toThrow(
          "same-origin",
        );
      }
      expect(fetchMock).not.toHaveBeenCalled();
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("bootstraps verified sessions or explicit manual mode without retaining credentials", async () => {
    const fetchMock = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(Response.json({ authenticated: true }))
      .mockResolvedValueOnce(
        Response.json({ authenticated: false, mode: "api-key" }),
      );
    try {
      const session = await readSession();
      expect(sessionConnection(session, "stale-key")).toEqual({
        kind: "session",
      });
      const [path, options] = fetchMock.mock.calls[0] ?? [];
      expect(path).toBe("/app/session");
      expect(options).toMatchObject({
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
      });
      expect(new Headers(options?.headers).get("x-localbase-ui")).toBe("1");
      const manual = await readSession();
      expect(sessionConnection(manual, "")).toBeNull();
      expect(sessionConnection(manual, "   ")).toBeNull();
      expect(sessionConnection(manual, "fixture-key")).toEqual({
        kind: "api-key",
        key: "fixture-key",
      });
      expect(sessionConnection({ kind: "checking" }, "stale-key")).toBeNull();
      expect(
        sessionConnection({ kind: "error", message: "Expired" }, "stale-key"),
      ).toBeNull();
    } finally {
      fetchMock.mockRestore();
    }
  });
  test.each([401, 403, 500, 404])(
    "session bootstrap %s never falls back to API keys",
    async (status) => {
      const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
        new Response(null, { status }),
      );
      try {
        await expect(readSession()).rejects.toThrow(/sign-in/i);
      } finally {
        fetchMock.mockRestore();
      }
    },
  );
  test("rejects incomplete bootstrap responses instead of selecting manual mode", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({ authenticated: false }),
    );
    try {
      await expect(readSession()).rejects.toThrow(/sign-in/i);
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("session requests prefix every API path and send only session headers", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({}),
    );
    const id = "00000000-0000-4000-8000-000000000000";
    try {
      for (const path of [
        "/_localbase/models",
        "/v1/chat/completions",
        "/v1/images/generations",
        "/v1/audio/speech",
        "/v1/audio/transcriptions",
        "/v1/embeddings",
        "/v1/videos",
        `/v1/videos/${id}`,
        `/v1/videos/${id}/content`,
        `/v1/videos/${id}/cancel`,
      ]) {
        await api(
          path,
          { kind: "session" },
          { headers: { authorization: "Bearer stale", "x-api-key": "stale" } },
        );
        const [actual, options] = fetchMock.mock.calls.at(-1) ?? [];
        expect(actual).toBe(`/app/api${path}`);
        const headers = new Headers(options?.headers);
        expect(headers.get("x-localbase-ui")).toBe("1");
        expect(headers.has("authorization")).toBe(false);
        expect(headers.has("x-api-key")).toBe(false);
        expect(options).toMatchObject({
          credentials: "same-origin",
          redirect: "error",
          cache: "no-store",
        });
      }
    } finally {
      fetchMock.mockRestore();
    }
  });
  test.each([401, 403])(
    "session API %s requests sign-in, not an API key",
    async (status) => {
      const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
        new Response(null, { status }),
      );
      try {
        await expect(
          api("/_localbase/models", { kind: "session" }),
        ).rejects.toBeInstanceOf(SessionRequiredError);
      } finally {
        fetchMock.mockRestore();
      }
    },
  );
  test("management permission denial does not expire a valid browser session", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json(
        {
          error: {
            code: "model_management_denied",
            message: "Model management access denied.",
          },
        },
        { status: 403 },
      ),
    );
    try {
      await expect(
        api(
          "/_localbase/model-management",
          { kind: "session" },
          { method: "POST" },
        ),
      ).rejects.toThrow("Model management access denied.");
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("unexpected HTML and network failures do not assert session expiry", async () => {
    const fetchMock = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        new Response("login", { headers: { "content-type": "text/html" } }),
      )
      .mockRejectedValueOnce(new TypeError("Failed to fetch"));
    try {
      for (const message of [
        "unexpected HTML",
        "Could not reach the gateway",
      ]) {
        const error = await api("/_localbase/models", {
          kind: "session",
        }).catch((error: unknown) => error);
        expect(error).toBeInstanceOf(Error);
        expect(error).not.toBeInstanceOf(SessionRequiredError);
        expect(error instanceof Error && error.message).toContain(message);
      }
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("sends credentials only in headers and preserves same-origin sessions without caching", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({}),
    );
    try {
      await api("/_localbase/models", { kind: "api-key", key: "fixture-key" });
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
            attachments: [
              {
                id: "file",
                kind: "text",
                name: "private.txt",
                size: 7,
                text: "private attachment contents",
              },
            ],
            media: { kind: "video", url: "blob:private-video", format: "mp4" },
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
          messages: [
            {
              id: "message",
              role: "assistant",
              text: "hello",
              attachmentsMissing: true,
            },
          ],
        },
      ]);
      expect(saved).not.toContain("fixture-secret");
      expect(saved).not.toContain("blob:");
      expect(saved).not.toContain("pending-tool");
      expect(saved).not.toContain("private attachment");
      expect(saved).not.toContain("private.txt");
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
    await expect(
      streamText(
        encode([
          {
            tool_calls: [
              {
                index: 0,
                id: "duplicate",
                function: { name: "generate_image", arguments: "{}" },
              },
              {
                index: 1,
                id: "duplicate",
                function: { name: "generate_image", arguments: "{}" },
              },
            ],
          },
        ]),
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
        memory: { minimumVramEstimateGb: 1, storageEstimateGb: 1 },
        inputModalities: ["text"],
        outputModalities: ["text"],
        contextWindowTokens: null,
        capabilities: null,
      },
    };
    const host = {
      memory: {
        kind: "unified",
        system: {
          capacityBytes: 64 * 1024 ** 3,
          availableBytes: 37.5 * 1024 ** 3,
        },
        accelerators: [],
      },
    };
    const parsed = modelsSchema.parse({
      host,
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
    });
    const models = parsed.data;
    expect(availableModels(models, "llm").map((m) => m.id)).toEqual([
      "assigned",
      "lazy",
    ]);
    expect(availableModels(models, "image")).toEqual([]);
    expect(catalogModels(models, "llm").map((model) => model.id)).toEqual([
      "assigned",
      "lazy",
      "unselected",
      "missing",
    ]);
    expect(catalogModels(models, "embedding").map((model) => model.id)).toEqual(
      ["embedding"],
    );
    const first = models[0];
    if (!first) throw new Error("Expected model fixture.");
    expect(modelMemorySummary(first, parsed.host.memory)).toBe(
      "Needs ~1 GB · 37.5 GB available",
    );
    expect(modelMemorySummary(first, null)).toBe(
      "Needs ~1 GB · memory availability unknown",
    );
    expect(
      modelMemorySummary(first, {
        kind: "discrete",
        system: {
          capacityBytes: 64 * 1024 ** 3,
          availableBytes: 40 * 1024 ** 3,
        },
        accelerators: [
          {
            capacityBytes: 32 * 1024 ** 3,
            availableBytes: 23.25 * 1024 ** 3,
          },
        ],
      }),
    ).toBe("Needs ~1 GB · 23.3 GB available");
    for (const accelerators of [
      [],
      [
        { capacityBytes: 16 * 1024 ** 3, availableBytes: 8 * 1024 ** 3 },
        { capacityBytes: 16 * 1024 ** 3, availableBytes: 8 * 1024 ** 3 },
      ],
    ]) {
      expect(
        modelMemorySummary(first, {
          kind: "discrete",
          system: {
            capacityBytes: 64 * 1024 ** 3,
            availableBytes: 40 * 1024 ** 3,
          },
          accelerators,
        }),
      ).toBe("Needs ~1 GB · memory availability unknown");
    }
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
          host,
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
