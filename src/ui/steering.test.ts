import { describe, expect, test } from "bun:test";
import type { Message } from "./client";
import { canSteer, interruptReply, phaseLabel } from "./steering";

describe("steering", () => {
  test("canSteer only while a chat reply runs and the draft has text", () => {
    const base = { busy: true, mode: "llm" as const, draft: "change course" };
    expect(canSteer(base)).toBe(true);
    expect(canSteer({ ...base, busy: false })).toBe(false);
    expect(canSteer({ ...base, draft: "   " })).toBe(false);
    expect(canSteer({ ...base, mode: "image" })).toBe(false);
    expect(canSteer({ ...base, retry: true })).toBe(false);
    expect(canSteer({ ...base, studio: true })).toBe(false);
  });

  test("interruptReply keeps text, drops protocol, and settles tools", () => {
    const message: Message = {
      id: "a",
      role: "assistant",
      text: "Partial",
      reasoning: "thinking",
      protocol: [{ role: "assistant", content: "Partial" }],
      artifacts: [
        { id: "1", label: "Image", state: "working", detail: "Generating" },
        {
          id: "2",
          label: "Speech",
          state: "error",
          detail: "Failed",
        },
      ],
    };
    const result = interruptReply(message, "Interrupted");
    expect(result.text).toBe("Partial");
    expect(result.reasoning).toBe("thinking");
    expect(result.interrupted).toBe(true);
    expect(result.protocol).toBeUndefined();
    expect(result.artifacts).toEqual([
      { id: "1", label: "Image", state: "error", detail: "Interrupted" },
      { id: "2", label: "Speech", state: "error", detail: "Failed" },
    ]);
    expect(interruptReply(result, "Stopped")).toEqual(result);
  });

  test("phaseLabel shows elapsed whole seconds", () => {
    expect(phaseLabel("waiting", 0.4)).toBe("Waiting for the model…");
    expect(phaseLabel("thinking", 12.9)).toBe("Thinking… 12s");
    expect(phaseLabel("compacting", 3)).toBe("Compacting earlier messages… 3s");
  });
});
