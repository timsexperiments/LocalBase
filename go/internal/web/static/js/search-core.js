// Search's pure parts (DESIGN §35): text into passages, passages into vectors in batches, and
// passages ranked by cosine. search.js draws the page; nothing here touches it, the network or
// storage, so tools/jstest/search-core.test.ts can hold all of it.

// The page's own limits (§35.2), and how many times it waits on a busy computer before it stops.
export const LIMITS = { batch: 32, passageChars: 1000, passages: 2000, busyWaits: 3, shown: 5, most: 20 };

// splitText cuts text into passages of at most maxChars: first at blank lines, then at sentence ends,
// then at spaces, and never inside a word. A single word longer than maxChars stays whole. Each
// passage is a span of the source: text.slice(start, end) is its text, with no space at either end.
export function splitText(text, { source = "", maxChars = LIMITS.passageChars } = {}) {
  const out = [];
  const push = (start, end) => out.push({ text: text.slice(start, end), start, end, source });
  for (const [a, b] of paragraphs(text)) {
    if (b - a <= maxChars) { push(a, b); continue; }
    for (const [s, e] of group(sentences(text, a, b), maxChars, (span) => group(wordsIn(text, span[0], span[1]), maxChars))) push(s, e);
  }
  return out;
}

// paragraphs are the trimmed spans between blank lines.
function paragraphs(text) {
  const spans = [];
  const sep = /\n\s*\n/g;
  let from = 0, m;
  while ((m = sep.exec(text))) { spans.push([from, m.index]); from = m.index + m[0].length; }
  spans.push([from, text.length]);
  return spans.map(([a, b]) => trim(text, a, b)).filter(([a, b]) => b > a);
}

// sentences are the trimmed spans of [a, b) that end at . ! ? or … (and a closing quote or bracket),
// where a space follows. What's left after the last one is a sentence too.
function sentences(text, a, b) {
  const spans = [];
  const end = /[.!?…]+["'’”)\]]*(?=\s)/g;
  end.lastIndex = a;
  let from = a, m;
  while ((m = end.exec(text)) && m.index < b) {
    const stop = m.index + m[0].length;
    spans.push(trim(text, from, stop));
    from = stop;
  }
  spans.push(trim(text, from, b));
  return spans.filter(([s, e]) => e > s);
}

// wordsIn are the spans of [a, b) with no space in them.
function wordsIn(text, a, b) {
  const spans = [];
  const word = /\S+/g;
  word.lastIndex = a;
  let m;
  while ((m = word.exec(text)) && m.index < b) spans.push([m.index, Math.min(m.index + m[0].length, b)]);
  return spans;
}

// group joins consecutive spans while the joined span stays within max. A span longer than max on
// its own is handed to split, when there is one, or kept whole.
function group(spans, max, split) {
  const out = [];
  let cur = null;
  for (const span of spans) {
    if (span[1] - span[0] > max) {
      if (cur) { out.push(cur); cur = null; }
      if (split) out.push(...split(span)); else out.push(span);
      continue;
    }
    if (cur && span[1] - cur[0] <= max) cur = [cur[0], span[1]];
    else { if (cur) out.push(cur); cur = [span[0], span[1]]; }
  }
  if (cur) out.push(cur);
  return out;
}

function trim(text, a, b) {
  while (a < b && /\s/.test(text[a])) a++;
  while (b > a && /\s/.test(text[b - 1])) b--;
  return [a, b];
}

// capPassages keeps the first max passages and says how many it dropped.
export function capPassages(passages, max = LIMITS.passages) {
  return { passages: passages.slice(0, max), dropped: Math.max(0, passages.length - max) };
}

// cosine is right whether or not a model normalizes its vectors (§35.1). A zero vector scores 0.
export function cosine(a, b) {
  let dot = 0, na = 0, nb = 0;
  for (let i = 0; i < a.length; i++) { dot += a[i] * b[i]; na += a[i] * a[i]; nb += b[i] * b[i]; }
  return na && nb ? dot / (Math.sqrt(na) * Math.sqrt(nb)) : 0;
}

// rank gives the k vectors closest to the query, closest first; a tie keeps the source's order.
export function rank(query, vectors, k = LIMITS.most) {
  return vectors.map((v, index) => ({ index, score: cosine(query, v) }))
    .sort((x, y) => y.score - x.score || x.index - y.index)
    .slice(0, k);
}

// marks places each result between the closest and the least close of this one search, as rel from 0
// to 1, with a word for screen readers. A cosine means nothing across searches or models, so it is
// never shown as a number (§35.5).
export function marks(results) {
  if (results.length === 0) return [];
  const best = results[0].score, least = results.at(-1).score;
  return results.map((r) => {
    const rel = best === least ? 1 : Math.round(((r.score - least) / (best - least)) * 10000) / 10000;
    return { ...r, rel, word: rel >= 0.5 ? "Closer" : "Less close" };
  });
}

export function batches(items, size = LIMITS.batch) {
  const out = [];
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size));
  return out;
}

const sleepFor = (ms, signal) => new Promise((resolve, reject) => {
  const t = setTimeout(resolve, ms);
  signal?.addEventListener("abort", () => { clearTimeout(t); reject(signal.reason); }, { once: true });
});

// An EmbedError is what embed throws for an answer that isn't vectors: { status, code, retryAfter }.
// A busy answer (429, or 503 that says when to retry) is waited on and tried again. One that is
// still busy after LIMITS.busyWaits waits is thrown with busy: true. Anything else is thrown as is.
const isBusy = (e) => (e?.status === 429 || e?.status === 503) && typeof e.retryAfter === "number";
const isTooLong = (e) => e?.status === 400 && e.code === "context_length_exceeded";

// readFrom reads passages from state.next on, a batch at a time and one request at a time, and adds
// each batch's vectors to state only once the batch is done. So a cancel (signal) loses nothing, and
// the batch it stopped in is simply sent again by the next readFrom (§35.3).
// A batch the model can't take is halved down to one passage, then that passage is halved at a space
// twice at most. A part that still can't be read is skipped, and its passage is listed in state.skipped
// (§35.2). The parts around it are kept.
// embed(inputs, signal) resolves to one vector per input, in order.
export async function readFrom(state, passages, embed, { signal, onProgress, onBusy, sleep = sleepFor } = {}) {
  const send = async (inputs) => {
    for (let waited = 0; ; waited++) {
      signal?.throwIfAborted();
      try {
        return await embed(inputs, signal);
      } catch (e) {
        if (!isBusy(e)) throw e;
        if (waited >= LIMITS.busyWaits) throw Object.assign(e, { busy: true });
        onBusy?.(e.retryAfter);
        await sleep(e.retryAfter * 1000, signal);
      }
    }
  };
  const read = async (items, entries, skipped) => {
    try {
      const vectors = await send(items.map((it) => it.text));
      items.forEach((it, i) => entries.push({ passage: it.passage, text: it.text, vector: vectors[i] }));
    } catch (e) {
      if (!isTooLong(e)) throw e;
      if (items.length > 1) {
        const mid = Math.ceil(items.length / 2);
        await read(items.slice(0, mid), entries, skipped);
        await read(items.slice(mid), entries, skipped);
        return;
      }
      const it = items[0];
      const halves = it.halvings < 2 ? halve(it.text) : null;
      if (!halves) { if (!skipped.includes(it.passage)) skipped.push(it.passage); return; }
      for (const text of halves) await read([{ passage: it.passage, text, halvings: it.halvings + 1 }], entries, skipped);
    }
  };
  const total = passages.length;
  while (state.next < total) {
    signal?.throwIfAborted();
    const from = state.next;
    const batch = passages.slice(from, from + LIMITS.batch).map((p, i) => ({ passage: from + i, text: p.text, halvings: 0 }));
    const entries = [], skipped = [];
    await read(batch, entries, skipped);
    state.entries.push(...entries);
    state.skipped.push(...skipped);
    state.next = from + batch.length;
    onProgress?.(state.next, total);
  }
  return state;
}

// halve splits text at the space nearest its middle, or gives null when there's no space to split at.
function halve(text) {
  const mid = Math.floor(text.length / 2);
  let at = -1;
  for (let d = 0; d <= mid; d++) {
    if (/\s/.test(text[mid + d] ?? "")) { at = mid + d; break; }
    if (/\s/.test(text[mid - d] ?? "")) { at = mid - d; break; }
  }
  if (at <= 0) return null;
  const left = text.slice(0, at).trim(), right = text.slice(at).trim();
  return left && right ? [left, right] : null;
}
