import { useEffect, useState } from "react";
import type { Compaction } from "./client";
import { phaseLabel, type Phase } from "./steering";

/** Live status while a reply has no text yet, with elapsed seconds. */
export function WorkingStatus({
  phase,
  since,
}: {
  phase: Phase;
  since: number;
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  return (
    <p className="thinking" role="status">
      {phaseLabel(phase, Math.max(0, (now - since) / 1000))}
    </p>
  );
}

/**
 * Collapsible model reasoning. Open while the model is only thinking, then
 * collapses once the answer starts; the user's toggle always wins.
 */
export function ReasoningBlock({
  text,
  live,
  answering,
}: {
  text: string;
  live: boolean;
  answering: boolean;
}) {
  const [chosen, setChosen] = useState<boolean | null>(null);
  const open = chosen ?? (live && !answering);
  return (
    <details className="reasoning" open={open}>
      <summary
        onClick={(event) => {
          event.preventDefault();
          setChosen(!open);
        }}
      >
        {live && !answering ? "Thinking…" : "Thoughts"}
      </summary>
      <div className="reasoning-text">{text.trim()}</div>
    </details>
  );
}

/** Divider after the last message folded into the conversation summary. */
export function CompactionDivider({ compaction }: { compaction: Compaction }) {
  const count = `${compaction.count} earlier message${compaction.count === 1 ? "" : "s"}`;
  return (
    <details className="compaction-divider">
      <summary>
        <span>
          {compaction.method === "summary"
            ? `Earlier messages compacted · ${count}`
            : `Earlier messages dropped from context · ${count}`}
        </span>
      </summary>
      <div className="compaction-summary">
        {compaction.method === "dropped" && (
          <p className="muted">
            Summarization failed, so these messages are no longer sent to the
            model. They are still shown above.
          </p>
        )}
        {compaction.summary && (
          <p className="compaction-text">{compaction.summary}</p>
        )}
        {compaction.method === "summary" && (
          <p className="muted">
            The model sees this summary instead of the messages above.
          </p>
        )}
      </div>
    </details>
  );
}
