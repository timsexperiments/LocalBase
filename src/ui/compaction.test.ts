import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import {
  readHistory,
  writeHistory,
  type Compaction,
  type Conversation,
  type Message,
  type Model,
} from "./client";
import {
  assumedRuntimeContext,
  chatContext,
  compact,
  contextWindow,
  estimateTextTokens,
  estimateTokens,
  interruptedNote,
  learnContext,
  overflowContext,
  planCompaction,
  promptBudget,
  transcript,
  visibleMessages,
} from "./compaction";

let store: Map<string, string>;
let descriptor: PropertyDescriptor | undefined;
beforeEach(() => {
  store = new Map();
  descriptor = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    value: {
      getItem: (key: string) => store.get(key) ?? null,
      setItem: (key: string, value: string) => store.set(key, value),
    },
  });
});
afterEach(() => {
  if (descriptor) Object.defineProperty(globalThis, "localStorage", descriptor);
  else Reflect.deleteProperty(globalThis, "localStorage");
});

const model = (id: string, contextWindowTokens?: number) =>
  ({ id, catalog: { contextWindowTokens } }) as unknown as Model;
const user = (id: string, text: string): Message => ({
  id,
  role: "user",
  text,
});
const reply = (id: string, text: string, extra: Partial<Message> = {}) =>
  ({ id, role: "assistant", text, ...extra }) as Message;
function turns(count: number, size: number) {
  return Array.from({ length: count }, (_, index) => [
    user(`u${index}`, `question ${index} ${"x".repeat(size)}`),
    reply(`a${index}`, `answer ${index} ${"y".repeat(size)}`),
  ]).flat();
}
function sse(events: unknown[]) {
  return new Response(
    events
      .map((event) => `data: ${JSON.stringify(event)}\n\n`)
      .join("")
      .concat("data: [DONE]\n\n"),
  );
}

describe("token estimates", () => {
  test("estimate text and messages", () => {
    expect(estimateTextTokens("")).toBe(0);
    expect(estimateTextTokens("a".repeat(35))).toBe(10);
    expect(estimateTextTokens("日本")).toBe(2);
    expect(
      estimateTokens([
        { role: "user", content: "a".repeat(35) },
        {
          role: "user",
          content: [
            { type: "text", text: "a".repeat(7) },
            { type: "image_url", image_url: { url: "data:," } },
          ],
        },
      ]),
    ).toBe(8 + 10 + 8 + 2 + 1024);
  });

  test("prompt budget leaves room for the reply", () => {
    expect(promptBudget(32_768)).toBe(24_576);
    expect(promptBudget(4096, 3000)).toBe(2048);
    expect(promptBudget(2000, 1800)).toBe(1000);
  });
});

describe("context window", () => {
  test("parses llama.cpp overflow errors", () => {
    expect(
      overflowContext(
        "the request exceeds the available context size (8192 tokens), try increasing it",
      ),
    ).toBe(8192);
    expect(overflowContext("Request failed (500).")).toBeNull();
  });

  test("assumes a conservative size until it learns the runtime one", () => {
    expect(contextWindow(model("big", 262_144))).toBe(assumedRuntimeContext);
    expect(contextWindow(model("small", 4096))).toBe(4096);
    expect(contextWindow(model("unknown"))).toBe(8192);
    expect(learnContext("big", 16_384)).toBe(true);
    expect(learnContext("big", 16_384)).toBe(false);
    expect(contextWindow(model("big", 262_144))).toBe(16_384);
  });
});

describe("chat context", () => {
  test("never sends reasoning and marks interrupted replies", () => {
    const context = chatContext([
      user("u1", "hi"),
      reply("a1", "Partial answer", { interrupted: true, reasoning: "secret" }),
      user("u2", "actually, do this"),
    ]);
    expect(context).toEqual([
      { role: "user", content: "hi" },
      { role: "assistant", content: `Partial answer\n\n${interruptedNote}` },
      { role: "user", content: "actually, do this" },
    ]);
    expect(JSON.stringify(context)).not.toContain("secret");
  });

  test("merges consecutive user messages when an interrupted reply is empty", () => {
    expect(
      chatContext([
        user("u1", "first"),
        reply("a1", "", { interrupted: true, reasoning: "half a thought" }),
        user("u2", "second"),
      ]),
    ).toEqual([{ role: "user", content: "first\n\nsecond" }]);
  });

  test("replaces the compacted prefix with a summary pair", () => {
    const messages = turns(3, 1);
    const compaction: Compaction = {
      through: "a0",
      summary: "- asked question 0",
      method: "summary",
      count: 2,
    };
    expect(visibleMessages(messages, compaction).map(({ id }) => id)).toEqual([
      "u1",
      "a1",
      "u2",
      "a2",
    ]);
    const context = chatContext(messages, compaction);
    expect(context).toHaveLength(6);
    expect(context[0]?.role).toBe("user");
    expect(context[0]?.content).toContain("- asked question 0");
    expect(context[1]?.role).toBe("assistant");
    expect(context[2]?.content).toStartWith("question 1");
    // Dropped compactions have no summary, so nothing replaces the prefix.
    expect(
      chatContext(messages, { ...compaction, summary: "", method: "dropped" }),
    ).toHaveLength(4);
  });
});

describe("planCompaction", () => {
  test("does nothing while the prompt fits", () => {
    expect(
      planCompaction({ messages: turns(3, 10), budget: 10_000 }),
    ).toBeNull();
  });

  test("keeps the newest turns and covers the rest", () => {
    const messages = turns(6, 400);
    const plan = planCompaction({ messages, budget: 700 });
    expect(plan?.through.id).toBe("a3");
    expect(plan?.covered.map(({ id }) => id)).toEqual([
      "u0",
      "a0",
      "u1",
      "a1",
      "u2",
      "a2",
      "u3",
      "a3",
    ]);
  });

  test("falls back to keeping only the last turn", () => {
    const plan = planCompaction({ messages: turns(4, 4000), budget: 1000 });
    expect(plan?.through.id).toBe("a2");
  });

  test("force compacts even when the prompt fits, after any earlier summary", () => {
    const messages = turns(4, 10);
    const plan = planCompaction({
      messages,
      budget: 100_000,
      force: true,
      compaction: { through: "a0", summary: "s", method: "summary", count: 2 },
    });
    expect(plan?.covered.map(({ id }) => id)).toEqual(["u1", "a1"]);
    expect(
      planCompaction({ messages: turns(1, 10), budget: 1, force: true }),
    ).toBeNull();
  });
});

describe("compact", () => {
  const connection = { kind: "session" as const };

  test("summarizes covered messages without thinking or reasoning", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      sse([
        {
          choices: [{ index: 0, delta: { content: "<think>x</think>- fact" } }],
        },
      ]),
    );
    try {
      const messages = [
        user("u0", "my name is Robin"),
        reply("a0", "Hello Robin", { reasoning: "private thought" }),
        user("u1", "next"),
      ];
      const result = await compact({
        connection,
        model: model("chat"),
        messages,
        compaction: {
          through: "gone",
          summary: "stale",
          method: "summary",
          count: 9,
        },
        covered: messages.slice(0, 2),
        through: messages[1]!,
        context: 8192,
        signal: new AbortController().signal,
      });
      expect(result).toEqual({
        compaction: {
          through: "a0",
          summary: "- fact",
          method: "summary",
          count: 2,
        },
      });
      const [url, init] = fetchMock.mock.calls[0]!;
      expect(String(url)).toBe("/app/api/v1/chat/completions");
      const body = JSON.parse(String(init?.body));
      expect(body.model).toBe("chat");
      expect(body.chat_template_kwargs).toEqual({ enable_thinking: false });
      expect(body.messages[1].content).toContain("User: my name is Robin");
      expect(body.messages[1].content).not.toContain("private thought");
      expect(body.messages[1].content).not.toContain("stale");
    } finally {
      fetchMock.mockRestore();
    }
  });

  test("drops covered messages when summarization fails", async () => {
    const fetchMock = spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({ error: { message: "model crashed" } }, { status: 500 }),
    );
    try {
      const messages = turns(3, 1);
      const result = await compact({
        connection,
        model: model("chat"),
        messages,
        compaction: {
          through: "a0",
          summary: "old",
          method: "summary",
          count: 2,
        },
        covered: messages.slice(2, 4),
        through: messages[3]!,
        context: 8192,
        signal: new AbortController().signal,
      });
      expect(result).toEqual({
        compaction: {
          through: "a1",
          summary: "old",
          method: "dropped",
          count: 4,
        },
        error: "model crashed",
      });
    } finally {
      fetchMock.mockRestore();
    }
  });

  test("rethrows when cancelled", async () => {
    const abort = new AbortController();
    abort.abort();
    const fetchMock = spyOn(globalThis, "fetch").mockRejectedValue(
      new DOMException("aborted", "AbortError"),
    );
    try {
      const messages = turns(2, 1);
      await expect(
        compact({
          connection,
          model: model("chat"),
          messages,
          covered: messages.slice(0, 2),
          through: messages[1]!,
          context: 8192,
          signal: abort.signal,
        }),
      ).rejects.toThrow();
    } finally {
      fetchMock.mockRestore();
    }
  });
});

test("transcript notes attachments and interruptions but omits reasoning", () => {
  const text = transcript(
    [
      { ...user("u0", "look"), attachmentsMissing: true },
      reply("a0", "Partial", { interrupted: true, reasoning: "hidden" }),
    ],
    10_000,
  );
  expect(text).toBe(
    `User: look\n[attachments not available]\n\nAssistant: Partial\n${interruptedNote}`,
  );
});

test("history keeps compaction, interruptions and reasoning", () => {
  const conversation: Conversation = {
    id: "c",
    title: "Chat",
    workspace: "chat",
    mode: "llm",
    model: "chat",
    messages: [
      user("u0", "hi"),
      reply("a0", "Part", { interrupted: true, reasoning: "r".repeat(25_000) }),
    ],
    compaction: { through: "a0", summary: "- hi", method: "summary", count: 2 },
  };
  writeHistory([conversation]);
  const [restored] = readHistory() ?? [];
  expect(restored?.compaction).toEqual(conversation.compaction);
  expect(restored?.messages[1]?.interrupted).toBe(true);
  expect(restored?.messages[1]?.reasoning?.length).toBe(20_001);
});
