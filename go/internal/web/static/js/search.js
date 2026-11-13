// Search (DESIGN §35): give it some text, ask a question, and see the passages closest in meaning.
// The text is split into passages here, sent to this LocalBase's /v1/embeddings to be turned into
// vectors, and ranked here (search-core.js). The text and the vectors live in this module's memory
// and nowhere else: not in localStorage, sessionStorage or IndexedDB, and the server keeps none of
// it (§35.5). Leaving the page, by a link or by closing it, forgets them.
import { LIMITS, capPassages, closest, marks, readFrom, splitText } from "./search-core.js";

const NOT_TEXT = "Choose a UTF-8 text or code file. PDF, Office documents, and archives are not supported.";

// What the page holds. All of it is dropped by forget().
let root = null;
let doc = null; // the text: { text, source, passages, dropped }
let file = null; // the file the text came from, while the box still holds its text: { name, text }
let read = null; // what one model made of doc: { model, state } (state is readFrom's)
let job = null; // what is being sent now: { kind: "text" | "question", model, controller, busy }
let pending = null; // a question waiting for the text to be read
let lastQuestion = "";
let results = null; // { question, hits, shown }
let error = null; // { text, retry, reload, get }
let modelChanged = false;
const answered = new Set(); // models that have answered on this page: the first request to another may load it

const $ = (selector) => root?.querySelector(selector);
const form = () => $("[data-search-form]");
const textBox = () => $("[data-text-box]");
const prompt = () => document.getElementById("lab-prompt");
const model = () => form()?.elements.model?.value ?? "";
const count = (n) => (n === 1 ? "1 passage" : `${n.toLocaleString("en-US")} passages`);

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") node.className = value;
    else node.setAttribute(key, value);
  }
  node.append(...children);
  return node;
}

function say(text) {
  const status = document.getElementById("search-said");
  if (status) status.textContent = text;
}

// --- talking to LocalBase ------------------------------------------------------------

class SessionEnded extends Error {}

// embed turns inputs into vectors with one model, one request. What isn't vectors is thrown as
// { status, code, retryAfter }, for readFrom to wait on, split or give up on (§35.3).
async function embed(modelID, inputs, signal) {
  let response;
  try {
    response = await fetch("/app/api/v1/embeddings", {
      method: "POST",
      signal,
      redirect: "manual", // an expired sign-in in front of LocalBase reads as that, not as a network error
      credentials: "same-origin",
      cache: "no-store",
      headers: { "content-type": "application/json", "x-localbase-ui": "1" },
      body: JSON.stringify({ model: modelID, input: inputs }),
    });
  } catch (e) {
    if (signal?.aborted) throw e;
    throw new Error("Can't reach LocalBase right now. Check the connection and try again.");
  }
  if (response.type === "opaqueredirect" || (response.status >= 300 && response.status < 400))
    throw new SessionEnded("Your sign-in expired. Reload to sign in again.");
  if (response.status === 401) throw new SessionEnded("Your session ended. Reload the page to sign in again.");
  const value = await response.json().catch(() => null);
  signal?.throwIfAborted();
  if (!response.ok) {
    const after = response.headers.get("retry-after")?.trim();
    const message = typeof value?.error?.message === "string" ? value.error.message : `Request failed (${response.status}). Try again.`;
    throw Object.assign(new Error(message), {
      status: response.status,
      code: value?.error?.code,
      retryAfter: after && /^\d+$/.test(after) ? Number(after) : undefined,
    });
  }
  const data = Array.isArray(value?.data) ? [...value.data].sort((a, b) => a.index - b.index) : [];
  if (data.length !== inputs.length || !data.every((d) => Array.isArray(d.embedding)))
    throw Object.assign(new Error("The model stopped answering."), { status: 502 });
  answered.add(modelID);
  return data.map((d) => Float32Array.from(d.embedding));
}

// failure is what the page says about an error, and what it offers (§35.3).
function failure(e, retry) {
  if (e instanceof SessionEnded) return { text: e.message, reload: true };
  if (e.busy) return { text: "Still busy. Try again in a minute.", retry };
  switch (e.status) {
    case undefined: return { text: e.message, retry };
    case 403: return { text: "Your access does not include search." };
    case 404: return { text: "That model isn't selected any more. Choose another." };
    case 409: return { text: "That model isn't installed yet.", get: true };
    case 422: return { text: "That model doesn't fit on this computer. Choose another." };
    case 502: return { text: "The model stopped answering.", retry };
  }
  return { text: e.message, retry };
}

// --- reading the text ------------------------------------------------------------------

// takeText reads what the box holds now. Only a change starts again: the same text keeps what was read.
function takeText() {
  const text = textBox()?.value ?? "";
  const source = file && file.text === text ? file.name : "";
  if (doc ? doc.text === text && doc.source === source : text === "") return;
  stop();
  read = null;
  results = null;
  pending = null;
  error = null;
  modelChanged = false;
  if (text === "") doc = null;
  else doc = { text, source, ...capPassages(splitText(text, { source })) };
  draw();
  if (doc?.passages.length) readText();
}

function stop() {
  job?.controller.abort();
  job = null;
}

// readText sends what isn't read yet, a batch at a time. A cancel, a failure or a busy computer keeps
// every batch already read, and the next readText goes on from there.
async function readText() {
  if (!doc?.passages.length || job) return;
  const m = model();
  if (read?.model !== m) read = { model: m, state: { next: 0, entries: [], skipped: [] } };
  const mine = read, passages = doc.passages;
  if (mine.state.next < passages.length) {
    const me = (job = { kind: "text", model: m, controller: new AbortController(), busy: 0 });
    error = null;
    draw();
    say(`Reading your text… ${mine.state.next.toLocaleString("en-US")} of ${count(passages.length)}`);
    try {
      await readFrom(mine.state, passages, (inputs, signal) => embed(m, inputs, signal), {
        signal: me.controller.signal,
        onProgress: () => { me.busy = 0; drawWork(); },
        onBusy: (seconds) => { me.busy = seconds; drawWork(); say(busyLine(seconds)); },
      });
    } catch (e) {
      if (job !== me) return; // cancelled, or the text, the model or the page changed
      job = null;
      error = failure(e, readText);
      draw();
      return;
    }
    if (job !== me) return;
    job = null;
    draw();
    say(skippedLine() ? `Text · ${count(passages.length)}. ${skippedLine()}` : `Text · ${count(passages.length)}`);
  }
  if (pending !== null) {
    const question = pending;
    pending = null;
    ask(question);
  }
}

const busyLine = (seconds) => `This computer is busy. Trying again in ${seconds} s…`;

function skippedLine() {
  const n = read?.state.skipped.length ?? 0;
  if (n === 0) return "";
  return n === 1 ? "1 passage was too long to read, so it isn't searched." : `${n.toLocaleString("en-US")} passages were too long to read, so they aren't searched.`;
}

// --- asking ----------------------------------------------------------------------------

async function ask(question) {
  lastQuestion = question;
  if (!doc?.passages.length) return;
  if (job?.kind === "text" || read?.model !== model() || read.state.next < doc.passages.length) {
    pending = question; // asked while the text is still being read: it's answered once it is
    readText();
    return;
  }
  stop();
  const mine = read;
  if (mine.state.entries.length === 0) return draw(); // every passage was too long, and the note says so
  const me = (job = { kind: "question", model: mine.model, controller: new AbortController(), busy: 0 });
  error = null;
  draw();
  let vector;
  try {
    const q = { next: 0, entries: [], skipped: [] };
    await readFrom(q, [{ text: question }], (inputs, signal) => embed(mine.model, inputs, signal), {
      signal: me.controller.signal,
      onBusy: (seconds) => { me.busy = seconds; drawWork(); say(busyLine(seconds)); },
    });
    if (q.entries.length === 0) throw Object.assign(new Error("Request failed (400). Try again."), { status: 400 });
    // A question too long for one input was read in parts; together they point where it does.
    vector = q.entries.reduce((sum, e) => sum.map((x, i) => x + e.vector[i]), new Float32Array(q.entries[0].vector.length));
  } catch (e) {
    if (job !== me) return;
    job = null;
    error = failure(e, () => ask(question));
    draw();
    return;
  }
  if (job !== me || read !== mine) return;
  job = null;
  const hits = marks(closest(vector, mine.state.entries));
  results = { question, hits, shown: Math.min(LIMITS.shown, hits.length) };
  draw();
  drawResults();
  say(`${question}. Closest in meaning`);
}

// --- drawing ---------------------------------------------------------------------------

function draw() {
  if (!root?.isConnected) return;
  const ready = !!doc?.passages.length;
  $("[data-search-empty]").hidden = !!doc;
  $("[data-text-chip]").textContent = ready ? `Text · ${count(doc.passages.length)}` : "Text";
  const box = prompt();
  if (box) box.disabled = !ready;
  const send = form()?.querySelector("button[type=submit]");
  if (send) send.disabled = !ready || job?.kind === "question";
  drawWork();
  drawNotes();
  drawError();
  const shown = !!results;
  $("[data-results]").hidden = !shown;
  if (!shown) $("[data-result-list]").replaceChildren();
}

function drawWork() {
  const panel = $("[data-reading]");
  if (!panel) return;
  const reading = job?.kind === "text";
  const loading = !!job && !answered.has(job.model);
  panel.hidden = !job || (!reading && !job.busy && !loading);
  if (panel.hidden) return;
  const countLine = $("[data-reading-count]");
  const meter = $("[data-reading-level]")?.parentElement;
  countLine.hidden = meter.hidden = !reading;
  if (reading) {
    const done = read.state.next, total = doc.passages.length;
    countLine.textContent = `Reading your text… ${done.toLocaleString("en-US")} of ${count(total)}`;
    $("[data-reading-level]").style.width = `${Math.round((done / total) * 100)}%`;
  }
  const note = $("[data-reading-note]");
  note.textContent = job.busy ? busyLine(job.busy) : loading ? "Loading the search model. The first search can take a minute." : "";
  note.hidden = !note.textContent;
}

function drawNotes() {
  const box = $("[data-search-notes]");
  const lines = [];
  if (doc && !doc.passages.length) lines.push("This text has nothing to search in it.");
  if (doc?.dropped) lines.push("That's more than 2,000 passages. Only the first 2,000 will be searched.");
  if (modelChanged && doc?.passages.length) lines.push("Changing the model reads your text again.");
  if (skippedLine()) lines.push(skippedLine());
  box.replaceChildren(...lines.map((line) => el("p", {}, line)));
  box.hidden = lines.length === 0;
}

function drawError() {
  const box = $("[data-search-error]");
  box.hidden = !error;
  $("[data-error-text]").textContent = error?.text ?? "";
  const again = $("[data-try-again]");
  again.hidden = !error?.retry && !error?.reload;
  again.textContent = error?.reload ? "Reload" : "Try again";
  const get = $("[data-get-model]");
  if (get) get.hidden = !error?.get;
}

function drawResults() {
  const list = $("[data-result-list]");
  if (!results || !list) return;
  $("[data-question]").textContent = results.question;
  list.replaceChildren(...results.hits.slice(0, results.shown).map(hitItem));
  $("[data-more]").hidden = results.shown >= results.hits.length;
}

// One result: where it came from, how close it is next to the others of this search (§35.5: no number),
// its text, and Copy.
function hitItem(hit) {
  const passage = doc.passages[hit.passage];
  const where = `search-where-${hit.passage}`;
  const fill = el("i");
  fill.style.width = `${Math.round(15 + hit.rel * 85)}%`;
  return el("li", { class: "grid gap-2 rounded-2xl border border-bound bg-surface p-4", tabindex: "-1" },
    el("div", { class: "flex items-center gap-3 text-sm text-muted" },
      el("span", { class: "transcribe-meter w-12 shrink-0", role: "img", "aria-label": hit.word }, fill),
      el("span", { id: where }, passage.source ? `Passage ${hit.passage + 1} · ${passage.source}` : `Passage ${hit.passage + 1}`)),
    el("p", { class: "whitespace-pre-wrap wrap-anywhere" }, passage.text),
    el("button", { type: "button", class: "btn btn-quiet justify-self-start", "aria-describedby": where, "data-copy-passage": String(hit.passage) }, "Copy"));
}

// --- what the person does -----------------------------------------------------------------

async function takeFile(chosen) {
  if (!chosen) return;
  let text;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(await chosen.arrayBuffer());
    if (text.includes("\0")) throw new Error("binary");
  } catch {
    error = { text: NOT_TEXT };
    draw();
    return;
  }
  file = { name: chosen.name, text };
  textBox().value = text;
  takeText();
}

async function copy(button) {
  const passage = doc?.passages[Number(button.dataset.copyPassage)];
  if (!passage) return;
  try {
    await navigator.clipboard.writeText(passage.text);
    button.textContent = "Copied";
    say("Copied.");
  } catch {
    button.textContent = "Couldn’t copy. Select the text instead.";
  }
  setTimeout(() => { if (button.isConnected) button.textContent = "Copy"; }, 2000);
}

function onClick(event) {
  const t = event.target;
  if (t.closest("[data-open-sheet='search-text']")) setTimeout(() => textBox()?.focus(), 0);
  else if (t.closest("[data-pick-text]")) $("[data-text-file]").click();
  else if (t.closest("[data-clear-text]")) {
    textBox().value = "";
    file = null;
    takeText();
    textBox().focus();
  } else if (t.closest("[data-cancel]")) {
    stop();
    pending = null;
    draw();
  } else if (t.closest("[data-try-again]")) {
    if (error?.reload) location.reload();
    else error?.retry?.();
  } else if (t.closest("[data-more]")) {
    const from = results.shown;
    results.shown = results.hits.length;
    drawResults();
    $("[data-result-list]").children[from]?.focus();
  } else if (t.closest("[data-copy-passage]")) copy(t.closest("[data-copy-passage]"));
}

function onChange(event) {
  const t = event.target;
  if (t.matches("[data-text-file]")) {
    takeFile(t.files?.[0]);
    t.value = ""; // the same file chosen again is read again
  } else if (t.name === "model" && read && read.model !== model()) {
    // Vectors from two models can't be compared (§35.1): the text is read again with the new one.
    stop();
    read = null;
    results = null;
    error = null;
    modelChanged = true;
    if (lastQuestion) pending = lastQuestion;
    draw();
    readText();
  }
}

// --- the page coming and going ----------------------------------------------------------------

function forget() {
  stop();
  doc = file = read = results = error = pending = null;
  lastQuestion = "";
  modelChanged = false;
}

const wired = new WeakSet();

function init() {
  const found = document.querySelector("[data-search]");
  if (found !== root) forget(); // another page: what this one held is gone
  root = found;
  if (!root || !form() || wired.has(root)) return draw();
  wired.add(root);
  root.addEventListener("click", onClick);
  root.addEventListener("change", onChange);
  $("#search-text").addEventListener("close", takeText);
  form().addEventListener("submit", (event) => {
    event.preventDefault();
    const question = prompt().value.trim();
    if (!question) return;
    modelChanged = false;
    ask(question);
  });
  draw();
}

// Leaving the page forgets the text even if the browser keeps the page to come back to.
addEventListener("pagehide", () => {
  forget();
  if (root) {
    for (const box of [textBox(), prompt()]) if (box) box.value = "";
    draw();
  }
});
addEventListener("pageshow", (event) => { if (event.persisted) init(); });
document.addEventListener("htmx:afterSettle", init);
document.addEventListener("htmx:historyRestore", init);
init();
