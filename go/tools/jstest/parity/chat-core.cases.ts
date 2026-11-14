// The inputs of chat-core.test.ts, shared by the test (the Go side) and the fixture generator
// (the frozen TS side). See frozen.ts.

export class MemoryStorage {
  items = new Map<string, string>();
  getItem(k: string) { return this.items.has(k) ? this.items.get(k)! : null; }
  setItem(k: string, v: string) { this.items.set(k, String(v)); }
  removeItem(k: string) { this.items.delete(k); }
}

export const texts = [
  "<think>plan the answer</think>The answer is 4.",
  "  <think>a</think>\n\nb",
  "No thinking here, but <think> mentioned.",
  "<think>unterminated reasoning",
  "<thi",
  "<think></think>",
  "Answer first <think>not reasoning</think>",
  "\n\n<think>x</think>y<think>z</think>",
];

type Splitter = { push(c: string): { content: string; reasoning: string }; flush(): { content: string; reasoning: string } };

function split(impl: Splitter, chunks: string[]) {
  let content = "";
  let reasoning = "";
  for (const c of chunks) {
    const p = impl.push(c);
    content += p.content;
    reasoning += p.reasoning;
  }
  const f = impl.flush();
  return { content: content + f.content, reasoning: reasoning + f.reasoning };
}

// splits is the text split under every chunk size, 1 to its length.
export function splits(make: () => Splitter, text: string) {
  const out = [];
  for (let size = 1; size <= text.length; size++) out.push(split(make(), text.match(new RegExp(`[\\s\\S]{1,${size}}`, "g")) ?? [""]));
  return out;
}

export const deltas = [{ reasoning: "a" }, { reasoning_content: "b" }, { reasoning: "", reasoning_content: "c" }, {}];

export const conversation = [
  { id: "1", role: "user" as const, text: "Hello" },
  { id: "2", role: "assistant" as const, text: "Hi! How can I help?" },
  { id: "3", role: "user" as const, text: "First" },
  { id: "4", role: "user" as const, text: "Second, merged" },
  { id: "5", role: "assistant" as const, text: "Partial rep", interrupted: true },
  { id: "6", role: "assistant" as const, text: "   ", interrupted: true },
  { id: "7", role: "user" as const, text: "Again" },
  { id: "8", role: "assistant" as const, text: "", reasoning: "only thoughts" },
];

export const contextCases: [string, any][] = [
  ["plain", undefined],
  ["summary", { through: "2", summary: "They greeted.", method: "summary", count: 2 }],
  ["dropped", { through: "4", summary: "", method: "dropped", count: 4 }],
  ["unknown through", { through: "nope", summary: "x", method: "summary", count: 1 }],
];

export const conversations = [
  {
    id: "c1", title: "A conversation", workspace: "chat", mode: "llm", model: "m",
    messages: [
      { id: "1", role: "user", text: "hi", attachments: [{ kind: "image", name: "a.png", url: "data:x" }] },
      { id: "2", role: "assistant", text: "hello", reasoning: "r".repeat(25000), interrupted: true, protocol: [{ role: "assistant", content: "hello" }] },
      { id: "3", role: "assistant", text: "", media: { kind: "image", url: "blob:x" } },
    ],
    compaction: { through: "1", summary: "s", method: "summary", count: 1 },
  },
  { id: "c2", title: "Lab", workspace: "lab", mode: "image", model: "img", messages: [] },
];

export const invalid = [
  "{}",
  JSON.stringify([{ id: "x" }]),
  JSON.stringify(Array.from({ length: 31 }, (_, i) => ({ id: String(i), title: "t", workspace: "chat", mode: "llm", model: "m", messages: [] }))),
  JSON.stringify([{ id: "x", title: "t".repeat(121), workspace: "chat", mode: "llm", model: "m", messages: [] }]),
  JSON.stringify([{ id: "x", title: "t", workspace: "chat", mode: "chat", model: "m", messages: [] }]),
  JSON.stringify([{ id: "x", title: "t", workspace: "chat", mode: "llm", model: "m", messages: [{ id: "1", role: "system", text: "" }] }]),
  JSON.stringify([{ id: "x", title: "t", workspace: "chat", mode: "llm", model: "m", messages: [], compaction: { through: "1", summary: "", method: "x", count: 0 } }]),
  JSON.stringify([{ id: "x", title: "t", workspace: "chat", mode: "llm", model: "m", messages: [{ id: "1", role: "user", text: "", interrupted: "yes" }] }]),
];

export const extraKeys = JSON.stringify([{ id: "x", title: "t", workspace: "chat", mode: "llm", model: "m", extra: 1,
  messages: [{ id: "1", role: "user", text: "a", more: true }] }]);

const stream = (events: string[]) => new Response(new ReadableStream({
  start(c) {
    for (const e of events) c.enqueue(new TextEncoder().encode(e));
    c.close();
  },
}));

// runStream is what a streamText implementation hands back, and what it passed on, for the events.
export async function runStream(impl: any, events: string[]) {
  let text = "";
  let thoughts = "";
  try {
    const calls = await impl(stream(events), (t: string) => (text += t), (t: string) => (thoughts += t));
    return { text, thoughts, calls };
  } catch (e) {
    return { text, thoughts, error: (e as Error).message };
  }
}

const chunk = (delta: object, finish: string | null = null) => `data: ${JSON.stringify({ choices: [{ index: 0, delta, finish_reason: finish }] })}\n\n`;
export const streams: Record<string, string[]> = {
  plain: [chunk({ content: "Hel" }), chunk({ content: "lo" }), chunk({}, "stop"), "data: [DONE]\n\n"],
  crlf: [`data: ${JSON.stringify({ choices: [{ delta: { content: "a" } }] })}\r\n\r\n`, "data: [DONE]\r\n\r\n"],
  split: ['data: {"choices":[{"delta":{"con', 'tent":"x"}}]}\n', "\ndata: [DONE]\n\n"],
  reasoning: [chunk({ reasoning: "think" }), chunk({ content: "<think>more</think>Answer" }), "data: [DONE]\n\n"],
  error: [chunk({ content: "a" }), `data: ${JSON.stringify({ error: { message: "Backend failed" } })}\n\n`],
  truncated: [chunk({ content: "a" })],
  tools: [chunk({ tool_calls: [{ index: 0, id: "abc", function: { name: "generate_image", arguments: '{"pro' } }] }),
    chunk({ tool_calls: [{ index: 0, function: { arguments: 'mpt":"x"}' } }] }), "data: [DONE]\n\n"],
  badTool: [chunk({ tool_calls: [{ index: 0, function: { name: "x" } }] }), "data: [DONE]\n\n"],
};
