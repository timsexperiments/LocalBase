// The Go UI's compaction and steering against the TS functions they port, frozen as fixtures
// (parity/frozen.ts; testdata/compaction.json).
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/chat-core.js";
import * as c from "./parity/compaction.cases.ts";
import { fixture, snap } from "./parity/frozen.ts";

const want = fixture("compaction");

describe("estimates", () => {
  c.textTokens.forEach((text, i) =>
    test(JSON.stringify(text.slice(0, 20)), () => expect(go.estimateTextTokens(text)).toBe(want.estimateTextTokens[i])));
  test("estimateTokens", () => {
    expect(go.estimateTokens(c.tokenMessages)).toBe(want.estimateTokens);
  });
  c.overflows.forEach((m, i) =>
    test(`overflowContext ${m.slice(0, 30)}`, () => expect(snap(go.overflowContext(m))).toEqual(want.overflowContext[i])));
  c.budgets.forEach(([context, max], i) =>
    test(`promptBudget ${context} ${max}`, () => expect(go.promptBudget(context, max)).toBe(want.promptBudget[i])));
});

describe("context window", () => {
  test("learned, catalog and fallback", () => {
    const w = want.contextWindow;
    const storage = new c.MemoryStorage();
    expect(c.windowModels().map((m) => go.contextWindow(storage, m.go))).toEqual(w.fresh);
    expect(go.learnContext(storage, "a", 16384)).toBe(w.learnFirst);
    expect(go.learnContext(storage, "a", 16384)).toBe(w.learnAgain);
    // The Go page stores what TS stored, under the same key, so each reads the other's.
    expect([...storage.items]).toEqual(w.learnedStorage);
    expect(go.contextWindow(storage, c.windowModels()[0].go)).toBe(w.afterLearn);
    storage.setItem(go.contextKey, c.damaged[0]);
    expect(["a", "b", "c"].map((id) => go.contextWindow(storage, c.windowModel(id, 9000).go))).toEqual(w.damaged);
    storage.setItem(go.contextKey, c.damaged[1]);
    expect(go.contextWindow(storage, c.windowModel("a").go)).toBe(w.notJSON);
  });
});

describe("planCompaction", () => {
  for (const [name, options] of c.plans)
    test(name, () => expect(snap(go.planCompaction(options()))).toEqual(want.planCompaction[name]));
});

describe("transcript", () => {
  c.transcriptSizes.forEach((characters, i) =>
    test(`${characters}`, () => expect(go.transcript(c.transcriptMessages, characters)).toBe(want.transcript[i])));
});

describe("compact", () => {
  const messages = c.compactMessages();
  const plan = want.compact.plan;

  async function goSide(name: string, content: string, compaction?: any) {
    const bodies: any[] = [];
    const signal = new AbortController().signal;
    const result = await go.compact({
      model: { id: "m" }, messages, compaction, ...plan, context: 8192, signal,
      post: async (body: any) => (bodies.push(body), c.sse(content)),
    });
    expect(snap(result)).toEqual(want.compact.cases[name].result);
    expect(snap(bodies[0])).toEqual(want.compact.cases[name].body);
    return result;
  }
  const [summary, withPrevious, empty] = c.compactCases;
  test(summary[0], async () => expect((await goSide(summary[0], summary[1])).compaction.method).toBe("summary"));
  test(withPrevious[0], async () => expect((await goSide(withPrevious[0], withPrevious[1], c.previous)).compaction.count).toBe(c.previous.count + plan.covered.length));
  test(empty[0], async () => expect((await goSide(empty[0], empty[1], c.previous)).compaction.method).toBe("dropped"));
  test("cancelled throws", async () => {
    const abort = new AbortController();
    abort.abort();
    const post = async () => { throw new Error("aborted"); };
    await expect(go.compact({ model: { id: "m" }, messages, ...plan, context: 8192, signal: abort.signal, post })).rejects.toThrow();
  });
});

describe("steering", () => {
  c.phases.forEach(([phase, s], i) =>
    test(`${phase} ${s}`, () => expect(go.phaseLabel(phase, s)).toBe(want.phaseLabel[i])));
  test("interruptReply", () => {
    expect(snap(c.interrupts.map(([m, label]) => go.interruptReply(m, label)))).toEqual(want.interruptReply);
  });
});
