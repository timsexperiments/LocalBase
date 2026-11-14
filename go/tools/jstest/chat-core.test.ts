// The Go UI's chat core against the TS UI functions it ports, frozen as fixtures
// (parity/frozen.ts; testdata/chat-core.json).
//
//   bun test go/tools/jstest
//
// Same inputs, same outputs: inline reasoning splitting under every
// chunking, the request context, and device history in both directions.
import { describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/chat-core.js";
import { MemoryStorage, contextCases, conversation, conversations, deltas, extraKeys, invalid, runStream, splits, streams, texts } from "./parity/chat-core.cases.ts";
import { fixture, snap } from "./parity/frozen.ts";

const want = fixture("chat-core");

describe("thinkSplitter", () => {
  for (const text of texts) {
    test(JSON.stringify(text), () => {
      expect(want.thinkSplitter[text]).toHaveLength(text.length);
      expect(splits(go.thinkSplitter, text)).toEqual(want.thinkSplitter[text]);
    });
  }
  test("deltaReasoning", () => {
    expect(snap(deltas.map((d) => go.deltaReasoning(d)))).toEqual(want.deltaReasoning);
  });
});

describe("chatContext", () => {
  for (const [name, compaction] of contextCases)
    test(name, () => expect(snap(go.chatContext(conversation, compaction))).toEqual(want.chatContext[name]));
  test("missing attachments", () => {
    const m = [{ id: "1", role: "user" as const, text: "x", attachmentsMissing: true }];
    expect(() => go.chatContext(m)).toThrow("missing its attachments");
  });
});

describe("history", () => {
  test("the Go page writes what TS writes", () => {
    expect(go.serializeHistory(conversations)).toBe(want.history.written);
  });
  test("each reads the other's history", () => {
    const s = new MemoryStorage();
    s.setItem(go.historyKey, go.serializeHistory(conversations));
    expect(snap(go.readHistory(s))).toEqual(want.history.read);
    expect(go.historyKey).toBe(want.history.key);
  });
  test("saving off is no key, for both", () => {
    expect(want.history.savingOff).toBeNull();
    expect(go.readHistory(new MemoryStorage())).toBeNull();
  });
  invalid.forEach((raw, i) =>
    test(`rejects like TS: ${raw.slice(0, 60)}`, () => {
      expect(want.history.rejected[i]).toBe(true);
      const s = new MemoryStorage();
      s.setItem(go.historyKey, raw);
      expect(() => go.readHistory(s)).toThrow();
    }));
  test("extra keys are dropped, as zod strips them", () => {
    const s = new MemoryStorage();
    s.setItem(go.historyKey, extraKeys);
    expect(snap(go.readHistory(s))).toEqual(want.history.extraKeys);
  });
});

describe("streamText", () => {
  for (const [name, events] of Object.entries(streams))
    test(name, async () => expect(snap(await runStream(go.streamText, events))).toEqual(want.streamText[name]));
});

test("stripThinking is not needed client-side but titles match", () => {
  expect(go.titleFrom("x".repeat(80))).toBe("x".repeat(60));
  expect(go.titleFrom("")).toBe("Attachments");
  expect(go.titleFrom("Plan a three day trip with kids. Keep it cheap.")).toBe("Plan a three day trip");
  expect(go.titleFrom("Why not?")).toBe("Why not");
  expect(go.titleFrom("???")).toBe("???");
});
