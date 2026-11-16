// Search's pure parts (DESIGN §35): splitting text into passages, ranking by cosine, and reading
// passages in batches with the retries §35.2 and §35.3 ask for. No page, no network.
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import { LIMITS, batches, capPassages, closest, cosine, marks, rank, readFrom, splitText, type EmbedError } from "../../internal/web/static/js/search-core.js";

const words = (s: string) => s.split(/\s+/).filter(Boolean);

describe("splitText", () => {
  const prose = Array.from({ length: 40 }, (_, i) => `Sentence ${i} talks about the harbor and the boats, and the gulls above them.`).join(" ");
  const text = `${prose}\n\nA short second paragraph.\n\n\n   \nThird one, after blank lines with spaces.`;

  test("blank lines split first, and each passage is a span of the source", () => {
    const p = splitText(text);
    expect(p.length).toBeGreaterThan(3);
    for (const x of p) expect(text.slice(x.start, x.end)).toBe(x.text);
    expect(p.at(-2)!.text).toBe("A short second paragraph.");
    expect(p.at(-1)!.text).toBe("Third one, after blank lines with spaces.");
  });

  test("no passage is longer than the limit, and none starts or ends inside a word", () => {
    for (const x of splitText(text)) {
      expect(x.text.length).toBeLessThanOrEqual(LIMITS.passageChars);
      expect(x.text).toBe(x.text.trim());
      if (x.start > 0) expect(/\s/.test(text[x.start - 1])).toBe(true);
      if (x.end < text.length) expect(/\s/.test(text[x.end])).toBe(true);
    }
  });

  test("every word is in exactly one passage, in order", () => {
    expect(splitText(text).flatMap((x) => words(x.text))).toEqual(words(text));
  });

  test("a long paragraph breaks at sentence ends before spaces", () => {
    const p = splitText(prose, { maxChars: 200 });
    expect(p.length).toBeGreaterThan(1);
    for (const x of p) expect(/[.!?]$/.test(x.text)).toBe(true);
  });

  test("a sentence longer than the limit breaks at spaces, and a word longer than it stays whole", () => {
    const long = Array.from({ length: 60 }, (_, i) => `word${i}`).join(" ");
    const p = splitText(long, { maxChars: 50 });
    for (const x of p) expect(x.text.length).toBeLessThanOrEqual(50);
    expect(p.flatMap((x) => words(x.text))).toEqual(words(long));
    const huge = "x".repeat(80);
    expect(splitText(`before ${huge} after`, { maxChars: 50 }).map((x) => x.text)).toEqual(["before", huge, "after"]);
  });

  test("a passage remembers its source", () => {
    expect(splitText("One.\n\nTwo.", { source: "notes.txt" }).map((x) => x.source)).toEqual(["notes.txt", "notes.txt"]);
  });

  test("text with nothing in it has no passages", () => {
    expect(splitText("")).toEqual([]);
    expect(splitText(" \n\n\t \n")).toEqual([]);
  });
});

describe("capPassages", () => {
  test("keeps the first 2,000 and says how many it dropped", () => {
    const many = Array.from({ length: 2003 }, (_, i) => ({ text: `p${i}`, start: 0, end: 0, source: "" }));
    const { passages, dropped } = capPassages(many);
    expect(passages.length).toBe(2000);
    expect(passages[1999].text).toBe("p1999");
    expect(dropped).toBe(3);
    expect(capPassages(many.slice(0, 5)).dropped).toBe(0);
  });
});

describe("cosine and rank", () => {
  test("cosine works on vectors that aren't normalized", () => {
    expect(cosine([3, 4], [6, 8])).toBeCloseTo(1, 12);
    expect(cosine([1, 0], [0, 2])).toBe(0);
    expect(cosine([1, 2, 3], [4, 5, 6])).toBeCloseTo(32 / (Math.sqrt(14) * Math.sqrt(77)), 12);
    expect(cosine([0, 0], [1, 1])).toBe(0);
  });

  test("rank orders by cosine, not by size: a hand-worked example a dot product gets wrong", () => {
    // Query [1, 0]. By cosine: #3 (1), #1 (2/sqrt(4.01) ≈ 0.99875), #2 (0.7071), #0 (0).
    // By dot product #1 (2) would come first, ahead of #3 (0.5).
    const r = rank([1, 0], [[0, 1], [2, 0.1], [1, 1], [0.5, 0]], 4);
    expect(r.map((x) => x.index)).toEqual([3, 1, 2, 0]);
    expect(r[1].score).toBeCloseTo(2 / Math.sqrt(4.01), 12);
  });

  test("rank keeps the closest k, and ties keep the source's order", () => {
    expect(rank([1], [[1], [2], [3]], 2).map((x) => x.index)).toEqual([0, 1]);
  });

  test("closest gives each passage once, by its best part, and keeps the closest k", () => {
    // Passage 0 was read in two parts (it was halved); its second part is the best match of all.
    const entries = [
      { passage: 0, vector: [0, 1] },
      { passage: 0, vector: [1, 0.1] },
      { passage: 1, vector: [1, 1] },
      { passage: 2, vector: [1, 0.5] },
      { passage: 3, vector: [-1, 0] },
    ];
    const r = closest([1, 0], entries, 3);
    expect(r.map((x) => x.passage)).toEqual([0, 2, 1]);
    expect(r[0].score).toBeCloseTo(1 / Math.sqrt(1.01), 12);
    expect(closest([1, 0], entries).map((x) => x.passage)).toEqual([0, 2, 1, 3]);
    expect(closest([1, 0], [])).toEqual([]);
  });

  test("marks are relative to the best of this search, named Closer or Less close, and never a percentage", () => {
    const m = marks([{ index: 0, score: 0.9 }, { index: 1, score: 0.8 }, { index: 2, score: 0.5 }]);
    expect(m.map((x) => x.rel)).toEqual([1, 0.75, 0]);
    expect(m.map((x) => x.word)).toEqual(["Closer", "Closer", "Less close"]);
    expect(marks([{ index: 0, score: 0.4 }]).map((x) => x.word)).toEqual(["Closer"]);
  });
});

describe("batches", () => {
  test("2,000 passages go as 63 requests of at most 32", () => {
    const b = batches(Array.from({ length: 2000 }, (_, i) => i));
    expect(b.length).toBe(63);
    expect(b.slice(0, 62).every((x) => x.length === 32)).toBe(true);
    expect(b[62].length).toBe(16);
  });
});

// A fake /v1/embeddings: one vector per input, made from its length; "fake:batch" anywhere in a
// request overflows the whole request, as the fake backend does.
function fakeEmbed(log: string[][]) {
  return async (inputs: string[]) => {
    log.push(inputs);
    if (inputs.some((t) => t.includes("fake:batch"))) throw { status: 400, code: "context_length_exceeded" } satisfies EmbedError;
    return inputs.map((t) => [t.length, 1]);
  };
}
const passagesOf = (texts: string[]) => texts.map((text) => ({ text, start: 0, end: text.length, source: "" }));
const fresh = () => ({ next: 0, entries: [] as { passage: number; text: string; vector: number[] }[], skipped: [] as number[] });
const instant = { sleep: async () => {} };

describe("readFrom", () => {
  test("reads in batches of 32, one request at a time, and counts as it goes", async () => {
    const log: string[][] = [], seen: [number, number][] = [];
    const p = passagesOf(Array.from({ length: 70 }, (_, i) => `passage ${i}`));
    const s = fresh();
    await readFrom(s, p, fakeEmbed(log), { ...instant, onProgress: (done, total) => seen.push([done, total]) });
    expect(log.map((x) => x.length)).toEqual([32, 32, 6]);
    expect(s.entries.map((e) => e.passage)).toEqual(p.map((_, i) => i));
    expect(seen).toEqual([[32, 70], [64, 70], [70, 70]]);
    expect(s.next).toBe(70);
  });

  test("a batch too long for the model is halved down to one passage, then that passage twice at most, then skipped", async () => {
    const log: string[][] = [];
    const bad = "one two three four fake:batch five six seven eight";
    const p = passagesOf(["alpha", "beta", bad, "gamma", "delta"]);
    const s = fresh();
    await readFrom(s, p, fakeEmbed(log), instant);
    expect(s.skipped).toEqual([2]);
    // Everything else is read, and what was left of the long passage around the bad word is kept.
    expect(s.entries.filter((e) => e.passage !== 2).map((e) => e.text)).toEqual(["alpha", "beta", "gamma", "delta"]);
    const parts = s.entries.filter((e) => e.passage === 2).map((e) => e.text);
    expect(parts.length).toBeGreaterThan(0);
    expect(parts.some((t) => t.includes("fake:batch"))).toBe(false);
    // Never inside a word, and no more than two halvings of the passage: at most four parts tried.
    const tried = log.filter((x) => x.length === 1 && bad.includes(x[0]) && x[0] !== bad);
    expect(tried.length).toBeLessThanOrEqual(6);
    for (const t of tried) expect(bad.includes(t[0]) && words(t[0]).every((w) => words(bad).includes(w))).toBe(true);
  });

  test("busy (429 or 503 with Retry-After) waits that long and tries again, saying so", async () => {
    let calls = 0;
    const waits: number[] = [], slept: number[] = [];
    const embed = async (inputs: string[]) => { calls++; if (calls === 1) throw { status: 429, code: "inference_queue_full", retryAfter: 2 } satisfies EmbedError; return inputs.map(() => [1]); };
    const s = fresh();
    await readFrom(s, passagesOf(["a", "b"]), embed, { onBusy: (sec) => waits.push(sec), sleep: async (ms) => void slept.push(ms) });
    expect(waits).toEqual([2]);
    expect(slept).toEqual([2000]);
    expect(s.entries.length).toBe(2);
  });

  test("still busy after three waits, it stops and says so, keeping what it read", async () => {
    let calls = 0;
    const embed = async (inputs: string[]) => { calls++; if (calls === 1) return inputs.map(() => [1]); throw { status: 503, code: "model_unloading", retryAfter: 2 } satisfies EmbedError; };
    const s = fresh();
    const p = passagesOf(Array.from({ length: 40 }, (_, i) => `p${i}`));
    await expect(readFrom(s, p, embed, instant)).rejects.toMatchObject({ busy: true });
    expect(calls).toBe(1 + 4);
    expect(s.next).toBe(32);
    expect(s.entries.length).toBe(32);
  });

  test("a 503 without Retry-After is a failure, not a wait", async () => {
    let calls = 0, waits = 0;
    const embed = async () => { calls++; throw { status: 503, code: "service_unavailable" } satisfies EmbedError; };
    const err: any = await readFrom(fresh(), passagesOf(["a"]), embed, { ...instant, onBusy: () => waits++ }).catch((e) => e);
    expect(err.status).toBe(503);
    expect("busy" in err).toBe(false);
    expect([calls, waits]).toEqual([1, 0]);
  });

  test("a cancel part-way through a halved batch adds none of it, so nothing is read twice", async () => {
    const ctl = new AbortController();
    let n = 0;
    const embed = async (inputs: string[], signal?: AbortSignal) => {
      n++;
      if (n === 3) { ctl.abort(); signal?.throwIfAborted(); }
      if (inputs.some((t) => t.includes("fake:batch"))) throw { status: 400, code: "context_length_exceeded" } satisfies EmbedError;
      return inputs.map(() => [1]);
    };
    // The batch overflows; its first half ["a", "b"] is read; the cancel comes on the second half.
    const p = passagesOf(["a", "b", "fake:batch c", "d"]);
    const s = fresh();
    await expect(readFrom(s, p, embed, { ...instant, signal: ctl.signal })).rejects.toMatchObject({ name: "AbortError" });
    expect([s.next, s.entries.length, s.skipped.length]).toEqual([0, 0, 0]);
    await readFrom(s, p, embed, instant);
    expect(s.entries.map((e) => [e.passage, e.text])).toEqual([[0, "a"], [1, "b"], [2, "c"], [3, "d"]]);
    expect(s.skipped).toEqual([2]);
  });

  test("a cancel stops in the middle of a batch, and that batch is sent again on the next read", async () => {
    const log: string[][] = [];
    const ctl = new AbortController();
    const embed = async (inputs: string[], signal?: AbortSignal) => {
      log.push(inputs);
      if (log.length === 2) { ctl.abort(); signal?.throwIfAborted(); }
      return inputs.map(() => [1]);
    };
    const p = passagesOf(Array.from({ length: 70 }, (_, i) => `p${i}`));
    const s = fresh();
    await expect(readFrom(s, p, embed, { ...instant, signal: ctl.signal })).rejects.toMatchObject({ name: "AbortError" });
    expect(s.next).toBe(32);
    await readFrom(s, p, embed, instant);
    expect(log.map((x) => x[0])).toEqual(["p0", "p32", "p32", "p64"]);
    expect(s.entries.length).toBe(70);
  });
});
