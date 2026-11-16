// The inputs of compaction.test.ts, shared by the test (the Go side) and the fixture generator
// (the frozen TS side). See frozen.ts.

export class MemoryStorage {
  items = new Map<string, string>();
  getItem(k: string) { return this.items.has(k) ? this.items.get(k)! : null; }
  setItem(k: string, v: string) { this.items.set(k, String(v)); }
  removeItem(k: string) { this.items.delete(k); }
}

export const long = (n: number, word = "words ") => word.repeat(n);
export const turns = (n: number, size: number) =>
  Array.from({ length: n }, (_, i) => [
    { id: `u${i}`, role: "user" as const, text: `Question ${i}: ${long(size)}` },
    { id: `a${i}`, role: "assistant" as const, text: `Answer ${i}: ${long(size, "ünï ")}`, ...(i === 2 ? { interrupted: true } : {}) },
  ]).flat();

export const textTokens = ["", "hello", "héllo wörld", "日本語のテキスト", long(100)];
export const tokenMessages = [
  { role: "system", content: "Be brief." },
  { role: "user", content: [{ type: "text", text: "look" }, { type: "image_url", image_url: { url: "data:," } }] },
  { role: "assistant", content: "", tool_calls: [{ id: "1", type: "function", function: { name: "generate_image", arguments: '{"prompt":"a cat"}' } }] },
];
export const overflows = ["the request exceeds the available context size (8192 tokens), try increasing it", "available context size (0 tokens)", "nothing", "Available Context Size (4096 TOKENS)"];
export const budgets = [[8192, undefined], [4096, 4096], [32768, 512], [100, 1024]] as const;

// The context-window scenario: each model's window before anything is learned, after one is, and
// with damaged storage. The go and ts shapes of each model.
export const windowModel = (id: string, context?: number) => ({ go: { id, context }, ts: { id, catalog: { contextWindowTokens: context } } });
export const windowModels = () => [windowModel("a", 131072), windowModel("b", 4096), windowModel("c")];
export const damaged = ['{"a": -1, "b": 2.5, "c": 3}', "not json"];

const compaction = { through: "a1", summary: "They asked twice.", method: "summary", count: 4 };
export const plans: [string, () => any][] = [
  ["fits", () => ({ messages: turns(3, 10), budget: 10_000 })],
  ["over budget", () => ({ messages: turns(6, 200), budget: 2000 })],
  ["over budget, tool overhead", () => ({ messages: turns(6, 50), budget: 2000, overhead: 1500 })],
  ["keep one turn", () => ({ messages: turns(4, 400), budget: 1500 })],
  ["single turn never compacts", () => ({ messages: turns(1, 2000), budget: 100 })],
  ["forced", () => ({ messages: turns(3, 5), budget: 100_000, force: true })],
  ["with summary", () => ({ messages: turns(6, 200), budget: 2000, compaction })],
  ["summary fits", () => ({ messages: turns(3, 10), budget: 10_000, compaction })],
  ["keepTurns 3", () => ({ messages: turns(6, 100), budget: 1500, keepTurns: 3 })],
];

export const transcriptMessages = [
  { id: "1", role: "user", text: "See these", attachments: [{ kind: "image", name: "cat.png", url: "data:," }] },
  { id: "2", role: "assistant", text: "", artifacts: [{ id: "x", label: "Image", state: "complete" }, { id: "y", label: "Audio", state: "error" }] },
  { id: "3", role: "user", text: "", attachmentsMissing: true },
  { id: "4", role: "assistant", text: long(3000), interrupted: true, reasoning: "hidden" },
];
export const transcriptSizes = [100, 2000, 50_000];

// compact: a scripted model reply, over the same five turns, with and without an earlier summary.
export const sse = (content: string) =>
  new Response(`data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\n\ndata: ${JSON.stringify({ choices: [{ delta: {}, finish_reason: "stop" }] })}\n\ndata: [DONE]\n\n`, { headers: { "content-type": "text/event-stream" } });
export const compactMessages = () => turns(5, 100);
export const compactBudget = 1000;
export const previous = { through: "a0", summary: "Earlier bit.", method: "summary" as const, count: 2 };
export const compactCases: [string, string, boolean][] = [
  ["summary", "<think>x</think>- point", false],
  ["with previous summary", "- more", true],
  ["empty summary drops", "<think>only</think>", true],
];

export const phases = [["waiting", 0], ["thinking", 1.7], ["compacting", 12]] as const;
export const interrupts: [any, string][] = [
  [{ id: "r", role: "assistant", text: "part", protocol: [{ role: "assistant", content: "x" }], artifacts: [{ id: "1", label: "Image", state: "working", detail: "Drawing" }, { id: "2", label: "Audio", state: "complete" }] }, "Interrupted"],
  [{ id: "r", role: "assistant", text: "" }, "Stopped"],
];
