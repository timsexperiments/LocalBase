import { expect, spyOn, test } from "bun:test";
import { generationTools, generateVideo, runChat } from "./tools";
import { modelsSchema, type Artifact, type Model } from "./client";

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
  outputFormats: ["avi"],
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
        key: "",
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
    outputFormats: ["avi"],
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
test("releases chat body before media and retains tool protocol without media bytes", async () => {
  const requests: { path: string; body: unknown }[] = [];
  let released = false;
  const artifacts: Artifact[] = [];
  const urls: string[] = [];
  const mock = mockFetch(async (input, init) => {
    const path = String(input);
    requests.push({ path, body: JSON.parse(String(init?.body)) });
    if (requests.length === 1)
      return stream(
        tool("generate_image", '{"model":"image","prompt":"sun"}'),
        () => {
          released = true;
        },
      );
    expect(released).toBe(true);
    if (path === "/v1/images/generations")
      return Response.json({ data: [{ b64_json: "aGVsbG8=" }] });
    return stream({ content: "Done" });
  });
  try {
    const protocol = await runChat({
      model: chat,
      models: [image],
      key: "key",
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
    expect(JSON.stringify(requests[2]?.body)).not.toContain("blob:");
    expect(JSON.stringify(requests[2]?.body)).not.toContain("aGVsbG8=");
    expect(artifacts.at(-1)?.state).toBe("complete");
  } finally {
    mock.mockRestore();
    urls.forEach((url) => URL.revokeObjectURL(url));
  }
});
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
      key: "",
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
        key: "",
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
test("cancellation after submission uses original owner, tries DELETE after failed cancel", async () => {
  const abort = new AbortController();
  const paths: string[] = [];
  const warnings: string[] = [];
  const id = "00000000-0000-4000-8000-000000000000";
  const mock = mockFetch(async (input, init) => {
    paths.push(`${init?.method ?? "GET"} ${String(input)}`);
    expect(new Headers(init?.headers).get("authorization")).toBe(
      "Bearer owner",
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
        key: "owner",
        signal: abort.signal,
        prompt: "sun",
        progress: () => {},
        warning: (message) => warnings.push(message),
      }),
    ).rejects.toThrow();
    expect(paths).toEqual([
      "POST /v1/videos",
      `POST /v1/videos/${id}/cancel`,
      `DELETE /v1/videos/${id}`,
    ]);
    expect(warnings.join()).toContain("cancellation");
  } finally {
    mock.mockRestore();
  }
});
test("successful AVI remains usable when cleanup fails", async () => {
  const id = "00000000-0000-4000-8000-000000000000";
  const warnings: string[] = [];
  const mock = mockFetch(async (input, init) => {
    if (String(input) === "/v1/videos")
      return Response.json({ id, status: "completed" });
    if (String(input).endsWith("/content"))
      return new Response("avi", {
        headers: { "content-type": "video/x-msvideo" },
      });
    expect(init?.method).toBe("DELETE");
    return new Response(null, { status: 503 });
  });
  try {
    const blob = await generateVideo({
      model: video,
      key: "",
      signal: new AbortController().signal,
      prompt: "sun",
      progress: () => {},
      warning: (message) => warnings.push(message),
    });
    expect(await blob.text()).toBe("avi");
    expect(warnings.join()).toContain("cleanup");
  } finally {
    mock.mockRestore();
  }
});
