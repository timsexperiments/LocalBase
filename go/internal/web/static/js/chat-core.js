// Chat logic without the DOM, ported from the TS UI (src/ui/reasoning.ts,
// client.ts, compaction.ts, attachments.ts) so conversations and requests
// stay identical across the two implementations. go/tools/jstest checks it
// against the TS functions.

// --- reasoning -------------------------------------------------------------------

/** Reasoning text carried by a streamed delta, preferring `reasoning`. */
export function deltaReasoning(delta) {
  return delta.reasoning || delta.reasoning_content || "";
}

const open = "<think>";
const close = "</think>";

function partialTag(text, tag) {
  for (let length = Math.min(tag.length - 1, text.length); length > 0; length--)
    if (text.endsWith(tag.slice(0, length))) return length;
  return 0;
}

/**
 * Splits streamed content into answer text and inline `<think>` reasoning.
 * Only a leading <think> opens reasoning; a possible partial tag is held
 * back until the next chunk (or flush) resolves it.
 */
export function thinkSplitter() {
  let thinking = false;
  let pending = "";
  let started = false;
  function push(chunk) {
    let text = pending + chunk;
    pending = "";
    let content = "";
    let reasoning = "";
    while (text) {
      if (!thinking) {
        const leading = !started && !content.trim();
        const index = leading ? text.indexOf(open) : -1;
        if (leading && index !== -1 && !text.slice(0, index).trim()) {
          thinking = true;
          started = true;
          text = text.slice(index + open.length);
          continue;
        }
        if (leading && !text.trim()) {
          pending = text;
          break;
        }
        const hold = leading ? partialTag(text.trimStart(), open) : 0;
        if (hold && hold === text.trimStart().length) {
          pending = text;
          break;
        }
        started = true;
        content += text;
        text = "";
      } else {
        const index = text.indexOf(close);
        if (index !== -1) {
          reasoning += text.slice(0, index);
          thinking = false;
          text = text.slice(index + close.length).replace(/^\s+/, "");
          started = true;
          continue;
        }
        const hold = partialTag(text, close);
        reasoning += text.slice(0, text.length - hold);
        pending = text.slice(text.length - hold);
        break;
      }
    }
    return { content, reasoning };
  }
  function flush() {
    const rest = pending;
    pending = "";
    return thinking ? { content: "", reasoning: rest } : { content: rest, reasoning: "" };
  }
  return { push, flush };
}

// --- device history --------------------------------------------------------------

export const historyKey = "localbase.playground.history.v1";
const storedReasoningLimit = 20_000;
const modes = ["llm", "image", "tts", "stt", "video", "embedding", "decision"];

const isString = (v) => typeof v === "string";
const isObject = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

/**
 * Validates stored history exactly as the TS historySchema does (zod
 * objects strip unknown keys); throws on anything it rejects.
 */
export function parseHistory(value) {
  const fail = () => {
    throw new Error("Saved history could not be read.");
  };
  if (!Array.isArray(value) || value.length > 30) fail();
  return value.map((c) => {
    if (!isObject(c) || !isString(c.id) || !isString(c.title) || c.title.length > 120 ||
      !["chat", "lab"].includes(c.workspace) || !modes.includes(c.mode) || !isString(c.model) ||
      !Array.isArray(c.messages)) fail();
    const conversation = {
      id: c.id, title: c.title, workspace: c.workspace, mode: c.mode, model: c.model,
      messages: c.messages.map((m) => {
        if (!isObject(m) || !isString(m.id) || !["user", "assistant"].includes(m.role) || !isString(m.text)) fail();
        const message = { id: m.id, role: m.role, text: m.text };
        if (m.attachmentsMissing !== undefined) {
          if (typeof m.attachmentsMissing !== "boolean") fail();
          message.attachmentsMissing = m.attachmentsMissing;
        }
        if (m.interrupted !== undefined) {
          if (typeof m.interrupted !== "boolean") fail();
          message.interrupted = m.interrupted;
        }
        if (m.reasoning !== undefined) {
          if (!isString(m.reasoning) || m.reasoning.length > 200000) fail();
          message.reasoning = m.reasoning;
        }
        return message;
      }),
    };
    if (c.compaction !== undefined) {
      const k = c.compaction;
      if (!isObject(k) || !isString(k.through) || !isString(k.summary) || !["summary", "dropped"].includes(k.method) ||
        !Number.isInteger(k.count) || k.count < 0) fail();
      conversation.compaction = { through: k.through, summary: k.summary, method: k.method, count: k.count };
    }
    return conversation;
  });
}

/** Saved conversations, or null when saving is off (no key). */
export function readHistory(storage) {
  const raw = storage.getItem(historyKey);
  return raw === null ? null : parseHistory(JSON.parse(raw));
}

/** The stored form: text only, no media or credentials (TS writeHistory). */
export function serializeHistory(conversations) {
  return JSON.stringify(
    conversations.slice(0, 30).map((c) => ({
      id: c.id,
      title: c.title,
      workspace: c.workspace,
      mode: c.mode,
      model: c.model,
      messages: c.messages.map(({ id, role, text, attachments, attachmentsMissing, interrupted, reasoning }) => ({
        id,
        role,
        text,
        ...(attachments?.length || attachmentsMissing ? { attachmentsMissing: true } : {}),
        ...(interrupted ? { interrupted } : {}),
        ...(reasoning
          ? { reasoning: reasoning.length > storedReasoningLimit ? `${reasoning.slice(0, storedReasoningLimit)}…` : reasoning }
          : {}),
      })),
      ...(c.compaction ? { compaction: c.compaction } : {}),
    })),
  );
}

export function writeHistory(storage, conversations) {
  storage.setItem(historyKey, serializeHistory(conversations));
}

// --- request context -------------------------------------------------------------

export const interruptedNote = "[Interrupted by the user]";
const summaryLead = "Summary of the earlier conversation:";
const summaryAck = "Understood. I will continue from that summary.";

export function userMessageContent(text, attachments) {
  if (!attachments.length) return text;
  const parts = text ? [{ type: "text", text }] : [];
  for (const attachment of attachments) {
    parts.push(attachment.kind === "image"
      ? { type: "image_url", image_url: { url: attachment.url } }
      : { type: "text", text: `Attached file ${JSON.stringify(attachment.name)}:\n${attachment.text}` });
  }
  return parts;
}

export function messageToChat(message) {
  if (message.attachmentsMissing)
    throw new Error("This saved conversation is missing its attachments. Start a new conversation and attach the files again.");
  if (message.protocol) return message.protocol;
  if (message.role === "user")
    return message.text || message.attachments?.length
      ? [{ role: "user", content: userMessageContent(message.text, message.attachments ?? []) }]
      : [];
  return message.text ? [{ role: "assistant", content: message.text }] : [];
}

export function visibleMessages(messages, compaction) {
  if (!compaction) return messages;
  const index = messages.findIndex(({ id }) => id === compaction.through);
  return index === -1 ? messages : messages.slice(index + 1);
}

function textParts(content) {
  if (typeof content === "string") return [{ type: "text", text: content }];
  return Array.isArray(content) ? content : [];
}

/**
 * The request history (TS chatContext): the summary pair, then messages
 * after the compacted prefix; interrupted replies keep their partial text,
 * and consecutive user messages are merged for strict templates.
 */
export function chatContext(messages, compaction) {
  const result = [];
  if (compaction?.summary && messages.some(({ id }) => id === compaction.through))
    result.push({ role: "user", content: `${summaryLead}\n\n${compaction.summary}` }, { role: "assistant", content: summaryAck });
  for (const message of visibleMessages(messages, compaction)) {
    const chat = message.role === "assistant" && message.interrupted && !message.protocol
      ? message.text.trim() ? [{ role: "assistant", content: `${message.text.trimEnd()}\n\n${interruptedNote}` }] : []
      : messageToChat(message);
    for (const item of chat) {
      const last = result.at(-1);
      if (item.role === "user" && last?.role === "user") {
        const parts = [...textParts(last.content), ...textParts(item.content)];
        result[result.length - 1] = {
          role: "user",
          content: parts.every((part) => part.type === "text") ? parts.map((part) => ("text" in part ? part.text : "")).join("\n\n") : parts,
        };
      } else result.push(item);
    }
  }
  return result;
}

// --- streaming -------------------------------------------------------------------

export const streamFinishGraceMs = 2000;

/**
 * Reads an OpenAI chat-completions event stream (TS streamText): content
 * and reasoning go to the callbacks; an error event throws its message.
 * Tool-call deltas are collected and returned.
 */
export async function streamText(response, append, reasoning) {
  if (!response.body) throw new Error("The gateway returned an empty response.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let done = false;
  let finished = false;
  const calls = new Map();
  const splitter = thinkSplitter();
  const emit = (part) => {
    if (part.reasoning) reasoning?.(part.reasoning);
    if (part.content) append(part.content);
  };
  const event = (raw) => {
    const data = raw.split("\n").filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
    if (!data) return;
    if (data === "[DONE]") {
      done = true;
      return;
    }
    const value = JSON.parse(data);
    if (isObject(value?.error) && isString(value.error.message)) throw new Error(value.error.message);
    for (const choice of value.choices ?? []) {
      if (choice.index !== undefined && choice.index !== 0) continue;
      if (choice.finish_reason) finished = true;
      if (!choice.delta) continue;
      const thought = deltaReasoning(choice.delta);
      if (thought) reasoning?.(thought);
      if (choice.delta.content) emit(splitter.push(choice.delta.content));
      for (const delta of choice.delta.tool_calls ?? []) {
        const call = calls.get(delta.index) ?? { id: "", type: "function", function: { name: "", arguments: "" } };
        call.id += delta.id ?? "";
        call.function.name += delta.function?.name ?? "";
        call.function.arguments += delta.function?.arguments ?? "";
        if (call.id.length > 256 || call.function.name.length > 128 || call.function.arguments.length > 20000)
          throw new Error("Tool call exceeds browser limits.");
        calls.set(delta.index, call);
      }
    }
  };
  try {
    while (!done) {
      const read = reader.read();
      let grace;
      const part = finished
        ? await Promise.race([read, new Promise((resolve) => { grace = setTimeout(() => resolve(null), streamFinishGraceMs); })]).finally(() => clearTimeout(grace))
        : await read;
      if (part === null) {
        // Finished, but the stream tail never arrived: treat it as complete.
        read.catch(() => {});
        done = true;
        break;
      }
      buffer += decoder.decode(part.value, { stream: !part.done });
      buffer = buffer.replace(/\r\n/g, "\n");
      let boundary;
      while ((boundary = buffer.indexOf("\n\n")) !== -1) {
        event(buffer.slice(0, boundary));
        buffer = buffer.slice(boundary + 2);
      }
      if (part.done) {
        if (buffer.trim()) event(buffer);
        break;
      }
    }
    emit(splitter.flush());
    if (!done) throw new Error("Connection ended before the response finished. You can retry.");
    const result = [...calls.entries()].sort(([a], [b]) => a - b).map(([, call]) => call);
    if (result.some((call) => !call.id || !call.function.name) || new Set(result.map((call) => call.id)).size !== result.length)
      throw new Error("Malformed tool call: missing or duplicate ID or missing function name.");
    return result;
  } finally {
    // Never let cleanup of a finished (or aborted) stream block the caller.
    reader.cancel().catch(() => {});
    try {
      reader.releaseLock();
    } catch {}
  }
}

/** A conversation title from its first message (TS: 60 characters). */
export function titleFrom(text) {
  return (text || "Attachments").slice(0, 60);
}

// --- compaction (TS compaction.ts) ------------------------------------------------

/** Compact once the prompt would use this share of the context window. */
export const compactionRatio = 0.75;
/** The browser can't see the runtime slot context; assume this until an overflow reports it. */
export const assumedRuntimeContext = 32_768;
const fallbackContext = 8192;
const messageOverhead = 8;
const imageTokens = 1024;
/** Tool schemas and the tool system prompt Chat adds. */
export const toolOverhead = 1500;
export const contextKey = "localbase.playground.context.v1";

/** Rough token count: ~3.5 ASCII characters per token, one per other char. */
export function estimateTextTokens(text) {
  let ascii = 0;
  let other = 0;
  for (const char of text) {
    if (char.charCodeAt(0) < 128) ascii++;
    else other++;
  }
  return Math.ceil(ascii / 3.5) + other;
}

export function estimateTokens(messages) {
  let total = 0;
  for (const message of messages) {
    total += messageOverhead;
    if (typeof message.content === "string") total += estimateTextTokens(message.content);
    else if (Array.isArray(message.content))
      for (const part of message.content) total += part.type === "text" ? estimateTextTokens(part.text) : imageTokens;
    if (message.role === "assistant")
      for (const call of message.tool_calls ?? [])
        total += messageOverhead + estimateTextTokens(call.function.name + call.function.arguments);
  }
  return total;
}

function learnedContexts(storage) {
  try {
    const value = JSON.parse(storage.getItem(contextKey) ?? "{}");
    if (!value || typeof value !== "object") return {};
    return Object.fromEntries(Object.entries(value).filter((e) => typeof e[1] === "number" && Number.isInteger(e[1]) && e[1] > 0));
  } catch {
    return {};
  }
}

/** Context size reported by a llama.cpp overflow error, if the text is one. */
export function overflowContext(message) {
  const match = /available context size \((\d+) tokens\)/i.exec(message);
  const size = match ? Number(match[1]) : NaN;
  return Number.isSafeInteger(size) && size > 0 ? size : null;
}

/** Remembers a model's runtime context; returns false if unchanged. */
export function learnContext(storage, modelId, size) {
  const contexts = learnedContexts(storage);
  if (contexts[modelId] === size) return false;
  try {
    storage.setItem(contextKey, JSON.stringify({ ...contexts, [modelId]: size }));
  } catch {
    // Best-effort; the next overflow reports the size again.
  }
  return true;
}

/** Best known runtime context for a chat model ({id, context}). */
export function contextWindow(storage, model) {
  const learned = learnedContexts(storage)[model.id];
  if (learned) return learned;
  return model.context ? Math.min(model.context, assumedRuntimeContext) : fallbackContext;
}

/** Prompt budget: leave room for the reply and stay under the ratio. */
export function promptBudget(context, maxTokens) {
  const reserve = Math.min(maxTokens ?? 1024, Math.floor(context / 2));
  return Math.floor(Math.min(context * compactionRatio, context - reserve));
}

/**
 * Chooses the messages to fold so the prompt fits `budget`, keeping at least
 * the newest turn (a user message and what follows it) verbatim.
 */
export function planCompaction(options) {
  const { messages, compaction, budget } = options;
  const overhead = options.overhead ?? 0;
  const visible = visibleMessages(messages, compaction);
  const starts = visible.flatMap((message, index) => (message.role === "user" ? [index] : []));
  const fits = (kept, extra) => overhead + estimateTokens(chatContext(kept)) + extra <= budget;
  if (!options.force && fits(visible, compaction ? estimateTextTokens(compaction.summary) + 40 : 0)) return null;
  for (let keep = options.keepTurns ?? 2; keep >= 1; keep--) {
    const start = starts.at(-keep);
    if (start === undefined || start === 0) continue;
    const kept = visible.slice(start);
    // Leave room for the summary that will replace the covered turns.
    if (options.force || keep === 1 || fits(kept, Math.min(2048, Math.floor(budget / 4))))
      return { covered: visible.slice(0, start), through: visible[start - 1] };
  }
  return null;
}

function middle(text, limit) {
  if (text.length <= limit) return text;
  const half = Math.max(0, Math.floor((limit - 20) / 2));
  return `${text.slice(0, half)}\n[… ${text.length - half * 2} characters omitted …]\n${text.slice(-half)}`;
}

/** Plain-text transcript for the summarizer; reasoning and media are omitted. */
export function transcript(messages, characters) {
  const entries = messages.flatMap((message) => {
    const names = (message.attachments ?? []).map(({ name }) => name);
    const media = message.artifacts?.filter((a) => a.state === "complete");
    const notes = [
      ...(names.length ? [`[attached: ${names.join(", ")}]`] : []),
      ...(message.attachmentsMissing && !names.length ? ["[attachments not available]"] : []),
      ...(media?.length ? [`[generated: ${media.map((a) => a.label).join(", ")}]`] : []),
      ...(message.interrupted ? [interruptedNote] : []),
    ];
    const text = [message.text.trim(), ...notes].filter(Boolean).join("\n");
    return text ? [{ role: message.role === "user" ? "User" : "Assistant", text }] : [];
  });
  const share = Math.max(400, Math.floor(characters / (entries.length || 1)));
  return entries.map(({ role, text }) => `${role}: ${middle(text, share)}`).join("\n\n");
}

export function stripThinking(text) {
  return text.replace(/<think>[\s\S]*?<\/think>/g, "").replace(/<think>[\s\S]*$/, "").trim();
}

const summaryPrompt =
  "You compress chat history. Write a concise summary of the conversation below as short bullet points. Preserve facts, decisions, names, numbers, file names, code identifiers, user preferences, and open questions. Do not add commentary, do not answer the conversation, and do not mention that you are summarizing.";

/** The summarizer request body (TS summarize). */
export function summaryRequest(options) {
  const maxTokens = Math.min(2048, Math.floor(options.context / 4));
  // ~3 characters per token keeps the summarizer prompt within ~60% of context.
  const characters = Math.max(2000, Math.floor(options.context * 0.6 * 3));
  const previous = options.previous ? `Earlier summary:\n${middle(options.previous, Math.floor(characters / 4))}\n\n` : "";
  return {
    model: options.model.id,
    stream: true,
    temperature: 0.2,
    max_tokens: maxTokens,
    // Reasoning templates would otherwise spend the budget thinking.
    chat_template_kwargs: { enable_thinking: false },
    messages: [
      { role: "system", content: summaryPrompt },
      { role: "user", content: `${previous}Conversation:\n${transcript(options.messages, characters - previous.length)}` },
    ],
  };
}

/**
 * Asks the chat model for a summary; throws when it fails or returns nothing.
 * `post(body, signal)` sends a chat-completions request and resolves to an ok
 * Response (or throws).
 */
export async function summarize(options) {
  let content = "";
  await streamText(await options.post(summaryRequest(options), options.signal), (text) => {
    content += text;
  });
  const summary = stripThinking(content);
  if (!summary) throw new Error("The model returned an empty summary.");
  return summary.length > 12_000 ? `${summary.slice(0, 12_000)}…` : summary;
}

/**
 * Compacts `covered` into the conversation's summary. If summarization fails
 * (and was not cancelled), falls back to dropping those messages from context.
 */
export async function compact(options) {
  const previous = options.compaction && options.messages.some(({ id }) => id === options.compaction?.through) ? options.compaction : undefined;
  const count = (previous?.count ?? 0) + options.covered.length;
  try {
    const summary = await summarize({ ...options, messages: options.covered, previous: previous?.summary });
    return { compaction: { through: options.through.id, summary, method: "summary", count } };
  } catch (error) {
    options.signal.throwIfAborted();
    return {
      // Keep the previous summary; only the newly covered turns are lost.
      compaction: { through: options.through.id, summary: previous?.summary ?? "", method: "dropped", count },
      error: error instanceof Error ? error.message : "Summarization failed.",
    };
  }
}

// --- steering (TS steering.ts) -----------------------------------------------------

/** Abort reason for a reply interrupted by a steering message. */
export const steerAbortReason = Symbol("steer");

export function phaseLabel(phase, seconds) {
  const label = phase === "compacting" ? "Compacting earlier messages" : phase === "thinking" ? "Thinking" : "Waiting for the model";
  return seconds >= 1 ? `${label}… ${Math.floor(seconds)}s` : `${label}…`;
}

/**
 * Marks a cut-short reply: its partial text is resent as context (without
 * reasoning or half-finished tool protocol) and running tools show `detail`.
 */
export function interruptReply(message, detail) {
  return {
    ...message,
    protocol: undefined,
    interrupted: true,
    artifacts: message.artifacts?.map((artifact) =>
      artifact.state === "working" ? { id: artifact.id, label: artifact.label, state: "error", detail } : artifact),
  };
}
