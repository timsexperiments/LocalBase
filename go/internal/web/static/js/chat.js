// Chat page (DESIGN §15 step 1, .design/research/chat/BRIEF.md). Replies
// stream from /app/api/v1/chat/completions as the signed-in person.
// Conversations live in this browser only: in memory, and in localStorage
// when "Save text history on this device" is on (the TS UI's key and
// schema, so history carries over). Pure logic is in chat-core.js and
// markdown.js.
import {
  chatContext, compact, contextWindow, interruptReply, learnContext, overflowContext, phaseLabel, planCompaction,
  promptBudget, readHistory, steerAbortReason, streamText, titleFrom, visibleMessages, writeHistory,
} from "./chat-core.js";
import { build, parse } from "./markdown.js";

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const models = JSON.parse($("#chat-models").textContent || "[]");
const ready = models.filter((m) => m.ready);
const log = $("#chat-log");
const list = $("[data-messages]");
const empty = $("[data-empty]");
const input = $("#chat-input");
const form = $("#composer");
const sendButton = $("[data-send]");
const stopButton = $("[data-stop]");
const jump = $("[data-jump]");
const status = $("#chat-status");
const errorBox = $("[data-error-box]");
const drawer = $("#chat-drawer");
const picker = $("#model-picker");
const canChat = !input.disabled;

const uuid = () => crypto.randomUUID();
const fresh = () => ({ id: uuid(), title: "New conversation", workspace: "chat", mode: "llm", model: (ready.find((m) => m.default) ?? ready[0])?.id ?? "", messages: [] });

// --- state ----------------------------------------------------------------------

let conversations = [];
let persistent = false;
let historyError = "";
try {
  const saved = readHistory(localStorage);
  persistent = saved !== null;
  conversations = saved ?? [];
} catch {
  historyError = "Saved history could not be read. You can clear it in Settings.";
}
let active;
let abort = null; // the running reply or compaction
let running = null; // {conversation, answer} while a reply streams
let compactingSince = 0; // "Compact now" in progress
let follow = true;
// Learned runtime context sizes (TS key); memory only when storage is blocked.
const storage = (() => {
  try {
    localStorage.getItem("");
    return localStorage;
  } catch {
    const items = new Map();
    return { getItem: (k) => items.get(k) ?? null, setItem: (k, v) => items.set(k, String(v)) };
  }
})();
// Prompt overhead the request adds beyond the messages (tool schemas, later).
const overhead = 0;

const chatsOnly = () => conversations.filter((c) => c.workspace === "chat" && c.mode === "llm");

function select(id) {
  active = conversations.find((c) => c.id === id);
  if (!active) {
    // Reuse an empty conversation rather than adding one per visit (TS).
    active = chatsOnly().find((c) => !c.messages.length) ?? fresh();
    if (!conversations.includes(active)) conversations.unshift(active);
  }
  const url = new URL(location.href);
  url.searchParams.set("conversation", active.id);
  history.replaceState(null, "", url);
}

function save() {
  if (!persistent) return;
  try {
    // Empty conversations are not worth keeping.
    writeHistory(localStorage, conversations.filter((c) => c.messages.length || c === active));
  } catch {
    showError("Device storage is unavailable, so this conversation was not saved.");
  }
}

// --- announcements and errors -----------------------------------------------------

let announceTimer;
function announce(text) {
  status.textContent = "";
  clearTimeout(announceTimer);
  announceTimer = setTimeout(() => (status.textContent = text), 100);
}

function showError(message, retry) {
  errorBox.replaceChildren();
  const card = document.createElement("div");
  card.className = "card mb-2 flex flex-wrap items-center justify-between gap-2 border-danger-line bg-danger-soft px-4 py-2";
  const text = document.createElement("p");
  text.className = "min-w-0 flex-1 text-sm font-semibold text-danger";
  text.textContent = message;
  card.append(text);
  if (retry) card.append(button("Retry", "btn btn-quiet text-sm", retry));
  card.append(button("Dismiss", "btn btn-quiet text-sm", () => errorBox.replaceChildren()));
  errorBox.append(card);
  announce(message);
}

// warn adds a notice that doesn't stop the conversation (it stays until
// dismissed or the next send).
function warn(message) {
  const card = document.createElement("div");
  card.className = "card mb-2 flex flex-wrap items-center justify-between gap-2 px-4 py-2";
  const text = document.createElement("p");
  text.className = "min-w-0 flex-1 text-sm";
  text.textContent = message;
  card.append(text, button("Dismiss", "btn btn-quiet text-sm", () => card.remove()));
  errorBox.append(card);
  announce(message);
}

function button(label, className, onClick, attrs = {}) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = className;
  b.textContent = label;
  for (const [k, v] of Object.entries(attrs)) b.setAttribute(k, v);
  b.addEventListener("click", onClick);
  return b;
}

// --- conversation list ------------------------------------------------------------

function renderConversations() {
  for (const menu of $$("[data-conversations]")) {
    const root = menu.closest("nav").parentElement;
    const query = $("[data-search]", root).value.trim().toLowerCase();
    const shown = chatsOnly().filter((c) => c.messages.length || c === active).filter((c) =>
      !query || c.title.toLowerCase().includes(query) || c.messages.some((m) => m.text.toLowerCase().includes(query)));
    menu.replaceChildren(...shown.map((c) => {
      const li = document.createElement("li");
      const a = document.createElement("a");
      a.href = `?conversation=${encodeURIComponent(c.id)}`;
      a.className = "flex min-h-11 items-center rounded-xl px-3 text-sm wrap-anywhere hover:bg-hover";
      a.textContent = c.title || "New conversation";
      if (c === active) {
        a.setAttribute("aria-current", "page");
        a.classList.add("bg-accent-soft", "font-semibold", "text-accent");
      }
      a.addEventListener("click", (e) => {
        e.preventDefault();
        if (abort) return;
        select(c.id);
        render();
        drawer.close();
        input.focus();
      });
      li.append(a);
      return li;
    }));
    const none = $("[data-no-conversations]", root);
    // Without saved history, say so even when the list has this session's chats.
    none.hidden = shown.length > 0 && (persistent || query);
    none.textContent = query ? "No conversations match." : persistent ? "No saved conversations yet." :
      "These conversations aren’t saved on this device. Turn on history in Settings to keep them.";
  }
}

// --- messages -----------------------------------------------------------------------

function codeBlock(lang, text) {
  const wrap = document.createElement("div");
  wrap.className = "md-code";
  const bar = document.createElement("div");
  bar.className = "md-code-bar";
  const label = document.createElement("span");
  label.textContent = lang || "Code";
  bar.append(label, copyButton(() => text, "Copy code"));
  const pre = document.createElement("pre");
  const code = document.createElement("code");
  code.textContent = text;
  pre.append(code);
  pre.tabIndex = 0; // long lines scroll inside the block
  wrap.append(bar, pre);
  return wrap;
}

function copyButton(text, label) {
  const b = button(label, "md-copy", async () => {
    try {
      await navigator.clipboard.writeText(text());
      b.textContent = "Copied";
      announce("Copied");
    } catch {
      b.textContent = "Couldn’t copy";
    }
    setTimeout(() => (b.textContent = label), 2000);
  });
  return b;
}

function heading(text) {
  const h = document.createElement("h2");
  h.className = "sr-only";
  h.textContent = text;
  return h;
}

function messageNode(m) {
  const article = document.createElement("article");
  article.dataset.id = m.id;
  if (m.role === "user") {
    article.className = "flex flex-col items-end";
    const bubble = document.createElement("div");
    bubble.className = "max-w-[85%] whitespace-pre-wrap wrap-anywhere rounded-2xl rounded-br-md bg-accent-soft px-4 py-2.5";
    bubble.textContent = m.text;
    article.append(heading("You"), bubble);
    if (m.attachmentsMissing) {
      const note = document.createElement("p");
      note.className = "mt-1 text-xs text-muted";
      note.textContent = "Attachments aren’t kept in saved history.";
      article.append(note);
    }
    return article;
  }
  article.className = "min-w-0";
  article.append(heading("Reply"));
  if (m.reasoning) {
    const details = document.createElement("details");
    details.className = "reasoning";
    const summary = document.createElement("summary");
    summary.textContent = "Reasoning";
    const body = document.createElement("div");
    body.className = "whitespace-pre-wrap wrap-anywhere text-sm text-muted";
    body.textContent = m.reasoning;
    details.append(summary, body);
    article.append(details);
  }
  const body = document.createElement("div");
  body.className = "reply";
  body.append(...build(parse(m.text), codeBlock));
  article.append(body);
  if (m.pending) {
    const working = document.createElement("p");
    working.className = "working text-sm text-muted";
    working.dataset.working = "";
    working.textContent = m.pendingText ?? "Waiting for a reply…";
    article.append(working);
  } else {
    const actions = document.createElement("div");
    actions.className = "mt-1 flex flex-wrap items-center gap-2";
    if (m.interrupted) {
      const badge = document.createElement("span");
      badge.className = "badge";
      badge.textContent = m.text ? "Interrupted" : "Interrupted before a response";
      actions.append(badge);
    }
    if (m.text) actions.append(copyButton(() => m.text, "Copy"));
    if (!m.text && !m.interrupted && !m.reasoning) {
      const none = document.createElement("span");
      none.className = "text-sm text-muted";
      none.textContent = "No response received.";
      actions.append(none);
    }
    // Folding older turns by hand, offered on the last reply once there are
    // at least two turns still sent verbatim (TS).
    if (!abort && m === active.messages.at(-1) &&
      visibleMessages(active.messages, active.compaction).filter(({ role }) => role === "user").length >= 2)
      actions.append(button("Compact now", "md-copy", compactNow));
    article.append(actions);
  }
  return article;
}

// dividerNode marks where the compacted prefix ends.
function dividerNode(c) {
  const count = `${c.count} earlier message${c.count === 1 ? "" : "s"}`;
  const details = document.createElement("details");
  details.className = "compaction";
  const summary = document.createElement("summary");
  summary.textContent = c.method === "summary" ? `Earlier messages compacted · ${count}` : `Earlier messages dropped from context · ${count}`;
  const body = document.createElement("div");
  body.className = "grid gap-2 pt-2 text-sm";
  const note = (text) => {
    const p = document.createElement("p");
    p.className = "text-muted";
    p.textContent = text;
    return p;
  };
  if (c.method === "dropped") body.append(note("Summarization failed, so these messages are no longer sent to the model. They are still shown above."));
  if (c.summary) {
    const p = document.createElement("p");
    p.className = "whitespace-pre-wrap wrap-anywhere";
    p.textContent = c.summary;
    body.append(p);
  }
  if (c.method === "summary") body.append(note("The model sees this summary instead of the messages above."));
  details.append(summary, body);
  return details;
}

function compactingNode() {
  const p = document.createElement("p");
  p.className = "working text-sm text-muted";
  p.dataset.compacting = "";
  p.textContent = phaseLabel("compacting", (Date.now() - compactingSince) / 1000);
  return p;
}

function render() {
  $("#chat-title").textContent = active.title;
  document.title = `${active.messages.length ? active.title : "Chat"} · LocalBase`;
  renderModel();
  const nodes = [];
  for (const m of active.messages) {
    nodes.push(messageNode(m));
    if (active.compaction?.through === m.id) nodes.push(dividerNode(active.compaction));
  }
  if (compactingSince) nodes.push(compactingNode());
  list.replaceChildren(...nodes);
  empty.hidden = active.messages.length > 0;
  renderConversations();
  scrollToEnd();
}

function rerenderMessage(m) {
  const old = list.querySelector(`article[data-id="${CSS.escape(m.id)}"]`);
  const next = messageNode(m);
  if (old) old.replaceWith(next);
  else list.append(next);
  empty.hidden = true;
  if (follow) scrollToEnd();
  else jump.hidden = false;
}

// --- scrolling ----------------------------------------------------------------------

const atEnd = () => log.scrollTop + log.clientHeight >= log.scrollHeight - 48;
const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)");
function scrollToEnd(smooth = false) {
  log.scrollTo({ top: log.scrollHeight, behavior: smooth && !reduceMotion.matches ? "smooth" : "auto" });
  jump.hidden = true;
}
log.addEventListener("scroll", () => {
  follow = atEnd();
  if (follow) jump.hidden = true;
});
jump.addEventListener("click", () => {
  follow = true;
  scrollToEnd(true);
  log.focus({ preventScroll: true });
});

// --- model --------------------------------------------------------------------------

function renderModel() {
  const m = models.find((x) => x.id === active.model);
  const name = m ? m.name : active.model ? "Model not available" : "Choose a model";
  $("[data-model-name]").textContent = m && !m.ready ? `${name} (not installed)` : name;
  $("[data-open-picker]").setAttribute("aria-label", `Choose model: ${m ? m.name : "none selected"}`);
}

function renderPicker() {
  const body = $("[data-picker-list]");
  const row = (m) => {
    const label = document.createElement("label");
    label.className = "flex min-h-11 cursor-pointer items-start gap-3 rounded-xl border border-line p-3 has-[:checked]:border-accent has-[:checked]:bg-accent-soft has-[:disabled]:cursor-default has-[:disabled]:opacity-70";
    const radio = document.createElement("input");
    radio.type = "radio";
    radio.name = "model";
    radio.value = m.id;
    radio.className = "mt-1 accent-[var(--lb-accent)]";
    radio.checked = m.id === active.model;
    radio.disabled = !m.ready;
    radio.addEventListener("change", () => {
      active.model = m.id;
      save();
      renderModel();
      picker.close();
    });
    const text = document.createElement("span");
    text.className = "grid min-w-0";
    const name = document.createElement("span");
    name.className = "font-semibold break-words";
    name.textContent = m.name;
    const facts = document.createElement("span");
    facts.className = "text-sm text-muted";
    facts.textContent = [m.ready ? "Ready" : "Not installed", m.default ? "Default" : "", m.context ? `${m.context.toLocaleString()} tokens of context` : ""]
      .filter(Boolean).join(" · ");
    text.append(name, facts);
    label.append(radio, text);
    return label;
  };
  const parts = [];
  if (!ready.length) {
    const p = document.createElement("p");
    p.className = "text-muted";
    p.textContent = "No chat model is ready. Install and enable one in the Library.";
    parts.push(p);
  }
  parts.push(...ready.map(row));
  const unavailable = models.filter((m) => !m.ready);
  if (unavailable.length) {
    const h = document.createElement("h3");
    h.className = "mt-2 text-sm font-semibold text-muted";
    h.textContent = "Not installed";
    parts.push(h, ...unavailable.map(row));
  }
  body.replaceChildren(...parts);
}

// --- sending --------------------------------------------------------------------

// While a reply streams, Stop shows; typing turns Send into "Send now", which
// interrupts the reply (keeping its partial text) and starts the next turn.
function updateSend() {
  const busy = !!abort;
  const steerable = busy && running?.conversation === active && !!input.value.trim();
  stopButton.hidden = !busy;
  sendButton.hidden = busy && !steerable;
  const label = busy ? "Send now (interrupts reply)" : "Send";
  sendButton.setAttribute("aria-label", label);
  sendButton.title = busy ? label : "";
}

function setBusy() {
  updateSend();
  for (const b of $$("[data-new-chat]")) b.disabled = !!abort;
}

async function apiError(response) {
  if (response.status === 401) return "Your session ended. Reload the page to sign in again.";
  try {
    const body = await response.json();
    if (body?.error?.message) return body.error.message;
  } catch {}
  return `The gateway answered ${response.status}.`;
}

// post sends a chat-completions request as the signed-in person.
async function post(body, signal) {
  const response = await fetch("/app/api/v1/chat/completions", {
    method: "POST",
    signal,
    credentials: "same-origin",
    cache: "no-store",
    headers: { "content-type": "application/json", "X-Localbase-UI": "1" },
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new Error(await apiError(response));
  return response;
}

const dropWarning = (error) => `Couldn’t summarize earlier messages (${error}); they were dropped from the model’s context.`;

function openPicker() {
  renderPicker();
  picker.showModal();
}

function readyModel(conversation) {
  const model = ready.find((m) => m.id === conversation.model);
  if (!model) {
    showError("This conversation’s model isn’t ready. Choose a ready model to continue.");
    openPicker();
  }
  return model;
}

// send starts a reply, or explains why it cannot and returns false (the
// caller keeps the draft). Sending while this conversation's reply streams
// steers: that reply is marked interrupted and stopped.
function send(text) {
  const steering = !!abort && running?.conversation === active;
  if (abort && !steering) return false;
  errorBox.replaceChildren();
  const model = readyModel(active);
  if (!model) return false;
  const conversation = active;
  if (steering) {
    const old = running.answer;
    Object.assign(old, interruptReply(old, "Interrupted"));
    delete old.protocol;
    delete old.pending;
    delete old.pendingText;
    abort.abort(steerAbortReason);
  }
  const user = { id: uuid(), role: "user", text };
  if (!conversation.messages.length) conversation.title = titleFrom(text);
  conversation.messages.push(user);
  reply(conversation, model);
  return true;
}

async function reply(conversation, model) {
  const history = [...conversation.messages];
  const answer = { id: uuid(), role: "assistant", text: "", pending: true };
  conversation.messages.push(answer);
  const controller = new AbortController();
  abort = controller;
  running = { conversation, answer };
  let phase = { name: "waiting", since: Date.now() };
  const paintPhase = () => {
    answer.pendingText = phaseLabel(phase.name, (Date.now() - phase.since) / 1000);
    const w = list.querySelector(`article[data-id="${CSS.escape(answer.id)}"] [data-working]`);
    if (w) w.textContent = answer.pendingText;
  };
  const setPhase = (name) => {
    phase = { name, since: Date.now() };
    paintPhase();
    if (name === "compacting") announce(answer.pendingText);
  };
  paintPhase();
  render();
  follow = true;
  scrollToEnd();
  setBusy();
  announce("Reply started");
  const tick = setInterval(paintPhase, 1000);
  let frame = 0;
  const update = () => {
    if (!frame) frame = requestAnimationFrame(() => {
      frame = 0;
      rerenderMessage(answer);
    });
  };
  // Fold older turns into a summary when the prompt would not fit; returns
  // whether anything was folded.
  let compaction = conversation.compaction;
  const compactHistory = async (context, force) => {
    const plan = planCompaction({ messages: history, compaction, budget: promptBudget(context), overhead, force });
    if (!plan) return false;
    setPhase("compacting");
    const result = await compact({ model, messages: history, compaction, ...plan, context, signal: controller.signal, post });
    compaction = conversation.compaction = result.compaction;
    save();
    if (conversation === active) render();
    if (result.error) warn(dropWarning(result.error));
    setPhase("waiting");
    return true;
  };
  let outcome = "Reply finished";
  try {
    await compactHistory(contextWindow(storage, model), false);
    let streamed = false;
    const chat = async () => streamText(
      await post({ model: model.id, stream: true, messages: chatContext(history, compaction) }, controller.signal),
      (t) => {
        streamed = true;
        answer.text += t;
        update();
      },
      (t) => {
        streamed = true;
        if (phase.name !== "thinking") setPhase("thinking");
        answer.reasoning = (answer.reasoning ?? "") + t;
        update();
      },
    );
    try {
      await chat();
    } catch (e) {
      // An overflow reports the real runtime context: learn it, compact to
      // fit, and retry once if nothing was streamed yet.
      const size = e instanceof Error && !streamed && !controller.signal.aborted ? overflowContext(e.message) : null;
      if (!size) throw e;
      learnContext(storage, model.id, size);
      if (!(await compactHistory(size, true))) throw e;
      await chat();
    }
  } catch (e) {
    if (controller.signal.aborted) {
      const steered = controller.signal.reason === steerAbortReason;
      Object.assign(answer, interruptReply(answer, steered ? "Interrupted" : "Stopped"));
      delete answer.protocol;
      outcome = steered ? "" : "Stopped";
    } else {
      outcome = "Reply failed";
      const message = e instanceof TypeError ? "LocalBase couldn’t be reached. Check the connection and retry." : e.message;
      if (!answer.text && !answer.reasoning) conversation.messages.splice(conversation.messages.indexOf(answer), 1);
      else answer.interrupted = true;
      queueMicrotask(() => showError(message, () => retry(conversation)));
    }
  } finally {
    clearInterval(tick);
    if (frame) cancelAnimationFrame(frame);
    frame = 0;
    delete answer.pending;
    delete answer.pendingText;
    // A steering send already owns the controller and busy state.
    if (abort === controller) {
      abort = null;
      running = null;
      setBusy();
    }
    save();
    if (conversation === active) render();
    if (outcome && outcome !== "Reply failed") announce(outcome);
  }
}

function retry(conversation) {
  if (abort || conversation !== active) return;
  errorBox.replaceChildren();
  const last = conversation.messages.at(-1);
  if (last?.role === "assistant" && last.interrupted) conversation.messages.pop();
  const model = readyModel(conversation);
  if (model) reply(conversation, model);
}

// compactNow folds all but the newest turn into a summary on request.
async function compactNow() {
  if (abort) return;
  const conversation = active;
  const model = readyModel(conversation);
  if (!model) return;
  const context = contextWindow(storage, model);
  let plan;
  try {
    plan = planCompaction({ messages: conversation.messages, compaction: conversation.compaction, budget: promptBudget(context), overhead, force: true });
  } catch (e) {
    showError(e.message);
    return;
  }
  if (!plan) return;
  const controller = new AbortController();
  abort = controller;
  running = null;
  compactingSince = Date.now();
  errorBox.replaceChildren();
  setBusy();
  render();
  announce(phaseLabel("compacting", 0));
  const tick = setInterval(() => {
    const node = $("[data-compacting]", list);
    if (node) node.textContent = phaseLabel("compacting", (Date.now() - compactingSince) / 1000);
  }, 1000);
  try {
    const result = await compact({ model, messages: conversation.messages, compaction: conversation.compaction, ...plan, context, signal: controller.signal, post });
    conversation.compaction = result.compaction;
    if (result.error) warn(dropWarning(result.error));
    else announce("Earlier messages compacted");
  } catch (e) {
    if (!controller.signal.aborted) showError(e instanceof Error ? e.message : "Compaction failed.");
    else announce("Stopped");
  } finally {
    clearInterval(tick);
    compactingSince = 0;
    if (abort === controller) abort = null;
    setBusy();
    save();
    if (conversation === active) render();
  }
}

form.addEventListener("submit", (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text || !canChat) return;
  if (send(text)) {
    input.value = "";
    grow();
    updateSend();
    input.focus();
  }
});

stopButton.addEventListener("click", () => {
  abort?.abort();
  input.focus();
});

// Enter sends where there is a keyboard; on touch screens it adds a line
// and the button sends (an assumption for the phone review to confirm).
const preciseKeyboard = matchMedia("(pointer: fine)");
input.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing && preciseKeyboard.matches) {
    e.preventDefault();
    form.requestSubmit();
  }
});
function grow() {
  input.style.height = "auto";
  input.style.height = `${input.scrollHeight}px`;
}
input.addEventListener("input", () => {
  grow();
  if (abort) updateSend();
});

// --- drawer, picker, new conversation ------------------------------------------------

$("[data-open-drawer]").addEventListener("click", () => {
  renderConversations();
  drawer.showModal();
});
$("[data-close-drawer]").addEventListener("click", () => drawer.close());
drawer.addEventListener("click", (e) => {
  if (e.target === drawer) drawer.close(); // the backdrop
});
$("[data-open-picker]").addEventListener("click", openPicker);
$("[data-close-picker]").addEventListener("click", () => picker.close());
picker.addEventListener("click", (e) => {
  if (e.target === picker) picker.close();
});
for (const b of $$("[data-new-chat]"))
  b.addEventListener("click", () => {
    if (abort) return;
    select("");
    errorBox.replaceChildren();
    render();
    if (drawer.open) drawer.close();
    input.focus();
  });
for (const s of $$("[data-search]")) s.addEventListener("input", renderConversations);
window.addEventListener("storage", (e) => {
  // Another tab changed saved history: pick it up when idle.
  if (e.key !== "localbase.playground.history.v1" || abort) return;
  try {
    const saved = readHistory(localStorage);
    persistent = saved !== null;
    if (saved) {
      const id = active.id;
      conversations = saved;
      if (!conversations.some((c) => c.id === id)) conversations.unshift(active);
      else active = conversations.find((c) => c.id === id);
      render();
    }
  } catch {}
});

// --- start --------------------------------------------------------------------------

select(new URLSearchParams(location.search).get("conversation") ?? "");
render();
if (historyError) showError(historyError);
if (canChat && matchMedia("(pointer: fine)").matches) input.focus();
