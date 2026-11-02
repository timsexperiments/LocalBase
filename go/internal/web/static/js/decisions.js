// Decisions (DESIGN §22.17): ask a question, read the answer in words, then its
// odds. A question and its answers stay in this browser; a run carries them to
// the model, and the page deletes the run once it has the answer. Everything is
// kept in one state object, so boosted navigation and a reload find it again.
import * as runs from "./runs.js";
import { MAX_OPTIONS, MAX_ROWS, STANDING_LINE, TEMPLATES, batchCSV, buildRequest, curlSnippet, jsSnippet, namedOptions, needsReview, problem, readAnswer, situations } from "./decision-read.js";

const KEY = "localbase.decisions.v1";
const SAVED = "localbase.decisions.saved.v1";
const PLACE = { href: "/app/studio/decisions", label: "Decision" };
const TYPE_NAMES = { choice: "Pick one", noul: "Yes or no", score: "Rate" };

const blankForm = () => ({
  type: "choice",
  template: "",
  question: "",
  situation: "",
  batch: false,
  choice: [{ name: "", note: "" }, { name: "", note: "" }],
  score: ["low", "medium", "high"].map((name) => ({ name, note: "" })),
});

const state = { form: blankForm(), collapsed: false, one: null, rows: null, filter: "all" };
let root = null;
let stopCurrent = null; // aborts what is running now
let batchRunning = false;

function restore() {
  try {
    Object.assign(state, JSON.parse(sessionStorage.getItem(KEY) ?? "{}"));
  } catch {
    // Nothing to find again.
  }
}

function persist() {
  try {
    sessionStorage.setItem(KEY, JSON.stringify(state));
  } catch {
    // Storage full or blocked: the page still works until it is reloaded.
  }
}

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") node.className = value;
    else if (key in node && key !== "list") node[key] = value;
    else node.setAttribute(key, value);
  }
  node.append(...children.filter((c) => c !== null && c !== undefined));
  return node;
}

const $ = (selector) => root.querySelector(selector);
const model = () => document.getElementById("decide")?.elements.model?.value ?? state.modelID ?? "";
const say = (text) => {
  const live = document.getElementById("decide-said");
  if (live) live.textContent = text;
};

// --- the form -----------------------------------------------------------------
function renderOptions() {
  const box = $("[data-options]");
  const { type } = state.form;
  box.hidden = type === "noul";
  if (type === "noul") return;
  const word = type === "score" ? "Level" : "Option";
  $("[data-options-title]").textContent = type === "score" ? "Levels, lowest first" : "Options";
  const list = $("[data-option-rows]");
  list.replaceChildren(
    ...state.form[type].map((option, i) =>
      el("li", { class: "decide-option" },
        el("div", { class: "decide-option-fields" },
          el("input", { class: "input", value: option.name, placeholder: type === "score" ? "A level" : "An answer it can give", maxLength: 200, autocomplete: "off", "aria-label": `${word} ${i + 1}`, "data-option": i, "data-part": "name" }),
          type === "choice" ? el("input", { class: "input", value: option.note ?? "", placeholder: "What it means (optional)", maxLength: 500, autocomplete: "off", "aria-label": `${word} ${i + 1}, what it means`, "data-option": i, "data-part": "note" }) : null,
        ),
        el("button", { type: "button", class: "icon-btn", "aria-label": `Remove ${word.toLowerCase()} ${i + 1}`, "data-remove-option": i, innerHTML: `<svg aria-hidden="true" viewBox="0 0 24 24" class="size-5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 6l12 12M18 6 6 18"/></svg>` }),
      ),
    ),
  );
  const add = $("[data-add-option]");
  add.textContent = type === "score" ? "Add a level" : "Add an option";
  add.hidden = state.form[type].length >= MAX_OPTIONS[type];
}

function renderForm() {
  const f = state.form;
  document.getElementById("lab-prompt").value = f.question;
  const type = root.querySelector(`input[name=decide-type][value=${f.type}]`);
  if (type) type.checked = true;
  $("#decide-situation").value = f.situation;
  $("[data-situation-title]").textContent = f.batch ? "Situations, one per line" : "Situation";
  $("#decide-situation").placeholder = f.batch ? "Each line gets its own answer" : "What should it look at? A message, a record, some notes.";
  $("#decide-situation").rows = f.batch ? 6 : 3;
  $("[data-batch-tools]").hidden = !f.batch;
  $("[data-batch-chip]").setAttribute("aria-pressed", String(f.batch));
  $("[data-batch-chip] [data-value]").textContent = f.batch ? "On" : "Off";
  $("[data-template-chip] [data-value]").textContent = allTemplates().find((t) => t.id === f.template)?.name ?? "None";
  renderCount();
  renderOptions();
  renderCollapse();
}

function renderCount() {
  const count = $("[data-situation-count]");
  if (!state.form.batch) return void (count.hidden = true);
  const n = situations(state.form.situation).length;
  count.hidden = false;
  count.textContent = n === 1 ? "1 situation" : `${n} situations`;
  count.classList.toggle("text-danger", n > MAX_ROWS);
}

function renderCollapse() {
  const f = state.form;
  $("[data-form-open]").hidden = state.collapsed;
  $("[data-form-summary]").hidden = !state.collapsed;
  if (!state.collapsed) return;
  const names = f.type === "noul" ? "Yes or no" : namedOptions(f[f.type]).map((o) => o.name).join(", ");
  const rows = f.batch ? situations(f.situation).length : 0;
  $("[data-summary-situation]").textContent = f.batch ? (rows === 1 ? "1 situation" : `${rows} situations`) : f.situation.trim() || "No situation given";
  $("[data-summary-options]").textContent = names;
}

function showProblem(found) {
  const box = $("#decide-error");
  for (const n of root.querySelectorAll("[aria-invalid]")) n.removeAttribute("aria-invalid");
  box.hidden = !found;
  box.textContent = found?.message ?? "";
  if (!found) return;
  state.collapsed = false;
  renderCollapse();
  const target = found.field === "question" ? document.getElementById("lab-prompt")
    : found.field === "situation" ? $("#decide-situation")
    : root.querySelector("[data-option][data-part=name]");
  target?.setAttribute("aria-invalid", "true");
  target?.focus();
}

// --- running ------------------------------------------------------------------
// One question as a run: waits for the end, returns the /v1 body, deletes the run.
async function follow(id, signal, onStatus) {
  let failure = null;
  let end = null;
  for await (const e of runs.events(id, { signal, after: "" })) {
    if (e.event === "status") onStatus?.(e.data?.status);
    if (e.event === "error") failure = e.data?.error?.message ?? null;
    if (e.event === "end") end = e.data;
  }
  if (end?.status === "completed") {
    const body = JSON.parse(await (await runs.result(id)).text());
    runs.dismiss(id).catch(() => {});
    return body;
  }
  runs.dismiss(id).catch(() => {});
  throw new Error(failure ?? end?.error?.message ?? runs.endMessage(end));
}

async function askOnce(body, onRun, onStatus, signal) {
  const run = await runs.create("systemone", body, PLACE, signal);
  onRun?.(run.id);
  signal?.addEventListener("abort", () => runs.cancel(run.id).then(() => runs.dismiss(run.id)).catch(() => {}), { once: true });
  return follow(run.id, signal, onStatus);
}

function collect(json, modelID) {
  const answer = json?.answers?.answer;
  const read = readAnswer(answer, modelID);
  if (!read) throw new Error("The model gave no answer. Try again.");
  return read;
}

async function decideOne() {
  const modelID = model();
  const body = buildRequest(state.form, state.form.situation, modelID);
  const controller = new AbortController();
  stopCurrent = () => controller.abort();
  state.rows = null;
  state.collapsed = true;
  state.one = { status: "waiting", body, model: modelID };
  update();
  try {
    const json = await askOnce(body, (id) => { state.one.runId = id; persist(); }, (status) => setOneStatus(status), controller.signal);
    finishOne(json);
  } catch (error) {
    failOne(error, controller.signal.aborted);
  }
}

// A reload lost the page's connection to the run, not the run.
async function resumeOne() {
  const { runId } = state.one;
  const controller = new AbortController();
  stopCurrent = () => {
    controller.abort();
    runs.cancel(runId).then(() => runs.dismiss(runId)).catch(() => {});
  };
  syncBusy();
  try {
    finishOne(await follow(runId, controller.signal, (status) => setOneStatus(status)));
  } catch (error) {
    failOne(error, controller.signal.aborted);
  }
}

function setOneStatus(status) {
  if (!state.one || status !== "in_progress") return;
  state.one.status = "deciding";
  update();
}

function finishOne(json) {
  stopCurrent = null;
  if (!state.one) return;
  const read = collect(json, state.one.model);
  state.one = { ...state.one, status: "done", json, read, runId: "" };
  update();
  say(`${read.word}: ${read.verdict}, ${read.odds}.`);
}

function failOne(error, stopped) {
  stopCurrent = null;
  if (stopped) {
    state.one = null;
    state.collapsed = false;
    say("Stopped.");
  } else {
    state.one = { ...state.one, status: "failed", error: error.message, runId: "" };
    say(error.message);
  }
  update();
}

async function runBatch() {
  if (batchRunning) return;
  batchRunning = true;
  // The rows belong to the form as it was sent: editing it starts the next batch.
  const modelID = state.rowsModel ?? model();
  const form = state.rowsForm ?? state.form;
  const rows = state.rows;
  const total = rows.length;
  let spoken = 0;
  let stopped = false;
  renderBatch();
  for (const row of rows) {
    if (row.status !== "waiting") continue;
    const controller = new AbortController();
    stopCurrent = () => { stopped = true; controller.abort(); };
    row.status = "running";
    renderRow(row);
    renderBatchHead();
    try {
      const json = await askOnce(buildRequest(form, row.situation, modelID), null, null, controller.signal);
      row.read = collect(json, modelID);
      row.status = "done";
    } catch (error) {
      row.status = stopped ? "waiting" : "failed";
      row.error = stopped ? "" : error.message;
    }
    renderRow(row);
    renderBatchHead();
    persist();
    const done = rows.filter((r) => r.status === "done" || r.status === "failed").length;
    if (done - spoken >= Math.max(1, Math.round(total / 10))) {
      spoken = done;
      say(`${done} of ${total} done.`);
    }
    if (stopped) break;
  }
  batchRunning = false;
  stopCurrent = null;
  if (root?.isConnected) renderBatch();
  persist();
  if (state.rows) say(batchSummary());
}

function startBatch() {
  state.one = null;
  state.collapsed = true;
  state.filter = "all";
  state.rowsModel = model();
  state.rowsForm = structuredClone(state.form);
  state.rows = situations(state.form.situation).map((situation, i) => ({ id: i, situation, status: "waiting" }));
  update();
  return runBatch();
}

// --- the answer ---------------------------------------------------------------
function bars(read) {
  return el("ul", { class: "decide-bars" },
    ...read.rows.map((row) => {
      const fill = el("i", {});
      fill.style.width = `${Math.max(row.p * 100, 0.5)}%`;
      return el("li", { class: row.top ? "decide-bar decide-bar-top" : "decide-bar" },
        el("span", { class: "decide-bar-label" }, el("span", { class: "min-w-0 break-words" }, row.label), el("span", { class: "shrink-0" }, row.text)),
        el("span", { class: "decide-track", "aria-hidden": "true" }, fill),
      );
    }),
  );
}

function renderOne() {
  const box = $("[data-result]");
  const one = state.one;
  box.hidden = !one;
  if (!one) return box.replaceChildren();
  if (one.status === "waiting" || one.status === "deciding") {
    box.replaceChildren(
      el("div", { class: "decide-card", "aria-busy": "true" },
        el("div", { class: "flex items-center justify-between gap-2" },
          el("p", { class: "font-bold" }, one.status === "waiting" ? "Waiting to start…" : "Deciding…"),
          el("button", { type: "button", class: "btn btn-quiet", "data-stop": "" }, "Stop"),
        ),
        el("div", { class: "mt-3 grid gap-3", "aria-hidden": "true" }, ...[0, 1, 2].map(() => el("span", { class: "skeleton block h-9 rounded-lg" }))),
      ),
    );
    return;
  }
  if (one.status === "failed") {
    box.replaceChildren(
      el("div", { class: "decide-card" },
        el("p", { role: "alert", class: "font-semibold" }, one.error),
        el("button", { type: "submit", form: "decide", class: "btn btn-quiet mt-3" }, "Try again"),
      ),
    );
    return;
  }
  const read = one.read;
  box.replaceChildren(
    el("article", { class: "decide-card" },
      el("p", { class: `decide-word decide-word-${read.band}` }, read.word),
      el("p", { class: "decide-verdict" }, el("span", { class: "min-w-0 break-words" }, read.verdict), " ", el("span", { class: "decide-odds" }, read.odds)),
      bars(read),
      el("p", { class: "mt-3 text-sm text-muted" }, STANDING_LINE),
      howToRead(),
      el("div", { class: "lab-item-foot mt-2" },
        el("button", { type: "button", class: "btn btn-quiet", "data-copy-json": "" }, "Copy JSON"),
        el("button", { type: "button", class: "btn btn-quiet", "data-code": "", "aria-haspopup": "dialog" }, "View code"),
        el("button", { type: "button", class: "btn btn-quiet", "data-save": "" }, "Save example"),
      ),
    ),
  );
}

function howToRead() {
  const line = (word, text) => el("li", {}, el("b", {}, word), `: ${text}`);
  return el("details", { class: "mt-2 text-sm text-muted" },
    el("summary", { class: "decide-summary" }, "How to read this"),
    el("ul", { class: "mt-1 grid gap-1 pb-1" },
      line("Strongly favored", "one answer is far ahead of the next."),
      line("Leaning", "one answer is ahead, with real room for another."),
      line("Close call", "the top two are near each other."),
      line("Unsure", "no answer reaches half."),
    ),
  );
}

// --- a batch ------------------------------------------------------------------
function batchSummary() {
  const rows = state.rows ?? [];
  const done = rows.filter((r) => r.status === "done").length;
  const review = rows.filter((r) => needsReview(r.read)).length;
  const failed = rows.filter((r) => r.status === "failed").length;
  const parts = [`${done} of ${rows.length} done`];
  if (review) parts.push(`${review} to review`);
  if (failed) parts.push(`${failed} failed`);
  return parts.join(", ");
}

const STATUS_WORDS = { waiting: "Waiting", running: "Deciding…", failed: "Failed" };

function rowShown(row) {
  if (state.filter === "review") return needsReview(row.read);
  if (state.filter === "failed") return row.status === "failed";
  return true;
}

function rowNode(row) {
  const answer = row.read
    ? el("details", {}, el("summary", { class: "decide-summary font-semibold" }, `${row.read.verdict} · ${row.read.odds}`), bars(row.read))
    : row.status === "failed"
      ? el("span", { class: "text-danger" }, row.error || STATUS_WORDS.failed)
      : el("span", { class: "text-muted" }, STATUS_WORDS[row.status] ?? "");
  const tr = el("div", { role: "row", class: "decide-row", id: `decide-row-${row.id}` },
    el("div", { role: "cell", class: "decide-cell-text" }, row.situation),
    el("div", { role: "cell" }, answer),
    el("div", { role: "cell" }, row.read ? el("span", { class: `decide-word decide-word-${row.read.band}` }, row.read.word) : row.status === "failed" ? el("span", { class: "decide-word" }, STATUS_WORDS.failed) : null),
  );
  tr.hidden = !rowShown(row);
  return tr;
}

function renderRow(row) {
  document.getElementById(`decide-row-${row.id}`)?.replaceWith(rowNode(row));
}

function renderBatchHead() {
  syncBusy();
  const head = $("[data-batch-summary]");
  if (head) head.textContent = batchSummary();
  const rows = state.rows ?? [];
  const waiting = rows.some((r) => r.status === "waiting");
  const failed = rows.some((r) => r.status === "failed");
  const set = (selector, shown) => { const n = $(selector); if (n) n.hidden = !shown; };
  set("[data-batch-stop]", batchRunning);
  set("[data-batch-continue]", !batchRunning && waiting);
  set("[data-batch-retry]", !batchRunning && failed);
  set("[data-batch-export]", !batchRunning && rows.some((r) => r.read || r.error));
}

function renderBatch() {
  const box = $("[data-batch]");
  box.hidden = !state.rows;
  if (!state.rows) return box.replaceChildren();
  const filter = (value, text) =>
    el("label", { class: "decide-filter" }, el("input", { type: "radio", name: "decide-filter", value, checked: state.filter === value, class: "sr-only" }), text);
  box.replaceChildren(
    el("div", { class: "decide-card" },
      el("div", { class: "flex flex-wrap items-center justify-between gap-2" },
        el("p", { class: "font-bold", "data-batch-summary": "" }),
        el("div", { class: "flex flex-wrap gap-1" },
          el("button", { type: "button", class: "btn btn-quiet", "data-batch-stop": "", hidden: true }, "Stop"),
          el("button", { type: "button", class: "btn btn-quiet", "data-batch-continue": "", hidden: true }, "Continue"),
          el("button", { type: "button", class: "btn btn-quiet", "data-batch-retry": "", hidden: true }, "Retry failed"),
          el("button", { type: "button", class: "btn btn-quiet", "data-batch-export": "", hidden: true }, "Export CSV"),
        ),
      ),
      el("fieldset", { class: "mt-2 flex flex-wrap gap-1.5" }, el("legend", { class: "sr-only" }, "Show"), filter("all", "All"), filter("review", "Close calls and unsure"), filter("failed", "Failed")),
      el("div", { role: "table", "aria-label": "Answers", class: "decide-table" },
        el("div", { role: "row", class: "decide-row decide-row-head" }, ...["Situation", "Answer", "Read"].map((t) => el("div", { role: "columnheader" }, t))),
        ...state.rows.map(rowNode),
      ),
      el("p", { class: "mt-3 text-sm text-muted" }, STANDING_LINE),
    ),
  );
  renderBatchHead();
}

// --- templates and saved examples -----------------------------------------------
function savedExamples() {
  try {
    return JSON.parse(localStorage.getItem(SAVED) ?? "[]");
  } catch {
    return [];
  }
}

function writeSaved(list) {
  try {
    localStorage.setItem(SAVED, JSON.stringify(list));
    return true;
  } catch {
    return false;
  }
}

const allTemplates = () => [...TEMPLATES, ...savedExamples()];

function fillFrom(template) {
  stopCurrent?.(); // a new question replaces one still running
  const form = blankForm();
  form.type = template.type;
  form.template = template.id ?? "";
  form.question = template.question ?? "";
  form.situation = template.situation ?? "";
  form.batch = !!template.batch;
  if (template.options?.length) form[template.type] = template.options.map((o) => ({ name: o.name, note: o.note ?? "" }));
  state.form = form;
  state.collapsed = false;
  state.one = null;
  state.rows = null;
  update();
  renderSheet();
}

function renderSheet() {
  const sheet = document.getElementById("decide-template");
  if (!sheet) return;
  const row = (t, extra) =>
    el("li", { class: "flex items-center" },
      el("button", { type: "button", class: "list-row min-w-0 flex-1 text-left", "data-template": t.id },
        el("span", { class: "min-w-0 flex-1" }, el("span", { class: "block truncate" }, t.name), el("span", { class: "block truncate text-sm text-muted" }, TYPE_NAMES[t.type])),
      ),
      extra,
    );
  sheet.querySelector("[data-templates]").replaceChildren(row({ id: "", name: "Start blank", type: "choice" }), ...TEMPLATES.map((t) => row(t)));
  const saved = savedExamples();
  sheet.querySelector("[data-saved-box]").hidden = !saved.length;
  sheet.querySelector("[data-saved]").replaceChildren(
    ...saved.map((t) => row(t, el("button", { type: "button", class: "btn btn-quiet shrink-0", "data-unsave": t.id, "aria-label": `Delete ${t.name}` }, "Delete"))),
  );
}

function saveExample(button) {
  const f = state.form;
  const name = f.question.trim().slice(0, 60) || "Example";
  const entry = { id: `saved-${Date.now()}`, name, type: f.type, question: f.question, situation: f.situation, batch: f.batch, options: f.type === "noul" ? [] : f[f.type] };
  const list = [entry, ...savedExamples()].slice(0, 50);
  const ok = writeSaved(list);
  state.form.template = ok ? entry.id : state.form.template;
  button.textContent = ok ? "Saved in this browser" : "Could not save";
  say(button.textContent);
  renderForm();
  renderSheet();
}

// --- code and copying -------------------------------------------------------------
function currentBody() {
  const first = state.form.batch ? (situations(state.form.situation)[0] ?? "") : state.form.situation;
  return buildRequest(state.form, first, model());
}

function renderCode() {
  const sheet = document.getElementById("decide-code");
  const js = sheet.querySelector("input[name=decide-code-lang]:checked")?.value === "js";
  const body = currentBody();
  sheet.querySelector("[data-code-text]").textContent = js ? jsSnippet(body, location.origin) : curlSnippet(body, location.origin);
  sheet.querySelector("[data-code-note]").hidden = !state.form.batch;
}

async function copy(text, button) {
  const was = button.textContent;
  try {
    await navigator.clipboard.writeText(text);
    button.textContent = "Copied";
  } catch {
    button.textContent = "Could not copy";
  }
  say(button.textContent);
  setTimeout(() => { button.textContent = was; }, 1500);
}

function download(text) {
  const url = URL.createObjectURL(new Blob([text], { type: "text/csv" }));
  el("a", { href: url, download: "decisions.csv" }).click();
  setTimeout(() => URL.revokeObjectURL(url), 5000);
}

// --- wiring -----------------------------------------------------------------------
// One thing at a time: Stop is on screen while it runs.
function syncBusy() {
  const send = document.querySelector("#decide .lab-dock button[type=submit]");
  if (send) send.disabled = !!stopCurrent;
}

function update() {
  if (root?.isConnected) {
    syncBusy();
    renderForm();
    renderOne();
    renderBatch();
  }
  persist();
}

function submit() {
  syncFromFields();
  const found = problem(state.form);
  showProblem(found);
  if (found || stopCurrent) return;
  if (state.form.batch) startBatch();
  else decideOne();
}

function syncFromFields() {
  state.form.question = document.getElementById("lab-prompt").value;
  state.form.situation = $("#decide-situation").value;
}

function onInput(event) {
  const t = event.target;
  if (t.id === "lab-prompt" || t.id === "decide-situation") {
    syncFromFields();
    renderCount();
    persist();
  }
  if (t.dataset.option !== undefined) {
    state.form[state.form.type][Number(t.dataset.option)][t.dataset.part] = t.value;
    persist();
  }
}

function onChange(event) {
  const t = event.target;
  if (t.name === "decide-type") {
    syncFromFields();
    state.form.type = t.value;
    renderOptions();
    persist();
  } else if (t.name === "decide-filter") {
    state.filter = t.value;
    for (const row of state.rows ?? []) {
      const node = document.getElementById(`decide-row-${row.id}`);
      if (node) node.hidden = !rowShown(row);
    }
    persist();
  } else if (t.name === "decide-code-lang") {
    renderCode();
  } else if (t.matches("[data-batch-file]") && t.files[0]) {
    t.files[0].text().then((text) => {
      state.form.situation = text;
      $("#decide-situation").value = situations(text).join("\n");
      syncFromFields();
      renderCount();
      persist();
      t.value = "";
    });
  }
}

function onClick(event) {
  const t = event.target.closest?.("button, [data-template]");
  if (!t) return;
  const type = state.form.type;
  if (t.matches("[data-add-option]")) {
    state.form[type].push({ name: "", note: "" });
    renderOptions();
    persist();
    root.querySelector(`[data-option="${state.form[type].length - 1}"][data-part=name]`)?.focus();
  } else if (t.matches("[data-remove-option]")) {
    state.form[type].splice(Number(t.dataset.removeOption), 1);
    renderOptions();
    persist();
    $("[data-add-option]").focus();
  } else if (t.matches("[data-batch-chip]")) {
    syncFromFields();
    state.form.batch = !state.form.batch;
    state.collapsed = false;
    update();
  } else if (t.matches("[data-edit]")) {
    state.collapsed = false;
    renderCollapse();
    persist();
    $("#decide-situation").focus();
  } else if (t.matches("[data-stop]") || t.matches("[data-batch-stop]")) {
    stopCurrent?.();
  } else if (t.matches("[data-batch-continue]")) {
    runBatch();
  } else if (t.matches("[data-batch-retry]")) {
    for (const row of state.rows) if (row.status === "failed") { row.status = "waiting"; row.error = ""; renderRow(row); }
    runBatch();
  } else if (t.matches("[data-batch-export]")) {
    download(batchCSV(state.rows));
  } else if (t.matches("[data-copy-json]")) {
    copy(JSON.stringify(state.one?.json?.answers?.answer ?? {}, null, 2), t);
  } else if (t.matches("[data-save]")) {
    saveExample(t);
  } else if (t.matches("[data-code]")) {
    renderCode();
    document.getElementById("decide-code").showModal();
  } else if (t.matches("[data-copy-code]")) {
    copy(document.querySelector("#decide-code [data-code-text]").textContent, t);
  } else if (t.matches("[data-pick-file]")) {
    $("[data-batch-file]").click();
  } else if (t.matches("[data-template]")) {
    const found = allTemplates().find((x) => x.id === t.dataset.template);
    fillFrom(found ?? { type: "choice" });
    document.getElementById("decide-template").close();
    document.getElementById("lab-prompt").focus();
  } else if (t.matches("[data-unsave]")) {
    writeSaved(savedExamples().filter((x) => x.id !== t.dataset.unsave));
    renderSheet();
  }
}

const wired = new WeakSet();

function init() {
  const found = document.querySelector("[data-decisions]");
  if (!found) {
    root = null;
    if (document.body.dataset.runPage === PLACE.href) delete document.body.dataset.runPage;
    return;
  }
  root = found;
  document.body.dataset.runPage = PLACE.href;
  if (!document.getElementById("decide")) return; // no model, or not allowed
  state.modelID = model();
  if (!wired.has(root)) {
    wired.add(root);
    root.addEventListener("input", onInput);
    root.addEventListener("change", onChange);
    root.addEventListener("click", onClick);
    document.getElementById("decide").addEventListener("submit", (event) => {
      event.preventDefault();
      submit();
    });
  }
  renderSheet();
  update();
}

restore();
// A page that went away mid-batch left its row "running": that row runs again.
for (const row of state.rows ?? []) if (row.status === "running") row.status = "waiting";
const pendingRun = state.one?.runId && (state.one.status === "waiting" || state.one.status === "deciding");
if (state.one && !pendingRun && state.one.status !== "done" && state.one.status !== "failed") state.one = null;

document.addEventListener("htmx:afterSettle", init);
document.addEventListener("htmx:historyRestore", init);
init();
if (pendingRun) resumeOne();
