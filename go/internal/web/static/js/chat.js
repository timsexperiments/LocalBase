// Chat page (DESIGN §15 step 1, .design/research/chat/BRIEF.md). Replies
// stream from /app/api/v1/chat/completions as the signed-in person.
// Conversations live in this browser only: in memory, and in localStorage
// when "Save text history on this device" is on (the TS UI's key and
// schema, so history carries over). Pure logic is in chat-core.js and
// markdown.js.
import {
  chatContext, compact, contextWindow, interruptReply, learnContext, overflowContext, phaseLabel, planCompaction,
  promptBudget, readHistory, steerAbortReason, titleFrom, toolOverhead, visibleMessages, writeHistory,
} from "./chat-core.js";
import { attachmentAccept, chatRequestError, readAttachments } from "./attachments.js";
import { build, parse } from "./markdown.js";
import { appendDictation, dictationUnavailable, startRecording, transcribe } from "./dictation.js";
import { SessionRequiredError, api, generationTools, jsonRequest, modelGenerationSettings, modelName, runChat, toolModels } from "./tools.js";
import { settingsFields } from "./generation.js";

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
const attachButton = $("[data-attach]");
const attachInput = $("[data-attach-input]");
const draftList = $("[data-attachments]");
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
// Files attached to the draft; a read in progress is dropped (epoch) when
// the conversation or model changes under it.
let draft = [];
let reading = false;
let attachEpoch = 0;
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
// Chat always budgets for the tool schemas and tool system prompt (TS).
const overhead = toolOverhead;
// The gateway's model metadata (for tools), loaded after the page.
let catalog = [];
const permissions = JSON.parse($("#chat-permissions").textContent || "[]");
// Generation settings per model id, for this page only (TS).
let preferences = {};
const mediaUrls = [];


const chatsOnly = () => conversations.filter((c) => c.workspace === "chat" && c.mode === "llm");

function select(id) {
  if (active && id !== active.id) {
    dropRead();
    cancelDictation();
    draft = [];
  }
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

function showError(message, retry, label = "Retry") {
  errorBox.replaceChildren();
  const card = document.createElement("div");
  card.className = "card mb-2 flex flex-wrap items-center justify-between gap-2 border-danger-line bg-danger-soft px-4 py-2";
  const text = document.createElement("p");
  text.className = "min-w-0 flex-1 text-sm font-semibold text-danger";
  text.textContent = message;
  card.append(text);
  if (retry) card.append(button(label, "btn btn-quiet text-sm", retry));
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

// chips lists attachments: an image thumbnail or file name and size, with a
// Remove button on the draft's.
function chips(attachments, remove) {
  const ul = document.createElement("ul");
  ul.className = "mt-1.5 flex max-w-full flex-wrap justify-end gap-2";
  ul.setAttribute("aria-label", "Attachments");
  for (const a of attachments) {
    const li = document.createElement("li");
    li.className = `flex min-h-11 max-w-full items-center gap-2 rounded-xl border border-line bg-surface py-1 pl-1 ${remove ? "pr-1" : "pr-3"} text-sm`;
    if (a.kind === "image") {
      const img = document.createElement("img");
      img.src = a.url;
      img.alt = a.name;
      img.className = "size-10 shrink-0 rounded-lg object-cover";
      li.append(img);
    } else {
      const icon = document.createElement("span");
      icon.className = "grid size-10 shrink-0 place-items-center rounded-lg bg-hover text-xs font-semibold uppercase text-muted";
      icon.setAttribute("aria-hidden", "true");
      icon.textContent = a.name.split(".").at(-1)?.slice(0, 4) ?? "";
      li.append(icon);
    }
    const info = document.createElement("span");
    info.className = "grid min-w-0";
    const name = document.createElement("span");
    name.className = "truncate font-semibold";
    name.textContent = a.name;
    name.title = a.name;
    const size = document.createElement("span");
    size.className = "text-xs text-muted";
    size.textContent = `${Math.max(1, Math.ceil(a.size / 1024))} KB`;
    info.append(name, size);
    li.append(info);
    if (remove) li.append(button("✕", "btn btn-quiet size-10 shrink-0 border-0 p-0", () => remove(a), { "aria-label": `Remove ${a.name}` }));
    ul.append(li);
  }
  return ul;
}

// artifactNode shows a tool call: its progress, its error, or the media it
// made (TS MediaCard). Media lives in this page only; history keeps text.
function artifactNode(a) {
  const section = document.createElement("section");
  section.className = "artifact";
  section.dataset.artifact = `${a.id}:${a.state}`;
  const title = document.createElement("h3");
  title.className = "text-sm font-semibold capitalize";
  title.textContent = a.label.replaceAll("_", " ");
  section.append(title);
  if (a.state !== "complete") {
    const p = document.createElement("p");
    p.className = `text-sm ${a.state === "error" ? "text-danger" : "working text-muted"}`;
    p.textContent = a.detail;
    section.append(p);
    return section;
  }
  const media = a.media;
  if (media.kind === "image") {
    const img = document.createElement("img");
    img.src = media.url;
    img.alt = "Generated from your prompt";
    img.className = "max-h-[60dvh] w-auto max-w-full rounded-xl";
    section.append(img);
  } else {
    const player = document.createElement(media.kind);
    player.controls = true;
    player.src = media.url;
    player.className = "w-full max-w-full rounded-xl";
    if (media.kind === "video") {
      player.playsInline = true;
      player.preload = "metadata";
      player.setAttribute("aria-label", "Generated video");
      player.addEventListener("error", () => {
        const note = document.createElement("p");
        note.setAttribute("role", "status");
        note.className = "text-sm text-muted";
        note.textContent = "This browser could not play the video. Download the MP4 to play it in another player.";
        player.after(note);
      }, { once: true });
    }
    section.append(player);
  }
  const download = document.createElement("a");
  download.href = media.url;
  download.download = `localbase.${media.kind === "image" ? "png" : media.kind === "audio" ? "wav" : media.format}`;
  download.className = "md-copy";
  download.textContent = `Download ${media.kind}`;
  section.append(download);
  return section;
}

function messageNode(m) {
  const article = document.createElement("article");
  article.dataset.id = m.id;
  if (m.role === "user") {
    article.className = "flex flex-col items-end";
    article.append(heading("You"));
    if (m.text) {
      const bubble = document.createElement("div");
      bubble.className = "max-w-[85%] whitespace-pre-wrap wrap-anywhere rounded-2xl rounded-br-md bg-accent-soft px-4 py-2.5";
      bubble.textContent = m.text;
      article.append(bubble);
    }
    if (m.attachments?.length) article.append(chips(m.attachments));
    if (m.attachmentsMissing) {
      const note = document.createElement("p");
      note.className = "mt-1 text-sm text-muted";
      note.textContent = "Attachments from the earlier session are no longer available.";
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
  for (const a of m.artifacts ?? []) article.append(artifactNode(a));
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
  renderDraft();
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
  // Keep finished media players (and their playback) across re-renders.
  if (old)
    for (const section of next.querySelectorAll("[data-artifact$=\":complete\"]")) {
      const kept = old.querySelector(`[data-artifact="${CSS.escape(section.dataset.artifact)}"]`);
      if (kept) section.replaceWith(kept);
    }
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
      dropRead();
      save();
      renderModel();
      picker.close();
      renderDraft();
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
  if (abort) cancelDictation();
  updateSend();
  renderDraft();
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
const post = (body, signal) => api("/v1/chat/completions", jsonRequest(body, signal));

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
  const user = { id: uuid(), role: "user", text, ...(draft.length ? { attachments: draft } : {}) };
  let problem;
  try {
    problem = chatRequestError([...conversation.messages, user], model);
  } catch (e) {
    problem = e.message;
  }
  if (problem) {
    showError(problem);
    return false;
  }
  if (steering) {
    const old = running.answer;
    Object.assign(old, interruptReply(old, "Interrupted"));
    delete old.protocol;
    delete old.pending;
    delete old.pendingText;
    abort.abort(steerAbortReason);
  }
  if (!conversation.messages.length) conversation.title = text ? titleFrom(text) : "Attachments";
  conversation.messages.push(user);
  draft = [];
  renderDraft();
  reply(conversation, model);
  return true;
}

// --- attachments ----------------------------------------------------------------

const currentModel = () => ready.find((m) => m.id === active.model);

function renderDraft() {
  draftList.replaceChildren(...(draft.length ? chips(draft, (a) => {
    draft = draft.filter((x) => x !== a);
    renderDraft();
    announce(`Removed ${a.name}`);
    input.focus();
  }).children : []));
  draftList.hidden = !draft.length;
  const accept = attachmentAccept(currentModel());
  attachInput.accept = accept;
  attachButton.disabled = !canChat || !accept || !!abort || reading;
  paintNote();
}

// --- dictation ------------------------------------------------------------------

const dictateButton = $("[data-dictate]");
const discardButton = $("[data-dictate-discard]");
const dictationModel = dictateButton?.dataset.model ?? "";
let dictation = null; // {abort, stop?, phase: "requesting" | "recording" | "transcribing"}

function paintNote() {
  const phase = dictation?.phase;
  $("[data-composer-note]").textContent = reading ? "Reading files…" : phase === "requesting" ? "Waiting for microphone…" :
    phase === "recording" ? "Recording…" : phase === "transcribing" ? "Transcribing…" : "";
  if (!dictateButton) return;
  dictateButton.disabled = !canChat || !!abort;
  dictateButton.setAttribute("aria-label", phase === "recording" ? "Stop recording for message" : phase ? "Cancel dictation for message" : "Dictate message");
  $("[data-dictate-icon]", dictateButton).hidden = !!phase;
  $("[data-dictate-stop]", dictateButton).hidden = phase !== "recording";
  $("[data-dictate-cancel]", dictateButton).hidden = !phase || phase === "recording";
  dictateButton.classList.toggle("text-danger", phase === "recording");
  discardButton.hidden = phase !== "recording";
}

function cancelDictation() {
  if (!dictation) return;
  dictation.abort.abort();
  dictation = null;
  paintNote();
}

async function startDictation() {
  cancelDictation();
  const operation = { abort: new AbortController(), phase: "requesting" };
  const { signal } = operation.abort;
  dictation = operation;
  paintNote();
  const phase = (p) => {
    operation.phase = p;
    if (dictation === operation) paintNote();
  };
  try {
    const recording = await startRecording({ signal });
    if (signal.aborted) {
      recording.cancel();
      return;
    }
    operation.stop = () => recording.stop();
    phase("recording");
    const audio = await recording.finished;
    if (signal.aborted) return;
    phase("transcribing");
    const text = await transcribe(audio, dictationModel, signal, apiError);
    if (signal.aborted) return;
    if (!text.trim()) showError("No speech was recognized. Try again.");
    else {
      input.value = appendDictation(input.value, text);
      grow();
      updateSend();
      announce("Dictation added to your message");
    }
  } catch (e) {
    if (!signal.aborted) showError(e instanceof Error && e.name !== "NotAllowedError" ? e.message : "Microphone access was not allowed.");
  } finally {
    operation.abort.abort();
    if (dictation === operation) {
      dictation = null;
      paintNote();
      input.focus();
    }
  }
}

dictateButton?.addEventListener("click", () => {
  const unavailable = dictationUnavailable(dictationModel);
  if (unavailable) {
    showError(unavailable);
    return;
  }
  if (dictation?.phase === "recording") dictation.stop?.();
  else if (dictation) cancelDictation();
  else startDictation();
});
discardButton?.addEventListener("click", () => {
  cancelDictation();
  announce("Recording discarded");
  dictateButton.focus();
});
document.addEventListener("visibilitychange", () => {
  if (document.hidden) cancelDictation();
});

// dropRead abandons a file read in progress (the model or conversation changed).
function dropRead() {
  attachEpoch++;
  if (reading) {
    reading = false;
    showError("File reading was canceled because the model or conversation changed. Choose the files again.");
  }
}

attachButton.addEventListener("click", () => attachInput.click());
attachInput.addEventListener("change", async () => {
  const files = [...attachInput.files];
  attachInput.value = "";
  const model = currentModel();
  if (!files.length || !model || abort || reading) return;
  const epoch = attachEpoch;
  reading = true;
  renderDraft();
  try {
    // Every attachment open in this browser counts toward the memory limit.
    const retained = conversations.flatMap((c) => c.messages.flatMap((m) => m.attachments ?? []));
    const added = await readAttachments(files, model, draft, retained);
    if (epoch !== attachEpoch) return;
    draft = [...draft, ...added];
    errorBox.replaceChildren();
    announce(`Attached ${added.map((a) => a.name).join(", ")}`);
  } catch (e) {
    if (epoch === attachEpoch) showError(e instanceof Error ? e.message : "Could not read the files. Choose them again.");
  } finally {
    if (epoch === attachEpoch) {
      reading = false;
      renderDraft();
    }
  }
});

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
    const plan = planCompaction({ messages: history, compaction, budget: promptBudget(context, modelGenerationSettings(preferences, model.id).llm.max_tokens), overhead, force });
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
    // The gateway's metadata for this model (tool calling); without it the
    // reply is plain text.
    const full = catalog.find((m) => m.id === model.id) ?? { id: model.id, catalog: { features: [], capabilityResolution: null } };
    const chat = () => runChat({
      preferences,
      model: full,
      models: catalog,
      signal: controller.signal,
      messages: chatContext(history, compaction),
      toolsEnabled: true,
      permissions,
      append: (t) => {
        streamed = true;
        answer.text += t;
        update();
      },
      reasoning: (t) => {
        streamed = true;
        if (phase.name !== "thinking") setPhase("thinking");
        answer.reasoning = (answer.reasoning ?? "") + t;
        update();
      },
      artifact: (a) => {
        answer.artifacts = [...(answer.artifacts ?? []).filter((x) => x.id !== a.id), a];
        if (a.state !== "working") announce(a.state === "error" ? `${a.label.replaceAll("_", " ")} failed: ${a.detail}` : `${a.label.replaceAll("_", " ")} finished`);
        update();
      },
      register: (url) => mediaUrls.push(url),
      warning: warn,
    });
    let protocol;
    try {
      protocol = await chat();
    } catch (e) {
      // An overflow reports the real runtime context: learn it, compact to
      // fit, and retry once if nothing was streamed yet.
      const size = e instanceof Error && !streamed && !controller.signal.aborted ? overflowContext(e.message) : null;
      if (!size) throw e;
      learnContext(storage, model.id, size);
      if (!(await compactHistory(size, true))) throw e;
      protocol = await chat();
    }
    answer.protocol = protocol;
  } catch (e) {
    if (controller.signal.aborted) {
      const steered = controller.signal.reason === steerAbortReason;
      Object.assign(answer, interruptReply(answer, steered ? "Interrupted" : "Stopped"));
      delete answer.protocol;
      outcome = steered ? "" : "Stopped";
    } else {
      outcome = "Reply failed";
      const message = e instanceof TypeError ? "LocalBase couldn’t be reached. Check the connection and retry." : e.message;
      if (!answer.text && !answer.reasoning && !answer.artifacts?.length) conversation.messages.splice(conversation.messages.indexOf(answer), 1);
      else answer.interrupted = true;
      // A session that ended (here or at the edge) needs a reload, not a retry.
      queueMicrotask(() => e instanceof SessionRequiredError
        ? showError(message, () => location.reload(), "Reload")
        : showError(message, () => retry(conversation)));
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
    // Pick up model changes made elsewhere (TS refreshes after each send).
    loadCatalog();
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
    plan = planCompaction({ messages: conversation.messages, compaction: conversation.compaction, budget: promptBudget(context, modelGenerationSettings(preferences, model.id).llm.max_tokens), overhead, force: true });
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
  // Attachments alone can start a turn, but only text steers a running reply.
  if (!canChat || reading || (!text && (!draft.length || abort))) return;
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

// --- generation controls ----------------------------------------------------------

const generation = $("#generation");
const toolMode = { generate_image: "image", generate_video: "video", synthesize_speech: "tts" };
const modeLabel = { image: "Image", video: "Video", tts: "Speech" };

function renderGeneration() {
  const body = $("[data-generation-body]");
  const parts = [];
  const note = (text) => {
    const p = document.createElement("p");
    p.className = "text-sm text-muted";
    p.textContent = text;
    return p;
  };
  const model = catalog.find((m) => m.id === active.model);
  const setFor = (id) => (settings) => {
    preferences = { ...preferences, [id]: settings };
  };
  if (!model) {
    parts.push(note("Choose an installed model to adjust its controls."));
  } else {
    const tools = generationTools(catalog, model, permissions);
    parts.push(note(tools.length
      ? `Chat tools: ${tools.map((t) => t.function.name.replaceAll("_", " ")).join(", ")}.`
      : "Text chat only. Tools need a tool-calling chat model and installed media models."));
    parts.push(note("These apply to the model wherever it runs: chat, its tools, Model Lab and the studios. They are not saved and reset when the page reloads. Blank fields use model defaults."));
    const heading = document.createElement("h3");
    heading.className = "font-bold";
    heading.textContent = modelName(model);
    parts.push(heading, settingsFields("llm", model, modelGenerationSettings(preferences, model.id), setFor(model.id)));
    for (const tool of tools) {
      const mode = toolMode[tool.function.name];
      for (const target of toolModels(catalog, tool.function.name)) {
        const details = document.createElement("details");
        details.className = "reasoning";
        const summary = document.createElement("summary");
        summary.textContent = `${modelName(target)} · ${modeLabel[mode]}`;
        details.append(summary, settingsFields(mode, target, modelGenerationSettings(preferences, target.id), setFor(target.id)));
        parts.push(details);
      }
    }
  }
  body.replaceChildren(...parts);
}

$("[data-open-generation]").addEventListener("click", () => {
  renderGeneration();
  generation.showModal();
});
$("[data-close-generation]").addEventListener("click", () => generation.close());
generation.addEventListener("click", (e) => {
  if (e.target === generation) generation.close();
});

// The gateway's model metadata, as the TS UI loads it; tools and the
// generation controls need its capabilities. Plain chat works without it.
async function loadCatalog() {
  try {
    const body = await (await api("/_localbase/models")).json();
    if (Array.isArray(body?.data)) catalog = body.data;
  } catch {
    // Tools stay off; the next page load tries again.
  }
}

// --- start --------------------------------------------------------------------------

select(new URLSearchParams(location.search).get("conversation") ?? "");
render();
if (canChat) loadCatalog();
if (historyError) showError(historyError);
if (canChat && matchMedia("(pointer: fine)").matches) input.focus();
