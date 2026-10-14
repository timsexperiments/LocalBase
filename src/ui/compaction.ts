/**
 * Chat context management: estimate prompt size, fold older turns into a
 * model-written summary, and build the request history from what remains.
 * Original messages always stay in the stored conversation.
 */
import { messageToChat } from "./attachments";
import {
  api,
  streamText,
  type ChatMessage,
  type Compaction,
  type Connection,
  type Message,
  type Model,
  type UserContentPart,
} from "./client";
import { stripThinking } from "./reasoning";

/** Compact once the prompt would use this share of the context window. */
export const compactionRatio = 0.75;
/**
 * The UI cannot see the runtime per-slot context (`/v1/models` is not exposed
 * to the browser), and catalog windows are training maxima, so assume a
 * conservative runtime size until an overflow error reports the real one.
 */
export const assumedRuntimeContext = 32_768;
const fallbackContext = 8192;
const messageOverhead = 8;
const imageTokens = 1024;
/** Tool schemas and the tool system prompt runChat adds in Chat. */
export const toolOverhead = 1500;
export const interruptedNote = "[Interrupted by the user]";
const summaryLead = "Summary of the earlier conversation:";
const summaryAck = "Understood. I will continue from that summary.";

/** Rough token count: ~3.5 ASCII characters per token, one per other char. */
export function estimateTextTokens(text: string) {
  let ascii = 0;
  let other = 0;
  for (const char of text) {
    if (char.charCodeAt(0) < 128) ascii++;
    else other++;
  }
  return Math.ceil(ascii / 3.5) + other;
}

export function estimateTokens(messages: ChatMessage[]) {
  let total = 0;
  for (const message of messages) {
    total += messageOverhead;
    if (typeof message.content === "string")
      total += estimateTextTokens(message.content);
    else if (Array.isArray(message.content))
      for (const part of message.content)
        total +=
          part.type === "text" ? estimateTextTokens(part.text) : imageTokens;
    if (message.role === "assistant")
      for (const call of message.tool_calls ?? [])
        total +=
          messageOverhead +
          estimateTextTokens(call.function.name + call.function.arguments);
  }
  return total;
}

const contextKey = "localbase.playground.context.v1";
function learnedContexts(): Record<string, number> {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(contextKey) ?? "{}");
    if (!value || typeof value !== "object") return {};
    return Object.fromEntries(
      Object.entries(value).filter(
        (entry): entry is [string, number] =>
          typeof entry[1] === "number" &&
          Number.isInteger(entry[1]) &&
          entry[1] > 0,
      ),
    );
  } catch {
    return {};
  }
}

/** Context size reported by a llama.cpp overflow error, if the text is one. */
export function overflowContext(message: string) {
  const match = /available context size \((\d+) tokens\)/i.exec(message);
  const size = match ? Number(match[1]) : NaN;
  return Number.isSafeInteger(size) && size > 0 ? size : null;
}

/** Remembers a model's runtime context; returns false if unchanged. */
export function learnContext(modelId: string, size: number) {
  const contexts = learnedContexts();
  if (contexts[modelId] === size) return false;
  try {
    localStorage.setItem(
      contextKey,
      JSON.stringify({ ...contexts, [modelId]: size }),
    );
  } catch {
    // Learning is best-effort; the next overflow reports the size again.
  }
  return true;
}

/** Best known runtime context for a chat model. */
export function contextWindow(model: Pick<Model, "id" | "catalog">) {
  const learned = learnedContexts()[model.id];
  if (learned) return learned;
  const catalog = model.catalog.contextWindowTokens;
  return catalog ? Math.min(catalog, assumedRuntimeContext) : fallbackContext;
}

/** Prompt budget: leave room for the reply and stay under the ratio. */
export function promptBudget(context: number, maxTokens?: number) {
  const reserve = Math.min(maxTokens ?? 1024, Math.floor(context / 2));
  return Math.floor(Math.min(context * compactionRatio, context - reserve));
}

/** Messages still sent verbatim (after the compacted prefix). */
export function visibleMessages(messages: Message[], compaction?: Compaction) {
  if (!compaction) return messages;
  const index = messages.findIndex(({ id }) => id === compaction.through);
  return index === -1 ? messages : messages.slice(index + 1);
}

function textParts(content: ChatMessage["content"]): UserContentPart[] {
  if (typeof content === "string") return [{ type: "text", text: content }];
  return Array.isArray(content) ? (content as UserContentPart[]) : [];
}

/**
 * Builds the request history: summary pair, then messages after the
 * compacted prefix. Interrupted replies keep their partial text (reasoning is
 * never sent), and consecutive user messages are merged for strict templates.
 */
export function chatContext(messages: Message[], compaction?: Compaction) {
  const result: ChatMessage[] = [];
  if (
    compaction?.summary &&
    messages.some(({ id }) => id === compaction.through)
  )
    result.push(
      { role: "user", content: `${summaryLead}\n\n${compaction.summary}` },
      { role: "assistant", content: summaryAck },
    );
  for (const message of visibleMessages(messages, compaction)) {
    const chat =
      message.role === "assistant" && message.interrupted && !message.protocol
        ? message.text.trim()
          ? [
              {
                role: "assistant" as const,
                content: `${message.text.trimEnd()}\n\n${interruptedNote}`,
              },
            ]
          : []
        : messageToChat(message);
    for (const item of chat) {
      const last = result.at(-1);
      if (item.role === "user" && last?.role === "user") {
        const parts = [...textParts(last.content), ...textParts(item.content)];
        result[result.length - 1] = {
          role: "user",
          content: parts.every((part) => part.type === "text")
            ? parts
                .map((part) => ("text" in part ? part.text : ""))
                .join("\n\n")
            : parts,
        };
      } else result.push(item);
    }
  }
  return result;
}

/**
 * Chooses the messages to fold so the prompt fits `budget`, keeping at least
 * the newest turn (a user message and what follows it) verbatim.
 */
export function planCompaction(options: {
  messages: Message[];
  compaction?: Compaction;
  budget: number;
  overhead?: number;
  keepTurns?: number;
  force?: boolean;
}) {
  const { messages, compaction, budget } = options;
  const overhead = options.overhead ?? 0;
  const visible = visibleMessages(messages, compaction);
  const starts = visible.flatMap((message, index) =>
    message.role === "user" ? [index] : [],
  );
  const fits = (kept: Message[], extra: number) =>
    overhead + estimateTokens(chatContext(kept)) + extra <= budget;
  if (
    !options.force &&
    fits(visible, compaction ? estimateTextTokens(compaction.summary) + 40 : 0)
  )
    return null;
  for (let keep = options.keepTurns ?? 2; keep >= 1; keep--) {
    const start = starts.at(-keep);
    if (start === undefined || start === 0) continue;
    const kept = visible.slice(start);
    // Leave room for the summary that will replace the covered turns.
    if (
      options.force ||
      keep === 1 ||
      fits(kept, Math.min(2048, Math.floor(budget / 4)))
    )
      return { covered: visible.slice(0, start), through: visible[start - 1]! };
  }
  return null;
}

function middle(text: string, limit: number) {
  if (text.length <= limit) return text;
  const half = Math.max(0, Math.floor((limit - 20) / 2));
  return `${text.slice(0, half)}\n[… ${text.length - half * 2} characters omitted …]\n${text.slice(-half)}`;
}

/** Plain-text transcript for the summarizer; reasoning and media are omitted. */
export function transcript(messages: Message[], characters: number) {
  const entries = messages.flatMap((message) => {
    const names = (message.attachments ?? []).map(({ name }) => name);
    const media = message.artifacts?.filter((a) => a.state === "complete");
    const notes = [
      ...(names.length ? [`[attached: ${names.join(", ")}]`] : []),
      ...(message.attachmentsMissing && !names.length
        ? ["[attachments not available]"]
        : []),
      ...(media?.length
        ? [`[generated: ${media.map((a) => a.label).join(", ")}]`]
        : []),
      ...(message.interrupted ? [interruptedNote] : []),
    ];
    const text = [message.text.trim(), ...notes].filter(Boolean).join("\n");
    return text
      ? [{ role: message.role === "user" ? "User" : "Assistant", text }]
      : [];
  });
  const share = Math.max(400, Math.floor(characters / (entries.length || 1)));
  return entries
    .map(({ role, text }) => `${role}: ${middle(text, share)}`)
    .join("\n\n");
}

const summaryPrompt =
  "You compress chat history. Write a concise summary of the conversation below as short bullet points. Preserve facts, decisions, names, numbers, file names, code identifiers, user preferences, and open questions. Do not add commentary, do not answer the conversation, and do not mention that you are summarizing.";

/** Asks the chat model for a summary; throws when it fails or returns nothing. */
export async function summarize(options: {
  connection: Connection;
  model: Model;
  messages: Message[];
  previous?: string;
  context: number;
  signal: AbortSignal;
}) {
  const maxTokens = Math.min(2048, Math.floor(options.context / 4));
  // ~3 characters per token keeps the summarizer prompt within ~60% of context.
  const characters = Math.max(2000, Math.floor(options.context * 0.6 * 3));
  const previous = options.previous
    ? `Earlier summary:\n${middle(options.previous, Math.floor(characters / 4))}\n\n`
    : "";
  let content = "";
  await streamText(
    await api("/v1/chat/completions", options.connection, {
      method: "POST",
      signal: options.signal,
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: options.model.id,
        stream: true,
        temperature: 0.2,
        max_tokens: maxTokens,
        // Reasoning templates would otherwise spend the budget thinking.
        chat_template_kwargs: { enable_thinking: false },
        messages: [
          { role: "system", content: summaryPrompt },
          {
            role: "user",
            content: `${previous}Conversation:\n${transcript(options.messages, characters - previous.length)}`,
          },
        ],
      }),
    }),
    (text) => {
      content += text;
    },
  );
  const summary = stripThinking(content);
  if (!summary) throw new Error("The model returned an empty summary.");
  return summary.length > 12_000 ? `${summary.slice(0, 12_000)}…` : summary;
}

/**
 * Compacts `covered` into the conversation's summary. If summarization fails
 * (and was not cancelled), falls back to dropping those messages from context.
 */
export async function compact(options: {
  connection: Connection;
  model: Model;
  messages: Message[];
  compaction?: Compaction;
  covered: Message[];
  through: Message;
  context: number;
  signal: AbortSignal;
}): Promise<{ compaction: Compaction; error?: string }> {
  const previous =
    options.compaction &&
    options.messages.some(({ id }) => id === options.compaction?.through)
      ? options.compaction
      : undefined;
  const count = (previous?.count ?? 0) + options.covered.length;
  try {
    const summary = await summarize({
      ...options,
      messages: options.covered,
      previous: previous?.summary,
    });
    return {
      compaction: {
        through: options.through.id,
        summary,
        method: "summary",
        count,
      },
    };
  } catch (error) {
    options.signal.throwIfAborted();
    return {
      compaction: {
        through: options.through.id,
        // Keep the previous summary; only the newly covered turns are lost.
        summary: previous?.summary ?? "",
        method: "dropped",
        count,
      },
      error: error instanceof Error ? error.message : "Summarization failed.",
    };
  }
}
