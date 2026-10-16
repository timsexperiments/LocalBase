import { useEffect, useRef, useState, type FormEvent } from "react";
import { z } from "zod";
import { api, type Connection } from "./client";

const checkSchema = z.discriminatedUnion("state", [
  z.object({
    state: z.literal("valid"),
    name: z.string().nullable(),
    role: z.string().nullable(),
  }),
  z.object({ state: z.literal("invalid") }),
  z.object({ state: z.literal("unreachable"), message: z.string() }),
]);
const statusSchema = z.object({
  configured: z.boolean(),
  source: z.enum(["saved", "env"]).nullable(),
  envAvailable: z.boolean(),
  check: checkSchema.optional(),
});
type Status = z.infer<typeof statusSchema>;
type Check = z.infer<typeof checkSchema>;

const sourceBadges = {
  saved: "Saved",
  env: "From HF_TOKEN",
} as const;

function describeCheck(check: Check) {
  if (check.state === "invalid")
    return "Hugging Face rejected this token. Save a fresh one.";
  if (check.state === "unreachable") return check.message;
  const who = check.name ? ` for ${check.name}` : "";
  const role = check.role ? ` (${check.role} access)` : "";
  return `Hugging Face accepted the token${who}${role}.`;
}

async function tokenRequest(
  connection: Connection,
  init?: { body: Record<string, unknown> },
) {
  const response = await api(
    "/_localbase/hf-token",
    connection,
    init
      ? {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify(init.body),
        }
      : {},
  );
  return statusSchema.parse(await response.json());
}

/**
 * Saves, replaces, checks and clears the Hugging Face token. The gateway
 * only ever answers whether a token is set and where it comes from.
 */
export function HfTokenPanel({
  connection,
  open,
  onToggle,
  focusKey,
}: {
  connection: Connection;
  open: boolean;
  onToggle: (open: boolean) => void;
  /** Changes when another part of the page asks to focus the token field. */
  focusKey: number;
}) {
  const [status, setStatus] = useState<Status | null>(null);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState<"" | "save" | "check" | "clear">("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const panel = useRef<HTMLDetailsElement>(null);
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    let live = true;
    tokenRequest(connection)
      .then((next) => live && setStatus(next))
      .catch(
        (error: unknown) =>
          live &&
          setError(
            error instanceof Error
              ? error.message
              : "Could not read the token status.",
          ),
      );
    return () => {
      live = false;
    };
  }, [connection]);

  useEffect(() => {
    if (!focusKey) return;
    panel.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
    input.current?.focus({ preventScroll: true });
  }, [focusKey]);

  async function run(
    action: "save" | "check" | "clear",
    body: Record<string, unknown>,
  ) {
    setBusy(action);
    setError("");
    setMessage("");
    try {
      const next = await tokenRequest(connection, { body });
      setStatus(next);
      if (action === "save") setToken("");
      setMessage(
        next.check
          ? describeCheck(next.check)
          : action === "clear"
            ? next.source === "env"
              ? "Cleared the saved token; LocalBase now uses HF_TOKEN."
              : "Cleared the saved token."
            : "Saved the token.",
      );
    } catch (error) {
      setError(
        error instanceof Error ? error.message : "Could not update the token.",
      );
    } finally {
      setBusy("");
    }
  }

  function save(event: FormEvent) {
    event.preventDefault();
    if (!token.trim()) return;
    void run("save", { action: "save", token: token.trim() });
  }

  const source = status?.source ?? null;
  return (
    <details
      id="hf-token"
      ref={panel}
      className="hf-token-panel"
      open={open}
      onToggle={(event) => onToggle(event.currentTarget.open)}
    >
      <summary>
        <span className="hf-token-title">Hugging Face token</span>
        {status && (
          <span
            className={`model-badge hf-token-state ${source ? `hf-token-${source}` : "hf-token-unset"}`}
          >
            {source ? sourceBadges[source] : "Not set"}
          </span>
        )}
      </summary>
      <div className="hf-token-body">
        <p className="hint">
          A token lets LocalBase download gated repositories you have accepted
          and raises the Hub rate limit. A{" "}
          <a
            href="https://huggingface.co/settings/tokens"
            target="_blank"
            rel="noopener noreferrer"
          >
            read token
          </a>{" "}
          is enough. A saved token takes precedence over HF_TOKEN and is never
          shown again.
        </p>
        <form className="hf-token-form" onSubmit={save}>
          <label>
            <span>{source === "saved" ? "Replace token" : "Token"}</span>
            <input
              ref={input}
              type="password"
              name="hf-token"
              autoComplete="off"
              autoCapitalize="off"
              spellCheck={false}
              placeholder="hf_…"
              maxLength={256}
              value={token}
              disabled={!!busy}
              onChange={(event) => setToken(event.target.value)}
            />
          </label>
          <div className="hf-token-actions">
            <button
              type="submit"
              className="primary-action"
              disabled={!!busy || !token.trim()}
            >
              {busy === "save"
                ? "Checking…"
                : source === "saved"
                  ? "Replace"
                  : "Save"}
            </button>
            <button
              type="button"
              disabled={!!busy || !status?.configured}
              onClick={() => void run("check", { action: "validate" })}
            >
              {busy === "check" ? "Checking…" : "Check token"}
            </button>
            {source === "saved" && (
              <button
                type="button"
                className="danger"
                disabled={!!busy}
                onClick={() => void run("clear", { action: "clear" })}
              >
                {busy === "clear" ? "Clearing…" : "Clear saved token"}
              </button>
            )}
          </div>
        </form>
        {source === "saved" && status?.envAvailable && (
          <p className="notice">Clearing it falls back to HF_TOKEN.</p>
        )}
        {message && (
          <p className="notice" role="status">
            {message}
          </p>
        )}
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
      </div>
    </details>
  );
}
