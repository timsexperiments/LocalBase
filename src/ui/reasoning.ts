/**
 * Reasoning ("thinking") stream handling. Reasoning models served by
 * llama-server stream their chain of thought in `delta.reasoning_content`
 * (the gateway normalizes it to `delta.reasoning`), or inline as
 * `<think>…</think>` inside `delta.content` when no reasoning parser is active.
 * Reasoning is shown to the user but never sent back to the model.
 */

/** Reasoning text carried by a streamed delta, preferring `reasoning`. */
export function deltaReasoning(delta: {
  reasoning?: string | null;
  reasoning_content?: string | null;
}) {
  return delta.reasoning || delta.reasoning_content || "";
}

const open = "<think>";
const close = "</think>";

/** Length of the longest suffix of `text` that is a prefix of `tag`. */
function partialTag(text: string, tag: string) {
  for (let length = Math.min(tag.length - 1, text.length); length > 0; length--)
    if (text.endsWith(tag.slice(0, length))) return length;
  return 0;
}

/**
 * Splits streamed content into answer text and inline `<think>` reasoning.
 * Tags may arrive split across chunks; a possible partial tag is held back
 * until the next chunk (or `flush`) resolves it.
 */
export function thinkSplitter() {
  let thinking = false;
  let pending = "";
  let started = false;
  function push(chunk: string) {
    let text = pending + chunk;
    pending = "";
    let content = "";
    let reasoning = "";
    while (text) {
      if (!thinking) {
        // Only a leading <think> (after optional whitespace) opens reasoning,
        // so answers that merely mention the tag are left alone.
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
    return thinking
      ? { content: "", reasoning: rest }
      : { content: rest, reasoning: "" };
  }
  return { push, flush };
}

/** Removes `<think>` blocks (closed or trailing unclosed) from final text. */
export function stripThinking(text: string) {
  return text
    .replace(/<think>[\s\S]*?<\/think>/g, "")
    .replace(/<think>[\s\S]*$/, "")
    .trim();
}
