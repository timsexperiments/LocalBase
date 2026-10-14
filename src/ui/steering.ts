/**
 * Steering: sending a message while a reply streams interrupts that reply,
 * keeps its partial text (marked interrupted), and starts a new turn.
 */
import type { Message, Mode } from "./client";

/** Abort reason for a reply interrupted by a steering message. */
export const steerAbortReason = Symbol("steer");

export type Phase = "waiting" | "thinking" | "compacting";

export function phaseLabel(phase: Phase, seconds: number) {
  const label =
    phase === "compacting"
      ? "Compacting earlier messages"
      : phase === "thinking"
        ? "Thinking"
        : "Waiting for the model";
  return seconds >= 1 ? `${label}… ${Math.floor(seconds)}s` : `${label}…`;
}

/** Whether a send while busy should interrupt the running chat reply. */
export function canSteer(options: {
  busy: boolean;
  mode: Mode;
  draft: string;
  retry?: boolean;
  studio?: boolean;
}) {
  return (
    options.busy &&
    options.mode === "llm" &&
    !options.retry &&
    !options.studio &&
    !!options.draft.trim()
  );
}

/**
 * Marks a cut-short reply: its partial text is resent as context (without
 * reasoning or half-finished tool protocol) and running tools show `detail`.
 */
export function interruptReply(message: Message, detail: string): Message {
  return {
    ...message,
    protocol: undefined,
    interrupted: true,
    artifacts: message.artifacts?.map((artifact) =>
      artifact.state === "working"
        ? {
            id: artifact.id,
            label: artifact.label,
            state: "error",
            detail,
          }
        : artifact,
    ),
  };
}
