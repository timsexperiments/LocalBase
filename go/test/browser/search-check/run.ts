// Drives headless Chrome for test/browser/search-check.sh: Search in Studio (DESIGN §35), on a --no-auth
// scratch gateway whose models are the fake llama-server. Every embeddings request the page sends is held
// in the browser (DevTools' Fetch domain), so each step decides what it gets: passed on to the gateway, kept
// waiting, or answered with an error the gateway can give. Exits 1 when any check fails.
import { mkdirSync } from "fs";
import { evaluate, openPage } from "../cdp/cdp.ts";

const [cdpBase, base] = process.argv.slice(2);
const work = new URL("../../../.cache/search-check/", import.meta.url).pathname;
mkdirSync(work, { recursive: true });
const page = await openPage(cdpBase, { errorText: (method, message) => method + message });
const { send, onEvent } = page;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const ev = (expression: string) => evaluate(send, expression, (details) => JSON.stringify(details).slice(0, 400));
let fails = 0; const check = (ok: boolean, msg: string) => { console.log((ok ? "ok   " : "FAIL ") + msg); if (!ok) fails++; };
const pageFits = () => `document.documentElement.scrollWidth <= document.documentElement.clientWidth`;

// --- the requests ------------------------------------------------------------------------------
type Seen = { id: string; net: string; model: string; inputs: string[]; dimensions: boolean; ui: string; url: string };
type Answer = "pass" | "hold" | { status: number; headers?: Record<string, string>; body: any };
const seen: Seen[] = [];
const held: Seen[] = [];
let rule: (s: Seen) => Answer = () => "pass";
const live = new Set<string>();
let most = 0;
const ended = new Map<string, { status?: number; failed?: string; canceled?: boolean }>();
const thrown: string[] = [];
onEvent((m: any) => {
  const p = m.params ?? {};
  if (m.method === "Runtime.exceptionThrown") thrown.push((p.exceptionDetails?.exception?.description ?? p.exceptionDetails?.text ?? "").slice(0, 300));
  if (m.method === "Fetch.requestPaused") {
    const raw = p.request.postData ?? (p.request.postDataEntries ?? []).map((e: any) => Buffer.from(e.bytes ?? "", "base64").toString()).join("");
    let body: any = {};
    try { body = JSON.parse(raw); } catch {}
    const header = Object.entries(p.request.headers ?? {}).find(([k]) => k.toLowerCase() === "x-localbase-ui");
    const s: Seen = { id: p.requestId, net: p.networkId, model: body.model, inputs: body.input ?? [], dimensions: "dimensions" in body, ui: String(header?.[1] ?? ""), url: p.request.url };
    seen.push(s);
    const what = rule(s);
    if (what === "pass") send("Fetch.continueRequest", { requestId: s.id }).catch(() => {});
    else if (what === "hold") held.push(s);
    else send("Fetch.fulfillRequest", {
      requestId: s.id, responseCode: what.status,
      responseHeaders: [{ name: "content-type", value: "application/json" }, ...Object.entries(what.headers ?? {}).map(([name, value]) => ({ name, value }))],
      body: Buffer.from(JSON.stringify(what.body)).toString("base64"),
    }).catch(() => {});
    return;
  }
  const url: string = p.request?.url ?? "";
  if (m.method === "Network.requestWillBeSent" && url.endsWith("/app/api/v1/embeddings")) { live.add(p.requestId); most = Math.max(most, live.size); }
  if (m.method === "Network.responseReceived" && live.has(p.requestId)) ended.set(p.requestId, { status: p.response.status });
  if (m.method === "Network.loadingFailed" && live.has(p.requestId)) ended.set(p.requestId, { ...ended.get(p.requestId), failed: p.errorText, canceled: p.canceled });
  if (m.method === "Network.loadingFinished" || m.method === "Network.loadingFailed") live.delete(p.requestId);
});
const release = () => { for (const s of held.splice(0)) send("Fetch.continueRequest", { requestId: s.id }).catch(() => {}); };
const error = (status: number, code: string, message: string, headers?: Record<string, string>): Answer => ({ status, headers, body: { error: { message, code, type: "error" } } });

// --- the page ----------------------------------------------------------------------------------
const nav = async (path: string) => { await send("Page.navigate", { url: base + path }); await sleep(1500); };
const ui = () => ev(`(() => {
  const q = (s) => document.querySelector(s), shown = (s) => !!q(s) && !q(s).hidden, text = (s) => shown(s) ? q(s).textContent : "";
  return { hello: shown("[data-search-empty]"), reading: shown("[data-reading]"), count: text("[data-reading-count]"), note: text("[data-reading-note]"),
    notes: shown("[data-search-notes]") ? [...q("[data-search-notes]").children].map((p) => p.textContent) : [],
    error: shown("[data-search-error]") ? q("[data-error-text]").textContent : "", again: text("[data-try-again]"), get: shown("[data-get-model]") ? q("[data-get-model]").getAttribute("href") : "",
    chip: q("[data-text-chip]").textContent, prompt: q("#lab-prompt").disabled ? "disabled" : "enabled", send: q("[data-search-form] button[type=submit]").disabled ? "disabled" : "enabled",
    results: shown("[data-results]") ? [...q("[data-result-list]").children].map((li) => li.querySelector("[id^=search-where]").textContent) : null,
    question: shown("[data-results]") ? q("[data-question]").textContent : "", more: shown("[data-more]"), box: q("[data-text-box]").value.length, said: q("#search-said").textContent };
})()`);
const until = async (test: (u: any) => boolean, ms = 10000) => {
  let u = await ui();
  for (const end = Date.now() + ms; !test(u) && Date.now() < end; u = await ui()) await sleep(100);
  return u;
};
const quietly = async (ms = 1500) => { const n = seen.length; await sleep(ms); return seen.length === n; };
// Paste through the sheet, as a person does: open it from the chip, put the text in, close it.
const pasteRaw = (text: string) => ev(`(() => {
  window.__closed = false;
  document.querySelector("#search-text").addEventListener("close", () => { window.__closed = true; }, { once: true });
  document.querySelector("[data-text-chip]").closest("button").click();
  const box = document.querySelector("[data-text-box]");
  box.value = ${JSON.stringify(text)};
  box.dispatchEvent(new Event("input", { bubbles: true }));
  document.querySelector("#search-text [data-close-sheet]").click();
})()`);
const pasted = async (text: string) => {
  const state = await ev(`({ closed: window.__closed, open: document.querySelector("#search-text").open, box: document.querySelector("[data-text-box]").value.length, active: document.activeElement?.id ?? document.activeElement?.tagName })`);
  if (state.box !== text.length || !state.closed) console.log("note: the paste did not take: " + JSON.stringify(state));
};
const paste = async (text: string) => {
  await pasteRaw(text);
  for (let i = 0; i < 50 && !(await ev(`window.__closed`)); i++) await sleep(50);
  await pasted(text);
};
const ask = (question: string) => ev(`(() => {
  const p = document.querySelector("#lab-prompt");
  p.value = ${JSON.stringify(question)};
  p.dispatchEvent(new Event("input", { bubbles: true }));
  document.querySelector("[data-search-form]").requestSubmit();
})()`);
const click = (selector: string) => ev(`document.querySelector(${JSON.stringify(selector)}).click()`);
const done = (ms?: number) => until((u) => !u.reading, ms);
// The text of n passages, each with words of its own, and the word "zebra" only in the one at `zebra`.
const passages = (n: number, tag: string, zebra = -1) => Array.from({ length: n }, (_, i) => i === zebra
  ? "Zebra stripes."
  : `Note ${tag}${i + 1} talks about item${tag}${i + 1} and nothing else at all.`);
const textOf = (list: string[]) => list.join("\n\n");
// The fake backend (internal/runtime/fakebackend) counts each word, lowercased, into one of 64 places by its FNV-1a
// hash. Rebuilt here, the order the page should give is known: by cosine, closest first, ties in the text's order.
const fakeVector = (text: string) => {
  const v = new Array(64).fill(0);
  for (const word of text.toLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean)) {
    let h = 2166136261;
    for (const b of new TextEncoder().encode(word)) h = Math.imul(h ^ b, 16777619) >>> 0;
    v[h % 64]++;
  }
  return v;
};
const cosineOf = (a: number[], b: number[]) => {
  let dot = 0, na = 0, nb = 0;
  for (let i = 0; i < a.length; i++) { dot += a[i] * b[i]; na += a[i] * a[i]; nb += b[i] * b[i]; }
  return na && nb ? dot / (Math.sqrt(na) * Math.sqrt(nb)) : 0;
};
const expected = (question: string, list: string[], k: number, skip: number[] = []) => list.map((t, i) => ({ i, s: cosineOf(fakeVector(question), fakeVector(t)) }))
  .filter((x) => !skip.includes(x.i)).sort((a, b) => b.s - a.s || a.i - b.i).slice(0, k).map((x) => `Passage ${x.i + 1}`);
const firstInputs = (from: number) => seen.slice(from).map((s) => s.inputs[0]);

await send("Page.enable");
await send("Runtime.enable");
await send("Network.enable");
await send("Emulation.setFocusEmulationEnabled", { enabled: true });
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await send("Fetch.enable", { patterns: [{ urlPattern: "*/app/api/v1/embeddings*", requestStage: "Request" }] });
const readable = await send("Browser.grantPermissions", { origin: new URL(base).origin, permissions: ["clipboardReadWrite", "clipboardSanitizedWrite"] }).then(() => true, () => false);

// 1. The page, and a first visit.
await nav("/app/studio/search");
const models: string[] = await ev(`[...document.querySelectorAll("[data-search-form] input[name=model]")].map((i) => i.value)`);
check(models.length === 2 && models.every((id) => /embedding/.test(id)), `the page offers the two embedding models and nothing else: ${JSON.stringify(models)}`);
let u = await ui();
check(u.hello && u.prompt === "disabled" && u.send === "disabled" && u.chip === "Text" && !u.reading && u.results === null, `a first visit asks for text, and the box waits for it: ${JSON.stringify(u)}`);
let choosers = 0;
onEvent((m: any) => { if (m.method === "Page.fileChooserOpened") choosers++; });
await send("Page.setInterceptFileChooserDialog", { enabled: true });
const press = async (expression: string) => {
  const at = await ev(`(() => { const r = (${expression}).getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
  for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: at.x, y: at.y, button: "left", clickCount: 1 });
  await sleep(400);
};
await press(`document.querySelector("[data-search-empty] [data-pick-text]")`);
const fromFirst = choosers;
await ev(`document.querySelector("[data-text-chip]").closest("button").click()`);
await sleep(300);
await press(`document.querySelector("#search-text [data-pick-text]")`);
await ev(`document.querySelector("#search-text [data-close-sheet]").click()`);
check(fromFirst === 1 && choosers === 2, `Choose a file opens the file chooser, from the first visit and from the sheet: ${JSON.stringify({ fromFirst, choosers })}`);
await send("Page.setInterceptFileChooserDialog", { enabled: false });

// 2. Reading 70 passages: one request at a time, 32 at most in each, and a count while it reads.
rule = () => "hold";
let from = seen.length;
const seventy = passages(70, "a", 40);
await paste(textOf(seventy));
u = await until((x) => x.reading && held.length === 1);
check(u.count === "Reading your text… 0 of 70 passages" && u.note === "Loading the search model. The first search can take a minute." && u.chip === "Text · 70 passages", `while the first request waits, the count and the loading line show: ${JSON.stringify({ count: u.count, note: u.note, chip: u.chip })}`);
check(await quietly() && held.length === 1, `one request at a time: nothing more is sent while one waits (${seen.length - from} sent)`);
const first = seen[from];
check(first.url === base + "/app/api/v1/embeddings" && first.ui === "1" && !first.dimensions && first.model === models[0] && first.inputs.length === 32 && first.inputs[0] === seventy[0], `the request: same origin, the UI header, the first model, no dimensions, 32 passages from the first: ${JSON.stringify({ ...first, inputs: first.inputs.length })}`);
rule = () => "pass";
release();
u = await done();
check(JSON.stringify(seen.slice(from).map((s) => s.inputs.length)) === "[32,32,6]" && most === 1, `70 passages went as 32, 32 and 6, never two at once: ${JSON.stringify(seen.slice(from).map((s) => s.inputs.length))}, at most ${most}`);
check(!u.reading && u.chip === "Text · 70 passages" && u.prompt === "enabled" && u.send === "enabled" && !u.hello && u.error === "", `read: the chip counts the passages and the box takes a question: ${JSON.stringify(u)}`);

// 3. A question: the closest passages first, five of them, with a mark and no number.
from = seen.length;
await ask("zebra stripes");
u = await until((x) => x.results !== null);
check(seen.length - from === 1 && JSON.stringify(seen[from]?.inputs) === `["zebra stripes"]`, `the question is one request of its own: ${JSON.stringify(seen.slice(from).map((s) => s.inputs))}`);
check(u.question === "zebra stripes" && u.results?.length === 5 && u.results[0] === "Passage 41" && JSON.stringify(u.results) === JSON.stringify(expected("zebra stripes", seventy, 5)) && u.more, `the question heads five results in cosine order, the zebra passage first, with Show more: ${JSON.stringify(u.results)} ${u.question}`);
const marks = await ev(`[...document.querySelectorAll("[data-result-list] > li")].map((li) => ({ role: li.querySelector("[role=img]")?.getAttribute("role"), name: li.querySelector("[role=img]")?.getAttribute("aria-label"), text: li.textContent }))`);
check(marks[0].name === "Closer" && marks.every((m: any) => m.role === "img" && (m.name === "Closer" || m.name === "Less close")) && !marks.some((m: any) => /%|\d\.\d/.test(m.text.replace(/Passage \d+/, ""))), `each result has a mark named Closer or Less close, and no score or percentage: ${JSON.stringify(marks.map((m: any) => m.name))}`);
check(u.said === "zebra stripes. Closest in meaning", `the results are announced: ${JSON.stringify(u.said)}`);

// 4. Show more, up to 20, and focus goes to the first new one.
await click("[data-more]");
u = await ui();
const focused = await ev(`[...document.querySelectorAll("[data-result-list] > li")].indexOf(document.activeElement)`);
check(u.results?.length === 20 && !u.more && focused === 5, `Show more shows 20 and moves to the sixth: ${u.results?.length}, more ${u.more}, focus ${focused}`);

// 5. Copy gives the passage's text.
const at = await ev(`(() => { const b = document.querySelector("[data-result-list] > li [data-copy-passage]"); b.scrollIntoView({ block: "center" }); const r = b.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: at.x, y: at.y, button: "left", clickCount: 1 });
await sleep(300);
const copied = await ev(`({ button: document.querySelector("[data-result-list] > li [data-copy-passage]").textContent, said: document.querySelector("#search-said").textContent })`);
const clip = readable ? await ev(`navigator.clipboard.readText().catch((e) => "unreadable: " + e.message)`) : "not readable here";
check(copied.button === "Copied" && copied.said === "Copied." && (clip === seventy[40] || !readable), `Copy copies the passage and says so: ${JSON.stringify(copied)}, clipboard ${JSON.stringify(clip)}`);

// 6. Another model reads the text again and asks the question again (vectors of two models can't be compared).
rule = () => "hold";
from = seen.length;
await ev(`document.querySelectorAll("[data-search-form] input[name=model]")[1].click()`);
u = await until((x) => held.length === 1);
check(u.notes.includes("Changing the model reads your text again.") && seen[from]?.model === models[1] && seen[from]?.inputs[0] === seventy[0] && u.results === null, `a new model says it reads the text again, and does, from the start: ${JSON.stringify({ notes: u.notes, model: seen[from]?.model, results: u.results })}`);
rule = () => "pass";
release();
u = await until((x) => x.results !== null);
check(seen.slice(from).every((s) => s.model === models[1]) && JSON.stringify(seen.slice(from).map((s) => s.inputs.length)) === "[32,32,6,1]" && u.question === "zebra stripes" && JSON.stringify(u.results) === JSON.stringify(expected("zebra stripes", seventy, 5)), `with the new model: 3 batches, then the same question again: ${JSON.stringify(seen.slice(from).map((s) => s.inputs.length))} ${JSON.stringify(u.results)}`);
await ev(`document.querySelectorAll("[data-search-form] input[name=model]")[0].click()`);
await until((x) => x.results !== null); // read again with the first model, and the question asked again

// 7. Busy mid-way (429 with Retry-After): it waits, says so, sends that batch again, and keeps the batches done.
let k = 0;
rule = () => (++k === 2 ? error(429, "inference_queue_full", "The queue is full.", { "retry-after": "1" }) : "pass");
from = seen.length;
const hundred = passages(100, "b");
await paste(textOf(hundred));
u = await until((x) => x.note.startsWith("This computer is busy"), 5000);
check(u.note === "This computer is busy. Trying again in 1 s…" && u.reading, `a 429 mid-way says it is waiting: ${JSON.stringify(u.note)}`);
u = await done();
check(JSON.stringify(firstInputs(from)) === JSON.stringify([hundred[0], hundred[32], hundred[32], hundred[64], hundred[96]]) && u.chip === "Text · 100 passages" && u.error === "", `after the wait the same batch is sent again, and the one before it isn't: ${JSON.stringify(firstInputs(from).map((t) => t.split(" ")[1]))}`);

// 8. Still busy after three waits: it stops, says so, keeps the text, and Try again goes on from that batch.
k = 0;
rule = () => (++k >= 2 ? error(503, "insufficient_memory", "Not enough memory right now.", { "retry-after": "1" }) : "pass");
from = seen.length;
const forty = passages(40, "c");
await paste(textOf(forty));
u = await until((x) => x.error !== "", 15000);
check(u.error === "Still busy. Try again in a minute." && u.again === "Try again" && u.box > 0 && u.chip === "Text · 40 passages" && seen.length - from === 5, `after 3 waits it stops and keeps the text: ${JSON.stringify({ error: u.error, again: u.again, box: u.box, sent: seen.length - from })}`);
rule = () => "pass";
const resumed = seen.length;
await click("[data-try-again]");
u = await done();
check(seen[resumed]?.inputs[0] === forty[32] && seen.length - resumed === 1 && u.error === "" && u.chip === "Text · 40 passages", `Try again sends only what is left: ${JSON.stringify(firstInputs(resumed).map((t) => t.split(" ")[1]))}`);

// 9. Each answer the page must handle says its own line, and the text stays (§35.3).
const cases: { answer: Answer; text: string; again: string; get?: boolean }[] = [
  { answer: error(503, "insufficient_memory", "Not enough memory to load this model right now."), text: "Not enough memory to load this model right now.", again: "Try again" },
  { answer: error(502, "backend_error", "upstream failed"), text: "The model stopped answering.", again: "Try again" },
  { answer: error(404, "model_not_found", "not found"), text: "That model isn't selected any more. Choose another.", again: "" },
  { answer: error(409, "model_not_installed", "not installed"), text: "That model isn't installed yet.", again: "", get: true },
  { answer: error(422, "model_too_large", "too large"), text: "That model doesn't fit on this computer. Choose another.", again: "" },
  { answer: error(403, "forbidden", "forbidden"), text: "Your access does not include search.", again: "" },
  { answer: error(401, "unauthorized", "signed out"), text: "Your session ended. Reload the page to sign in again.", again: "Reload" },
];
for (const [i, c] of cases.entries()) {
  rule = () => c.answer;
  from = seen.length;
  await paste(textOf(passages(3, `e${i}`)));
  u = await until((x) => x.error !== "");
  const quiet = await quietly(1200);
  const status = (c.answer as any).status;
  check(u.error === c.text && u.again === c.again && (c.get ? u.get === "/app/models/get?filter=embeddings" : u.get === "") && u.box > 0 && quiet && seen.length - from === 1 && !u.reading, `${status} says ${JSON.stringify(c.text)}${c.again ? " with " + c.again : ""}${c.get ? " and Get it" : ""}, once, and keeps the text: ${JSON.stringify({ error: u.error, again: u.again, get: u.get, sent: seen.length - from })}`);
}
rule = () => "pass";

// 10. Cancel stops the request it is waiting on and sends nothing more. What was read is kept.
k = 0;
rule = () => (++k === 2 ? "hold" : "pass");
from = seen.length;
const cancelled = passages(70, "d", 10);
await paste(textOf(cancelled));
await until(() => held.length === 1);
const waiting = held[0];
await click("[data-cancel]");
const calm = await quietly(2000);
u = await ui();
const end = ended.get(waiting.net);
check(!u.reading && end?.canceled === true && calm && u.error === "" && u.chip === "Text · 70 passages", `Cancel ends the request in flight and sends nothing more: ${JSON.stringify({ reading: u.reading, ended: end, sent: seen.length - from })}`);
held.splice(0);
rule = () => "pass";
const again = seen.length;
await ask("zebra stripes");
u = await until((x) => x.results !== null);
check(JSON.stringify(firstInputs(again)) === JSON.stringify([cancelled[32], cancelled[64], "zebra stripes"]) && u.results?.[0] === "Passage 11" && JSON.stringify(u.results) === JSON.stringify(expected("zebra stripes", cancelled, 5)), `asking after Cancel reads only the rest, then answers: ${JSON.stringify(firstInputs(again).map((t) => t.split(" ").slice(0, 2).join(" ")))}`);
const runs = await ev(`fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => b.data.length)`);
check(runs === 0, `a search is not a run: ${runs} runs`);

// 11. A passage too long to read is skipped and said, and the rest is searched (fake:batch overflows a request).
from = seen.length;
const long = ["Apples are red.", "Pears are green.", "fake:batch", "Plums are purple.", "Limes are sour."];
await paste(textOf(long));
u = await done();
const statuses = seen.slice(from).map((s) => ended.get(s.net)?.status);
check(u.notes.includes("1 passage was too long to read, so it isn't searched.") && u.chip === "Text · 5 passages" && statuses.includes(400) && u.error === "", `one passage too long is skipped and said: ${JSON.stringify({ notes: u.notes, sizes: seen.slice(from).map((s) => s.inputs.length), statuses })}`);
await ask("green pears");
u = await until((x) => x.results !== null);
check(u.results?.length === 4 && JSON.stringify(u.results) === JSON.stringify(expected("green pears", long, 4, [2])), `the skipped passage is never a result, and the rest are in cosine order: ${JSON.stringify(u.results)}`);
await paste(textOf(["One.", "fake:batch", "Two.", "fake:batch"]));
u = await done();
check(u.notes.includes("2 passages were too long to read, so they aren't searched."), `two too long are counted: ${JSON.stringify(u.notes)}`);

// 12. One passage is one, not ones; text with nothing in it; and too much text.
rule = () => "hold";
await paste("Just one passage here.");
u = await until(() => held.length === 1);
check(u.chip === "Text · 1 passage" && u.count === "Reading your text… 0 of 1 passage", `one passage is counted as one: ${JSON.stringify({ chip: u.chip, count: u.count })}`);
rule = () => "pass";
release();
await done();
from = seen.length;
await paste("   \n\n   \n");
u = await ui();
check(u.notes.includes("This text has nothing to search in it.") && u.prompt === "disabled" && await quietly(800), `blank text says there's nothing to search and sends nothing: ${JSON.stringify({ notes: u.notes, prompt: u.prompt })}`);
from = seen.length;
most = 0;
await paste(textOf(passages(2050, "f")));
u = await until((x) => x.chip !== "Text" && !x.reading, 60000);
const sizes = seen.slice(from).map((s) => s.inputs.length);
check(u.notes.includes("That's more than 2,000 passages. Only the first 2,000 will be searched.") && u.chip === "Text · 2,000 passages" && sizes.length === 63 && sizes.at(-1) === 16 && Math.max(...sizes) === 32 && most === 1, `2,050 passages: only 2,000 are read, as 63 requests one at a time: ${sizes.length} requests, last ${sizes.at(-1)}, at most ${most} at once, ${JSON.stringify(u.notes)}`);

// 13. A file is read in the page: its name goes with each passage, and a file that isn't text is refused.
const file = work + "notes.md";
await Bun.write(file, "# Garden\n\nTomatoes need sun.\n\nThe zebra came to the garden gate.\n");
const fileInput = async (path: string) => {
  const { root } = await send("DOM.getDocument", { depth: -1 });
  const { nodeId } = await send("DOM.querySelector", { nodeId: root.nodeId, selector: "[data-text-file]" });
  await send("DOM.setFileInputFiles", { nodeId, files: [path] });
};
await fileInput(file);
u = await until((x) => x.chip === "Text · 3 passages" && !x.reading);
const boxText = await ev(`document.querySelector("[data-text-box]").value`);
await ask("zebra gate");
u = await until((x) => x.results !== null);
check(boxText.startsWith("# Garden") && u.chip === "Text · 3 passages" && u.results?.[0] === "Passage 3 · notes.md" && u.results.every((r: string) => r.endsWith(" · notes.md")), `a chosen file's text is searched, with its name: ${JSON.stringify(u.results)}`);
const binary = work + "picture.txt";
await Bun.write(binary, new Uint8Array([0xff, 0xd8, 0xff, 0x00, 0x10, 0x4a, 0x46]));
await fileInput(binary);
u = await until((x) => x.error !== "");
check(u.error === "Choose a UTF-8 text or code file. PDF, Office documents, and archives are not supported." && u.chip === "Text · 3 passages", `a file that isn't text is refused, and the text stays: ${JSON.stringify({ error: u.error, chip: u.chip })}`);

// 14. Nothing is kept in the browser (§35.5), and leaving the page forgets the text.
const PLANT = "Planted-7f3a quokka";
await paste(textOf(["Something ordinary.", `The ${PLANT} lives on an island.`]));
await done();
await ask("quokka island");
await until((x) => x.results !== null);
const stored = () => ev(`(async () => {
  const all = (s) => Object.keys(s).map((k) => k + "=" + s.getItem(k)).join("\\n");
  const dbs = indexedDB.databases ? (await indexedDB.databases()).map((d) => d.name) : [];
  const caches_ = typeof caches === "undefined" ? [] : await caches.keys();
  return { local: all(localStorage), session: all(sessionStorage), dbs, caches: caches_, cookie: document.cookie };
})()`);
const shown = await ev(`document.querySelector("[data-result-list]").textContent.includes("7f3a")`);
let s = await stored();
check(shown && !JSON.stringify(s).includes("7f3a") && s.dbs.length === 0, `after a search no storage holds the text, which the page shows: shown ${shown}, ${JSON.stringify(s).slice(0, 300)}`);
// Away by a link in the page (htmx), then back.
await ev(`document.querySelector("a[href='/app/studio']").click()`);
await sleep(1500);
s = await stored();
check(!JSON.stringify(s).includes("7f3a"), `leaving by a link stores nothing of it: ${JSON.stringify(s).slice(0, 300)}`);
await ev(`history.back()`);
await sleep(2000);
u = await ui();
const back1 = await ev(`document.body.innerHTML.includes("7f3a")`);
check(u.hello && u.chip === "Text" && u.box === 0 && u.results === null && !back1, `Back after a link: the text is forgotten: ${JSON.stringify({ hello: u.hello, chip: u.chip, box: u.box, inPage: back1 })}`);
// Away by a whole navigation, then back (the browser may keep the page to come back to).
await paste(textOf([`The ${PLANT} again.`]));
await done();
await nav("/app/models");
await ev(`history.back()`);
await sleep(2000);
u = await ui();
const back2 = await ev(`document.body.innerHTML.includes("7f3a") || document.querySelector("[data-text-box]").value.includes("7f3a")`);
check(u.hello && u.chip === "Text" && u.box === 0 && !back2, `Back after leaving the page: the text is forgotten: ${JSON.stringify({ hello: u.hello, chip: u.chip, box: u.box, inPage: back2 })}`);

// 15. 320 px: nothing is wider than the screen, with results, the sheet open, or an error.
await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 640, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/search");
check(await ev(pageFits()), `the first visit fits 320 px`);
await paste(textOf(passages(12, "g", 3).map((t, i) => i === 0 ? "A-very-long-word-without-spaces-" + "x".repeat(120) : t)));
await done();
await ask("zebra stripes on the savanna, asked as a long question that wraps");
await until((x) => x.results !== null);
check(await ev(pageFits()), `results fit 320 px`);
await ev(`document.querySelector("[data-text-chip]").closest("button").click()`);
await sleep(300);
const sheet = await ev(`(() => { const r = document.querySelector("#search-text").getBoundingClientRect(); return { left: r.left, right: r.right, width: document.documentElement.clientWidth, focus: document.activeElement?.id }; })()`);
check(sheet.left >= 0 && sheet.right <= sheet.width && sheet.focus === "search-text-box", `the text sheet fits 320 px and opens on the box: ${JSON.stringify(sheet)}`);
await ev(`document.querySelector("#search-text [data-close-sheet]").click()`);
rule = () => error(502, "backend_error", "upstream failed");
await paste(textOf(passages(2, "h")));
await until((x) => x.error !== "");
check(await ev(pageFits()), `an error fits 320 px`);

check(thrown.length === 0, `no exception in the page: ${JSON.stringify(thrown)}`);
console.log(fails === 0 ? "all pass" : `${fails} FAIL`);
page.ws.close();
process.exit(fails === 0 ? 0 : 1);
