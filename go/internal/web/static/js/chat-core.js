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
