// The Go UI's compaction and steering against the TS functions they port.
//
//   bun test go/tools/jstest
import { afterEach, describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/chat-core.js";
import * as ts from "../../../src/ui/compaction";
import * as steer from "../../../src/ui/steering";

class MemoryStorage {
  items = new Map<string, string>();
  getItem(k: string) { return this.items.has(k) ? this.items.get(k)! : null; }
  setItem(k: string, v: string) { this.items.set(k, String(v)); }
  removeItem(k: string) { this.items.delete(k); }
}

const long = (n: number, word = "words ") => word.repeat(n);
const turns = (n: number, size: number) =>
  Array.from({ length: n }, (_, i) => [
    { id: `u${i}`, role: "user" as const, text: `Question ${i}: ${long(size)}` },
    { id: `a${i}`, role: "assistant" as const, text: `Answer ${i}: ${long(size, "ünï ")}`, ...(i === 2 ? { interrupted: true } : {}) },
  ]).flat();

describe("estimates", () => {
  for (const text of ["", "hello", "héllo wörld", "日本語のテキスト", long(100)])
    test(JSON.stringify(text.slice(0, 20)), () => expect(go.estimateTextTokens(text)).toBe(ts.estimateTextTokens(text)));
  test("estimateTokens", () => {
    const messages = [
      { role: "system", content: "Be brief." },
      { role: "user", content: [{ type: "text", text: "look" }, { type: "image_url", image_url: { url: "data:," } }] },
      { role: "assistant", content: "", tool_calls: [{ id: "1", type: "function", function: { name: "generate_image", arguments: '{"prompt":"a cat"}' } }] },
    ];
    expect(go.estimateTokens(messages)).toBe(ts.estimateTokens(messages as any));
  });
  for (const m of ["the request exceeds the available context size (8192 tokens), try increasing it", "available context size (0 tokens)", "nothing", "Available Context Size (4096 TOKENS)"])
    test(`overflowContext ${m.slice(0, 30)}`, () => expect(go.overflowContext(m)).toBe(ts.overflowContext(m)));
  for (const [context, max] of [[8192, undefined], [4096, 4096], [32768, 512], [100, 1024]] as const)
    test(`promptBudget ${context} ${max}`, () => expect(go.promptBudget(context, max)).toBe(ts.promptBudget(context, max)));
});

describe("context window", () => {
  const saved = (globalThis as any).localStorage;
  afterEach(() => ((globalThis as any).localStorage = saved));
  test("learned, catalog and fallback", () => {
    const storage = new MemoryStorage();
    (globalThis as any).localStorage = storage;
    const model = (id: string, context?: number) => ({ go: { id, context }, ts: { id, catalog: { contextWindowTokens: context } } });
    for (const m of [model("a", 131072), model("b", 4096), model("c")])
      expect(go.contextWindow(storage, m.go)).toBe(ts.contextWindow(m.ts as any));
    expect(go.learnContext(storage, "a", 16384)).toBe(true);
    expect(go.learnContext(storage, "a", 16384)).toBe(false);
    expect(ts.learnContext("a", 16384)).toBe(false); // same key and format
    expect(go.contextWindow(storage, model("a", 131072).go)).toBe(ts.contextWindow(model("a", 131072).ts as any));
    storage.setItem(go.contextKey, '{"a": -1, "b": 2.5, "c": 3}');
    for (const id of ["a", "b", "c"]) expect(go.contextWindow(storage, model(id, 9000).go)).toBe(ts.contextWindow(model(id, 9000).ts as any));
    storage.setItem(go.contextKey, "not json");
    expect(go.contextWindow(storage, model("a").go)).toBe(ts.contextWindow(model("a").ts as any));
  });
});

describe("planCompaction", () => {
  const compaction = { through: "a1", summary: "They asked twice.", method: "summary", count: 4 };
  const cases: [string, any][] = [
    ["fits", { messages: turns(3, 10), budget: 10_000 }],
    ["over budget", { messages: turns(6, 200), budget: 2000 }],
    ["over budget, tool overhead", { messages: turns(6, 50), budget: 2000, overhead: 1500 }],
    ["keep one turn", { messages: turns(4, 400), budget: 1500 }],
    ["single turn never compacts", { messages: turns(1, 2000), budget: 100 }],
    ["forced", { messages: turns(3, 5), budget: 100_000, force: true }],
    ["with summary", { messages: turns(6, 200), budget: 2000, compaction }],
    ["summary fits", { messages: turns(3, 10), budget: 10_000, compaction }],
    ["keepTurns 3", { messages: turns(6, 100), budget: 1500, keepTurns: 3 }],
  ];
  for (const [name, options] of cases)
    test(name, () => expect(go.planCompaction(options)).toEqual(ts.planCompaction(options)));
});

describe("transcript", () => {
  const messages = [
    { id: "1", role: "user", text: "See these", attachments: [{ kind: "image", name: "cat.png", url: "data:," }] },
    { id: "2", role: "assistant", text: "", artifacts: [{ id: "x", label: "Image", state: "complete" }, { id: "y", label: "Audio", state: "error" }] },
    { id: "3", role: "user", text: "", attachmentsMissing: true },
    { id: "4", role: "assistant", text: long(3000), interrupted: true, reasoning: "hidden" },
  ];
  for (const characters of [100, 2000, 50_000])
    test(`${characters}`, () => expect(go.transcript(messages, characters)).toBe(ts.transcript(messages as any, characters)));
});

describe("compact", () => {
  const original = globalThis.fetch;
  afterEach(() => (globalThis.fetch = original));
  const sse = (content: string) =>
    new Response(`data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\n\ndata: ${JSON.stringify({ choices: [{ delta: {}, finish_reason: "stop" }] })}\n\ndata: [DONE]\n\n`, { headers: { "content-type": "text/event-stream" } });
  const messages = turns(5, 100);
  const plan = ts.planCompaction({ messages: messages as any, budget: 1000 })!;
  const previous = { through: "a0", summary: "Earlier bit.", method: "summary" as const, count: 2 };

  async function both(reply: () => Response, compaction?: any) {
    const bodies: any[] = [];
    globalThis.fetch = (async (_url: any, init: any) => (bodies.push(JSON.parse(init.body)), reply())) as any;
    const signal = new AbortController().signal;
    const tsResult = await ts.compact({ connection: { kind: "session" }, model: { id: "m" } as any, messages: messages as any, compaction, ...plan, context: 8192, signal });
    const goResult = await go.compact({
      model: { id: "m" }, messages, compaction, ...plan, context: 8192, signal,
      post: async (body: any) => (bodies.push(body), reply()),
    });
    expect(goResult).toEqual(tsResult);
    expect(bodies[1]).toEqual(bodies[0]);
    return goResult;
  }
  test("summary", async () => expect((await both(() => sse("<think>x</think>- point"))).compaction.method).toBe("summary"));
  test("with previous summary", async () => expect((await both(() => sse("- more"), previous)).compaction.count).toBe(previous.count + plan.covered.length));
  test("empty summary drops", async () => expect((await both(() => sse("<think>only</think>"), previous)).compaction.method).toBe("dropped"));
  test("cancelled throws", async () => {
    const abort = new AbortController();
    abort.abort();
    const post = async () => { throw new Error("aborted"); };
    await expect(go.compact({ model: { id: "m" }, messages, ...plan, context: 8192, signal: abort.signal, post })).rejects.toThrow();
  });
});

describe("steering", () => {
  for (const [phase, s] of [["waiting", 0], ["thinking", 1.7], ["compacting", 12]] as const)
    test(`${phase} ${s}`, () => expect(go.phaseLabel(phase, s)).toBe(steer.phaseLabel(phase, s)));
  test("interruptReply", () => {
    const m = { id: "r", role: "assistant", text: "part", protocol: [{ role: "assistant", content: "x" }], artifacts: [{ id: "1", label: "Image", state: "working", detail: "Drawing" }, { id: "2", label: "Audio", state: "complete" }] };
    expect(go.interruptReply(m, "Interrupted")).toEqual(steer.interruptReply(m as any, "Interrupted"));
    expect(go.interruptReply({ id: "r", role: "assistant", text: "" }, "Stopped")).toEqual(steer.interruptReply({ id: "r", role: "assistant", text: "" }, "Stopped"));
  });
});
