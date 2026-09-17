import { expect, spyOn, test } from "bun:test";
import { generationTools, generateVideo, runChat } from "./tools";
import {
  modelsSchema,
  SessionRequiredError,
  type Artifact,
  type Model,
} from "./client";

function fixture(
  id: string,
  kind: Model["catalog"]["kind"],
  capabilities: Model["catalog"]["capabilities"] = null,
): Model {
  return modelsSchema.parse({
    data: [
      {
        id,
        catalog: {
          name: id,
          kind,
          quantization: "test",
          features: ["tool-calling"],
          inputModalities: ["text"],
          outputModalities: ["text"],
          contextWindowTokens: null,
          capabilities,
        },
        device: { selected: true, installed: true, runtime: null },
      },
    ],
  }).data[0]!;
}
const chat = fixture("chat", "llm");
const image = fixture("image", "image");
const video = fixture("video", "video", {
  kind: "video",
  mode: "t2v",
  width: 256,
  height: 256,
  frames: 9,
  fps: 16,
  jobDeadlineMs: 5000,
  outputFormats: ["mp4"],
});
function stream(delta: unknown, onCancel = () => {}) {
  return new Response(
    new ReadableStream({
      start(controller) {
        controller.enqueue(
          new TextEncoder().encode(
            `data: ${JSON.stringify({ choices: [{ delta }] })}\n\ndata: [DONE]\n\n`,
          ),
        );
      },
      cancel: onCancel,
    }),
  );
}
function tool(name: string, args: string, id = "call") {
  return {
    tool_calls: [
      { index: 0, id, type: "function", function: { name, arguments: args } },
    ],
  };
}
function mockFetch(
  implementation: (
    input: Parameters<typeof fetch>[0],
    init?: Parameters<typeof fetch>[1],
  ) => Promise<Response>,
) {
  return spyOn(globalThis, "fetch").mockImplementation(
    Object.assign(implementation, { preconnect: fetch.preconnect }),
  );
}
test("aborted media stops the loop without returning pending tool protocol", async () => {
  const abort = new AbortController();
  let requests = 0;
  const artifacts: Artifact[] = [];
  const mock = mockFetch(async (_input, init) => {
    if (++requests === 1)
      return stream(tool("generate_image", '{"model":"image","prompt":"sun"}'));
    expect(init?.signal).toBe(abort.signal);
    abort.abort();
    throw abort.signal.reason;
  });
  try {
    await expect(
      runChat({
        model: chat,
        models: [image],
        connection: { kind: "api-key", key: "" },
        signal: abort.signal,
        messages: [],
        toolsEnabled: true,
        append: () => {},
        artifact: (a) => artifacts.push(a),
        register: () => {},
        warning: () => {},
      }),
    ).rejects.toThrow();
    expect(requests).toBe(2);
    expect(artifacts.at(-1)).toMatchObject({
      state: "error",
      detail: "Stopped",
    });
  } finally {
    mock.mockRestore();
  }
});
test("advertises only installed selected tools with t2v and tool-calling support", () => {
  const absent = fixture("absent", "tts");
  absent.device.installed = false;
  const s2v = fixture("s2v", "video", {
    ...video.catalog.capabilities,
    kind: "video",
    mode: "s2v",
    width: 256,
    height: 256,
    frames: 9,
    fps: 16,
    jobDeadlineMs: 5000,
    outputFormats: ["mp4"],
  });
  expect(
    generationTools([image, video, s2v, absent], chat).map(
      (t) => t.function.name,
    ),
  ).toEqual(["generate_image", "generate_video"]);
  expect(
    generationTools([image], {
      ...chat,
      catalog: { ...chat.catalog, features: [] },
    }),
  ).toEqual([]);
});
test.each([false, true])(
  "releases chat body before media and retains tool protocol without media bytes (session=%s)",
  async (session) => {
    const prefix = session ? "/app/api" : "";
    const requests: { path: string; body: unknown }[] = [];
    let released = false;
    const artifacts: Artifact[] = [];
    const urls: string[] = [];
    const mock = mockFetch(async (input, init) => {
      const path = String(input);
      expect(path.startsWith(`${prefix}/v1/`)).toBe(true);
      expect(new Headers(init?.headers).get("x-localbase-ui")).toBe(
        session ? "1" : null,
      );
      requests.push({ path, body: JSON.parse(String(init?.body)) });
      if (requests.length === 1)
        return stream(
          tool("generate_image", '{"model":"image","prompt":"sun"}'),
          () => {
            released = true;
          },
        );
      expect(released).toBe(true);
      if (path === `${prefix}/v1/images/generations`)
        return Response.json({ data: [{ b64_json: "aGVsbG8=" }] });
      return stream({ content: "Done" });
    });
    try {
      const protocol = await runChat({
        model: chat,
        models: [image],
        connection: session
          ? { kind: "session" }
          : { kind: "api-key", key: "key" },
        signal: new AbortController().signal,
        messages: [{ role: "user", content: "Draw the sun" }],
        toolsEnabled: true,
        append: () => {},
        artifact: (a) => artifacts.push(a),
        register: (url) => urls.push(url),
        warning: () => {},
      });
      expect(protocol.map((m) => m.role)).toEqual([
        "assistant",
        "tool",
        "assistant",
      ]);
      expect(JSON.stringify(requests[2]?.body)).toContain("tool_call_id");
      const assistant = protocol[0];
      const result = protocol[1];
      const id =
        assistant?.role === "assistant"
          ? assistant.tool_calls?.[0]?.id
          : undefined;
      expect(id).toMatch(/^[a-zA-Z0-9]{9}$/);
      expect(id).not.toBe("call");
      expect(result?.role === "tool" ? result.tool_call_id : undefined).toBe(
        id,
      );
      expect(artifacts.every((artifact) => artifact.id === id)).toBe(true);
      expect(JSON.stringify(requests[2]?.body)).not.toContain("blob:");
      expect(JSON.stringify(requests[2]?.body)).not.toContain("aGVsbG8=");
      expect(artifacts.at(-1)?.state).toBe("complete");
    } finally {
      mock.mockRestore();
      urls.forEach((url) => URL.revokeObjectURL(url));
    }
  },
);
test("browser IDs reserve history and allow raw IDs to repeat across rounds", async () => {
  const existingId = "123456789";
  const random = spyOn(crypto, "randomUUID")
    .mockReturnValueOnce("12345678-9000-4000-8000-000000000000")
    .mockReturnValueOnce("abcdefab-c000-4000-8000-000000000000")
    .mockReturnValueOnce("fedcbafe-d000-4000-8000-000000000000");
  let requests = 0;
  const artifacts: Artifact[] = [];
  const mock = mockFetch(async (_input, init) => {
    requests++;
    if (requests === 2) {
      const body = String(init?.body);
      expect(body).toContain('"id":"123456789"');
      expect(body).toContain('"id":"abcdefabc"');
      expect(body).toContain('"tool_call_id":"abcdefabc"');
      expect(body).not.toContain('"id":"1234567890"');
    }
    return requests === 3
      ? stream({ content: "Done" })
      : stream(tool("unknown", "{}", "1234567890"));
  });
  try {
    const protocol = await runChat({
      model: chat,
      models: [image],
      connection: { kind: "session" },
      signal: new AbortController().signal,
      messages: [
        {
          role: "assistant",
          content: null,
          tool_calls: [
            {
              id: existingId,
              type: "function",
              function: { name: "unknown", arguments: "{}" },
            },
          ],
        },
        { role: "tool", tool_call_id: existingId, content: "Error" },
      ],
      toolsEnabled: true,
      append: () => {},
      artifact: (a) => artifacts.push(a),
      register: () => {},
      warning: () => {},
    });
    expect(requests).toBe(3);
    expect(random).toHaveBeenCalledTimes(3);
    expect(
      protocol.flatMap((m) =>
        m.role === "assistant"
          ? (m.tool_calls ?? []).map((call) => call.id)
          : [],
      ),
    ).toEqual(["abcdefabc", "fedcbafed"]);
    expect(
      protocol.flatMap((m) => (m.role === "tool" ? [m.tool_call_id] : [])),
    ).toEqual(["abcdefabc", "fedcbafed"]);
    expect([...new Set(artifacts.map((a) => a.id))]).toEqual([
      "abcdefabc",
      "fedcbafed",
    ]);
  } finally {
    mock.mockRestore();
    random.mockRestore();
  }
});
test.each([502, 401, 403])(
  "post-image HTTP %s retains the artifact and classifies auth separately",
  async (status) => {
    let requests = 0;
    const artifacts: Artifact[] = [];
    const urls: string[] = [];
    const mock = mockFetch(async (input, init) => {
      expect(String(input).startsWith("/app/api/v1/")).toBe(true);
      expect(new Headers(init?.headers).get("x-localbase-ui")).toBe("1");
      expect(init?.credentials).toBe("same-origin");
      if (++requests === 1)
        return stream(
          tool("generate_image", '{"model":"image","prompt":"sun"}'),
        );
      if (requests === 2)
        return Response.json({ data: [{ b64_json: "aGVsbG8=" }] });
      return status === 502
        ? new Response("<html>Bad gateway</html>", {
            status,
            headers: { "content-type": "text/html" },
          })
        : Response.json(
            {
              error: { code: "ui_access_denied", message: "UI access denied." },
            },
            { status },
          );
    });
    try {
      const error = await runChat({
        model: chat,
        models: [image],
        connection: { kind: "session" },
        signal: new AbortController().signal,
        messages: [],
        toolsEnabled: true,
        append: () => {},
        artifact: (a) => artifacts.push(a),
        register: (url) => urls.push(url),
        warning: () => {},
      }).catch((error: unknown) => error);
      expect(requests).toBe(3);
      expect(error).toBeInstanceOf(Error);
      expect(error instanceof SessionRequiredError).toBe(status !== 502);
      if (status === 502)
        expect(error instanceof Error && error.message).toContain("502");
      expect(artifacts.at(-1)?.state).toBe("complete");
      expect(urls).toHaveLength(1);
    } finally {
      mock.mockRestore();
      urls.forEach((url) => URL.revokeObjectURL(url));
    }
  },
);
test.each([
  ["unknown_tool", "{}", "Unknown generation tool"],
  ["generate_video", '{"model":"video","prompt":"sun"}', "Unavailable tool"],
  [
    "generate_image",
    '{"model":"uninstalled","prompt":"sun"}',
    "not selected and installed",
  ],
  ["generate_image", '{"model":"image",', "Malformed tool arguments"],
  [
    "generate_image",
    '{"model":"image","prompt":"sun","url":"https://example.com"}',
    "Malformed tool arguments",
  ],
])("returns explicit tool errors for %s", async (name, args, expected) => {
  let requests = 0;
  const artifacts: Artifact[] = [];
  const mock = mockFetch(async () =>
    ++requests === 1
      ? stream(tool(name, args))
      : stream({ content: "Unable to create it" }),
  );
  try {
    const protocol = await runChat({
      model: chat,
      models: [image],
      connection: { kind: "api-key", key: "" },
      signal: new AbortController().signal,
      messages: [],
      toolsEnabled: true,
      append: () => {},
      artifact: (a) => artifacts.push(a),
      register: () => {},
      warning: () => {},
    });
    expect(requests).toBe(2);
    expect(JSON.stringify(protocol)).toContain(expected);
    expect(artifacts.at(-1)?.state).toBe("error");
  } finally {
    mock.mockRestore();
  }
});
test("stops at four model rounds without executing a last-round tool", async () => {
  let rounds = 0;
  const mock = mockFetch(async () =>
    stream(tool("unknown", "{}", `call-${++rounds}`)),
  );
  try {
    await expect(
      runChat({
        model: chat,
        models: [image],
        connection: { kind: "api-key", key: "" },
        signal: new AbortController().signal,
        messages: [],
        toolsEnabled: true,
        append: () => {},
        artifact: () => {},
        register: () => {},
        warning: () => {},
      }),
    ).rejects.toThrow("limit reached");
    expect(rounds).toBe(4);
  } finally {
    mock.mockRestore();
  }
});
test.each([false, true])(
  "cancellation after submission uses original owner, tries DELETE after failed cancel (session=%s)",
  async (session) => {
    const prefix = session ? "/app/api" : "";
    const abort = new AbortController();
    const paths: string[] = [];
    const warnings: string[] = [];
    const id = "00000000-0000-4000-8000-000000000000";
    const mock = mockFetch(async (input, init) => {
      paths.push(`${init?.method ?? "GET"} ${String(input)}`);
      expect(new Headers(init?.headers).get("authorization")).toBe(
        session ? null : "Bearer owner",
      );
      expect(init?.credentials).toBe("same-origin");
      expect(new Headers(init?.headers).get("x-localbase-ui")).toBe(
        session ? "1" : null,
      );
      if (paths.length === 1) {
        abort.abort();
        return Response.json({ id, status: "queued" });
      }
      if (String(input).endsWith("/cancel"))
        return Response.json(
          { error: { message: "cancel unavailable" } },
          { status: 503 },
        );
      return new Response(null, { status: 204 });
    });
    try {
      await expect(
        generateVideo({
          model: video,
          connection: session
            ? { kind: "session" }
            : { kind: "api-key", key: "owner" },
          signal: abort.signal,
          prompt: "sun",
          progress: () => {},
          warning: (message) => warnings.push(message),
        }),
      ).rejects.toThrow();
      expect(paths).toEqual([
        `POST ${prefix}/v1/videos`,
        `POST ${prefix}/v1/videos/${id}/cancel`,
        `DELETE ${prefix}/v1/videos/${id}`,
      ]);
      expect(warnings.join()).toContain("cancellation");
    } finally {
      mock.mockRestore();
    }
  },
);
test("expired session during a tool stops before another model round", async () => {
  let requests = 0;
  const mock = mockFetch(async () =>
    ++requests === 1
      ? stream(tool("generate_image", '{"model":"image","prompt":"sun"}'))
      : new Response(null, { status: 401 }),
  );
  try {
    await expect(
      runChat({
        model: chat,
        models: [image],
        connection: { kind: "session" },
        signal: new AbortController().signal,
        messages: [],
        toolsEnabled: true,
        append: () => {},
        artifact: () => {},
        register: () => {},
        warning: () => {},
      }),
    ).rejects.toBeInstanceOf(SessionRequiredError);
    expect(requests).toBe(2);
  } finally {
    mock.mockRestore();
  }
});
test.each(["video/mp4", "video/x-msvideo", ""])(
  "validates delivered video type %s and retains MP4 when cleanup fails",
  async (mimeType) => {
    const id = "00000000-0000-4000-8000-000000000000";
    const warnings: string[] = [];
    const mock = mockFetch(async (input, init) => {
      if (String(input) === "/v1/videos")
        return Response.json({
          id,
          status: "completed",
          content_type: mimeType,
        });
      if (String(input).endsWith("/content"))
        return new Response("mp4", {
          headers: { "content-type": mimeType },
        });
      expect(init?.method).toBe("DELETE");
      return new Response(null, { status: 503 });
    });
    try {
      const result = generateVideo({
        model: video,
        connection: { kind: "api-key", key: "" },
        signal: new AbortController().signal,
        prompt: "sun",
        progress: () => {},
        warning: (message) => warnings.push(message),
      });
      if (mimeType === "video/mp4") {
        const video = await result;
        expect(video.format).toBe("mp4");
        expect(video.blob.type).toBe("video/mp4");
        expect(await video.blob.text()).toBe("mp4");
      } else {
        await expect(result).rejects.toThrow("Unsupported video response type");
      }
      expect(warnings.join()).toContain("cleanup");
    } finally {
      mock.mockRestore();
    }
  },
);
