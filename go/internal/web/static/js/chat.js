// Chat page (DESIGN §15 step 1, .design/research/chat/BRIEF.md). Replies
// stream from /app/api/v1/chat/completions as the signed-in person.
// Conversations live in this browser only: in memory, and in localStorage
// when "Save text history on this device" is on (the TS UI's key and
// schema, so history carries over). Pure logic is in chat-core.js and
// markdown.js.
import { chatContext, readHistory, streamText, titleFrom, writeHistory } from "./chat-core.js";
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
let abort = null;
let follow = true;

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
    article.append(actions);
  }
  return article;
}

function render() {
  $("#chat-title").textContent = active.title;
  document.title = `${active.messages.length ? active.title : "Chat"} · LocalBase`;
  renderModel();
  list.replaceChildren(...active.messages.map(messageNode));
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

function setBusy(busy) {
  sendButton.hidden = busy;
  stopButton.hidden = !busy;
  for (const b of $$("[data-new-chat]")) b.disabled = busy;
}

async function apiError(response) {
  if (response.status === 401) return "Your session ended. Reload the page to sign in again.";
  try {
    const body = await response.json();
    if (body?.error?.message) return body.error.message;
  } catch {}
  return `The gateway answered ${response.status}.`;
}

function openPicker() {
  renderPicker();
  picker.showModal();
}

// send starts a reply, or explains why it cannot and returns false (the
// caller keeps the draft).
function send(text) {
  errorBox.replaceChildren();
  const model = ready.find((m) => m.id === active.model);
  if (!model) {
    showError("This conversation’s model isn’t ready. Choose a ready model to continue.");
    openPicker();
    return false;
  }
  const conversation = active;
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
  render();
  follow = true;
  scrollToEnd();
  setBusy(true);
  announce("Reply started");
  const started = Date.now();
  let phase = "Waiting for a reply";
  const tick = setInterval(() => {
    const seconds = Math.round((Date.now() - started) / 1000);
    answer.pendingText = seconds >= 2 ? `${phase}… ${seconds}s` : `${phase}…`;
    const w = list.querySelector(`article[data-id="${CSS.escape(answer.id)}"] [data-working]`);
    if (w) w.textContent = answer.pendingText;
  }, 1000);
  let frame = 0;
  const update = () => {
    if (!frame) frame = requestAnimationFrame(() => {
      frame = 0;
      rerenderMessage(answer);
    });
  };
  abort = new AbortController();
  let outcome = "Reply finished";
  try {
    const response = await fetch("/app/api/v1/chat/completions", {
      method: "POST",
      signal: abort.signal,
      credentials: "same-origin",
      headers: { "content-type": "application/json", "X-Localbase-UI": "1" },
      body: JSON.stringify({ model: model.id, stream: true, messages: chatContext(history, conversation.compaction) }),
    });
    if (!response.ok) throw new Error(await apiError(response));
    await streamText(response, (t) => {
      answer.text += t;
      update();
    }, (t) => {
      phase = "Thinking";
      answer.reasoning = (answer.reasoning ?? "") + t;
      update();
    });
  } catch (e) {
    if (abort.signal.aborted) {
      answer.interrupted = true;
      outcome = "Stopped";
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
    abort = null;
    setBusy(false);
    save();
    if (conversation === active) render();
    if (outcome !== "Reply failed") announce(outcome);
  }
}

function retry(conversation) {
  if (abort || conversation !== active) return;
  errorBox.replaceChildren();
  const last = conversation.messages.at(-1);
  if (last?.role === "assistant" && last.interrupted) conversation.messages.pop();
  const model = ready.find((m) => m.id === conversation.model);
  if (!model) {
    showError("This conversation’s model isn’t ready. Choose a ready model to continue.");
    openPicker();
    return;
  }
  reply(conversation, model);
}

form.addEventListener("submit", (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text || abort || !canChat) return;
  if (send(text)) {
    input.value = "";
    grow();
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
input.addEventListener("input", grow);

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
