import { describe, expect, spyOn, test } from "bun:test";
import type { ModelMetadata } from "../domains/models/model-metadata";
import {
  api,
  ApiError,
  discardLegacyFragmentCredential,
  logoutSession,
  readSession,
  redirectToSignIn,
  sessionConnection,
  SessionRequiredError,
  availableModels,
  catalogModels,
  canUseMode,
  hasPermission,
  modelMemorySummary,
  modelsSchema,
  permittedModes,
  permittedUiModes,
  streamFinishGraceMs,
  streamText,
  readHistory,
  writeHistory,
  type Conversation,
  codeLanguage,
  embeddingSummary,
  embeddingSummaryText,
  modelModalities,
  modelName,
  speechVoiceCount,
  type Model,
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
  test("maps inference permissions to the visible UI modes", () => {
    const permissions = ["inference:chat", "inference:video"] as const;
    expect(permittedModes(permissions)).toEqual(["llm", "video"]);
    expect(canUseMode(permissions, "video")).toBe(true);
    expect(canUseMode(permissions, "image")).toBe(false);
    expect(hasPermission(permissions, "inference:speech")).toBe(false);
    expect(permittedModes(null)).toEqual([]);
    expect(permittedUiModes(permissions)).toEqual([]);
    expect(permittedUiModes([...permissions, "models:read"])).toEqual([
      "llm",
      "video",
    ]);
  });

  test("discards obsolete key fragments without accepting their value", () => {
    const location = new URL(
      "https://localbase.example/app?view=chat#key=obsolete-secret",
    );
    const history = {
      state: { view: "chat" },
      replaceState(data: unknown, _unused: string, url?: string | URL | null) {
        expect(data).toBe(this.state);
        location.href = new URL(String(url), location).href;
      },
    };
    discardLegacyFragmentCredential({ location, history });
    expect(location.href).toBe("https://localbase.example/app?view=chat");
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
  test("requires a verified browser session", async () => {
    const fetchMock = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        Response.json({
          authenticated: true,
          permissions: ["inference:chat", "models:manage"],
          verifiedEmail: "person@example.com",
          logoutUrl: "/cdn-cgi/access/logout",
        }),
      )
      .mockResolvedValueOnce(
        Response.json({ authenticated: false, mode: "api-key" }),
      );
    try {
      const session = await readSession();
      expect(sessionConnection(session)).toEqual({
        kind: "session",
        permissions: ["inference:chat", "models:manage"],
        verifiedEmail: "person@example.com",
        logoutUrl: "/cdn-cgi/access/logout",
      });
      const [path, options] = fetchMock.mock.calls[0] ?? [];
      expect(path).toBe("/app/session");
      expect(options).toMatchObject({
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
      });
      expect(new Headers(options?.headers).get("x-localbase-ui")).toBe("1");
      await expect(readSession()).rejects.toThrow(/sign-in/i);
      expect(sessionConnection({ kind: "checking" })).toBeNull();
      expect(
        sessionConnection({ kind: "error", message: "Expired" }),
      ).toBeNull();
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("signs out through the same-origin session endpoint", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(null, { status: 204 }),
    );
    try {
      await logoutSession();
      const [path, options] = fetchMock.mock.calls[0] ?? [];
      expect(path).toBe("/app/logout");
      expect(options).toMatchObject({
        method: "POST",
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
      });
      expect(new Headers(options?.headers).get("x-localbase-ui")).toBe("1");
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
  test("rejects incomplete bootstrap responses", async () => {
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
  test("unauthenticated API requests require sign-in, not an API key", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(null, { status: 401 }),
    );
    try {
      await expect(
        api("/_localbase/models", { kind: "session" }),
      ).rejects.toBeInstanceOf(SessionRequiredError);
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("permission denial does not expire a valid browser session", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json(
        {
          error: {
            code: "insufficient_permissions",
            message: "Permission required: models:manage.",
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
      ).rejects.toThrow("Permission required: models:manage.");
    } finally {
      fetchMock.mockRestore();
    }
  });
  test("gateway errors keep their status and code for the UI", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json(
        {
          error: {
            code: "hub_rate_limited",
            message: "Hugging Face answered 429 (rate limited).",
          },
        },
        { status: 502 },
      ),
    );
    try {
      const error = await api("/_localbase/model-discovery", {
        kind: "session",
      }).catch((error: unknown) => error);
      expect(error).toBeInstanceOf(ApiError);
      expect(error).toMatchObject({
        status: 502,
        code: "hub_rate_limited",
        message: "Hugging Face answered 429 (rate limited).",
      });
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
  test("completes a finished reply whose stream tail never arrives", async () => {
    const encoder = new TextEncoder();
    let cancelled = false;
    const response = new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(
            encoder.encode(
              [
                { choices: [{ delta: { reasoning_content: "Plan." } }] },
                { choices: [{ delta: { content: "- one\n- two" } }] },
                { choices: [{ delta: {}, finish_reason: "stop" }] },
                { choices: [], usage: { completion_tokens: 3 } },
              ]
                .map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`)
                .join(""),
            ),
          );
          // No [DONE] and no close: the connection stays open.
        },
        cancel() {
          cancelled = true;
        },
      }),
    );
    let text = "";
    let thought = "";
    const started = Date.now();
    await streamText(
      response,
      (chunk) => (text += chunk),
      (chunk) => (thought += chunk),
    );
    expect(Date.now() - started).toBeGreaterThanOrEqual(
      streamFinishGraceMs - 50,
    );
    expect(text).toBe("- one\n- two");
    expect(thought).toBe("Plan.");
    expect(cancelled).toBe(true);
  });
  test("accepts finish chunks without a delta and chunks without choices", async () => {
    let text = "";
    await streamText(
      streaming([
        new TextEncoder().encode(
          'data: {"choices":[{"delta":{"content":"ok"}}]}\n\ndata: {"choices":[{"index":0,"finish_reason":"stop"}]}\n\ndata: {"usage":{"total_tokens":2}}\n\ndata: [DONE]\n\n',
        ),
      ]),
      (chunk) => (text += chunk),
    );
    expect(text).toBe("ok");
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
        qualificationState: "qualified",
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

test("redirects an expired session to sign-in without looping", () => {
  const assigned: string[] = [];
  const storage = new Map<string, string>();
  const original = {
    location: globalThis.location,
    sessionStorage: globalThis.sessionStorage,
  };
  const install = (search: string) => {
    Object.defineProperty(globalThis, "location", {
      configurable: true,
      value: { search, assign: (url: string) => assigned.push(url) },
    });
  };
  Object.defineProperty(globalThis, "sessionStorage", {
    configurable: true,
    value: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => storage.set(key, value),
    },
  });
  try {
    install("");
    expect(redirectToSignIn()).toBe(true);
    expect(redirectToSignIn()).toBe(false);
    expect(assigned).toEqual(["/app/login"]);

    storage.clear();
    install("?signin=failed");
    expect(redirectToSignIn()).toBe(false);
    expect(assigned).toEqual(["/app/login"]);
  } finally {
    for (const [key, value] of Object.entries(original))
      Object.defineProperty(globalThis, key, { configurable: true, value });
  }
});

describe("display helpers", () => {
  const card = (catalog: Record<string, unknown>) =>
    modelsSchema.parse({
      host: {
        memory: {
          kind: "unified",
          system: { capacityBytes: 1024, availableBytes: 512 },
          accelerators: [],
        },
      },
      data: [
        {
          id: "image:flux/q4",
          catalog: {
            name: "flux",
            kind: "image",
            quantization: "Q4",
            qualificationState: "qualified",
            memory: { minimumVramEstimateGb: 1, storageEstimateGb: 1 },
            inputModalities: ["text"],
            outputModalities: ["image"],
            contextWindowTokens: null,
            capabilities: null,
            ...catalog,
          },
          device: { selected: true, installed: true, runtime: null },
        },
      ],
    }).data[0]!;

  test("names models by their display name, falling back to the family", () => {
    expect(modelName(card({ displayName: "FLUX.1 Schnell" }))).toBe(
      "FLUX.1 Schnell",
    );
    expect(modelName(card({}))).toBe("flux");
  });

  test("lists image input for image-to-image models", () => {
    expect(modelModalities(card({}))).toBe("text → image");
    expect(modelModalities(card({ features: ["image-to-image"] }))).toBe(
      "text, image → image",
    );
  });

  test("summarises an embedding vector", () => {
    const summary = embeddingSummary([3, 4, 0, 0, 0, 0, 0, 0, 0, 1], 3);
    expect(summary.dimensions).toBe(10);
    expect(summary.preview).toEqual([3, 4, 0]);
    expect(summary.norm).toBeCloseTo(Math.sqrt(26));
    expect(embeddingSummaryText("embed", [3, 4])).toBe(
      "Embedding from embed: 2 dimensions, norm 5.0000.",
    );
    expect(embeddingSummary([])).toEqual({
      dimensions: 0,
      preview: [],
      norm: 0,
    });
  });

  test("reads the code block language from its class", () => {
    expect(codeLanguage("language-ts")).toBe("ts");
    expect(codeLanguage("hljs language-python")).toBe("python");
    expect(codeLanguage("language-")).toBeUndefined();
    expect(codeLanguage(undefined)).toBeUndefined();
  });

  test("counts selectable speech voices", () => {
    const speech = (requestValues: string[]) =>
      ({
        catalog: { capabilities: { kind: "speech", voice: { requestValues } } },
      }) as unknown as Pick<Model, "catalog">;
    expect(speechVoiceCount(speech(["default", "harbor"]))).toBe(2);
    expect(speechVoiceCount(speech(["default"]))).toBe(1);
    expect(speechVoiceCount(card({}))).toBe(0);
    expect(speechVoiceCount(undefined)).toBe(0);
  });
});
