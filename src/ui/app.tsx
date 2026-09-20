import { useEffect, useRef, useState, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import Markdown from "react-markdown";
import {
  api,
  availableModels,
  historyKey,
  imageResponseSchema,
  modelsSchema,
  readHistory,
  readinessSchema,
  streamText,
  transcriptionSchema,
  writeHistory,
  type Conversation,
  type Message,
  type Mode,
  type Model,
} from "./client";
import "./style.css";

const labels: Record<Mode, string> = {
  llm: "Chat",
  image: "Image",
  tts: "Speech",
  stt: "Transcribe",
};
const starters = [
  "Explain something simply",
  "Help me write a first draft",
  "Think through an idea",
];
function fresh(mode: Mode = "llm", model = ""): Conversation {
  return {
    id: crypto.randomUUID(),
    title: "New conversation",
    mode,
    model,
    messages: [],
  };
}
function Copy({ text }: { text: string | (() => string) }) {
  const [status, setStatus] = useState("Copy");
  return (
    <button
      className="copy"
      onClick={() => {
        if (!navigator.clipboard) {
          setStatus("Copy unavailable");
          return;
        }
        void navigator.clipboard
          .writeText(typeof text === "function" ? text() : text)
          .then(
            () => setStatus("Copied"),
            () => setStatus("Copy unavailable"),
          );
      }}
    >
      {status}
    </button>
  );
}
function CodeBlock({ children }: { children?: ReactNode }) {
  const ref = useRef<HTMLPreElement>(null);
  return (
    <div className="code-block">
      <Copy text={() => ref.current?.textContent ?? ""} />
      <pre ref={ref}>{children}</pre>
    </div>
  );
}
function Drawer({
  title,
  close,
  children,
}: {
  title: string;
  close: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    const previous = document.activeElement;
    dialog?.showModal();
    return () => {
      dialog?.close();
      if (previous instanceof HTMLElement) previous.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      aria-labelledby="drawer-title"
      onCancel={close}
      onClick={(e) => {
        if (e.target === e.currentTarget) close();
      }}
    >
      <div className="drawer">
        <header>
          <h2 id="drawer-title">{title}</h2>
          <button aria-label="Close drawer" onClick={close}>
            ✕
          </button>
        </header>
        {children}
      </div>
    </dialog>
  );
}
function App() {
  const [key, setKey] = useState("");
  const [models, setModels] = useState<Model[]>([]);
  const [connection, setConnection] = useState("Connecting");
  const [drawer, setDrawer] = useState<
    "settings" | "models" | "history" | null
  >(null);
  const [conversations, setConversations] = useState<Conversation[]>([fresh()]);
  const [activeId, setActiveId] = useState("");
  const [persistent, setPersistent] = useState(false);
  const [draft, setDraft] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [voice, setVoice] = useState("default");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const mediaUrls = useRef<string[]>([]);
  const bottom = useRef<HTMLDivElement>(null);
  const nearBottom = useRef(true);
  const active =
    conversations.find((c) => c.id === activeId) ?? conversations[0];
  const candidates = availableModels(models, active?.mode ?? "llm");
  const model = candidates.find((m) => m.id === active?.model) ?? candidates[0];
  const voices = model?.catalog.capabilities?.voice.requestValues ?? [
    "default",
  ];
  const selectedVoice = voices.includes(voice) ? voice : "default";
  const update = (id: string, change: (c: Conversation) => Conversation) =>
    setConversations((items) =>
      items.map((c) => (c.id === id ? change(c) : c)),
    );
  async function refresh(signal?: AbortSignal) {
    try {
      const [metadata, readiness] = await Promise.all([
        api("/_localbase/models", key, { signal })
          .then((r) => r.json())
          .then((v) => modelsSchema.parse(v)),
        fetch("/health/ready", { signal, cache: "no-store" })
          .then((r) => r.json())
          .then((v) => readinessSchema.parse(v)),
      ]);
      setModels(metadata.data);
      setConnection(
        readiness.status === "ready" ? "Gateway ready" : "Gateway not ready",
      );
    } catch (e) {
      if (!signal?.aborted) {
        setModels([]);
        setConnection(e instanceof Error ? e.message : "Unable to connect");
      }
    }
  }
  useEffect(() => {
    const abort = new AbortController();
    void refresh(abort.signal);
    const timer = setInterval(() => void refresh(abort.signal), 15000);
    return () => {
      abort.abort();
      clearInterval(timer);
    };
  }, [key]);
  useEffect(() => {
    try {
      const saved = readHistory();
      if (saved !== null) {
        setPersistent(true);
        if (saved.length) setConversations(saved);
      }
    } catch {
      setError(
        "Saved history could not be read. You can clear it in Settings.",
      );
    }
    return () => {
      controller.current?.abort();
      mediaUrls.current.forEach((url) => URL.revokeObjectURL(url));
    };
  }, []);
  useEffect(() => {
    if (persistent)
      try {
        writeHistory(conversations);
      } catch {
        setError(
          "Device storage is full or unavailable. History could not be saved.",
        );
      }
  }, [conversations, persistent]);
  useEffect(() => {
    if (nearBottom.current)
      bottom.current?.scrollIntoView({ behavior: "instant" });
  }, [active?.messages]);
  function newChat(mode: Mode = active?.mode ?? "llm") {
    if (busy) return;
    const c = fresh(mode);
    setConversations((items) => [c, ...items].slice(0, 30));
    setActiveId(c.id);
    setDraft("");
    setFile(null);
    setError("");
    setDrawer(null);
    nearBottom.current = true;
  }
  async function send(retry = false) {
    if (
      !active ||
      !model ||
      busy ||
      (!retry && active.mode !== "stt" && !draft.trim()) ||
      (active.mode === "stt" && !file)
    )
      return;
    const previousUser = active.messages.reduce(
      (last, m, index) => (m.role === "user" ? index : last),
      -1,
    );
    const base =
      retry && previousUser >= 0
        ? active.messages.slice(0, previousUser)
        : active.messages;
    const text =
      retry && previousUser >= 0
        ? (active.messages[previousUser]?.text ?? "")
        : draft.trim();
    const user: Message = {
      id: crypto.randomUUID(),
      role: "user",
      text: active.mode === "stt" ? (file?.name ?? "Audio file") : text,
    };
    const reply: Message = {
      id: crypto.randomUUID(),
      role: "assistant",
      text: "",
    };
    const abort = new AbortController();
    controller.current = abort;
    setBusy(true);
    setError("");
    setDraft("");
    nearBottom.current = true;
    update(active.id, (c) => ({
      ...c,
      model: model.id,
      title: user.text.slice(0, 60),
      messages: [...base, user, reply],
    }));
    const patch = (change: (m: Message) => Message) =>
      update(active.id, (c) => ({
        ...c,
        messages: c.messages.map((m) => (m.id === reply.id ? change(m) : m)),
      }));
    const json = (body: unknown): RequestInit => ({
      method: "POST",
      signal: abort.signal,
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    try {
      switch (active.mode) {
        case "llm":
          await streamText(
            await api(
              "/v1/chat/completions",
              key,
              json({
                model: model.id,
                stream: true,
                messages: [...base, user]
                  .filter((m) => m.text)
                  .map((m) => ({ role: m.role, content: m.text })),
              }),
            ),
            (text) => patch((m) => ({ ...m, text: m.text + text })),
          );
          break;
        case "image": {
          const result = imageResponseSchema.parse(
            await (
              await api(
                "/v1/images/generations",
                key,
                json({
                  model: model.id,
                  prompt: text,
                  n: 1,
                  size: "512x512",
                  response_format: "b64_json",
                }),
              )
            ).json(),
          );
          const image = result.data[0];
          if (image)
            patch((m) => ({
              ...m,
              text: "Generated image",
              media: {
                kind: "image",
                url: `data:image/png;base64,${image.b64_json}`,
              },
            }));
          break;
        }
        case "tts": {
          const response = await api(
            "/v1/audio/speech",
            key,
            json({
              model: model.id,
              input: text,
              voice: selectedVoice,
              response_format: "wav",
            }),
          );
          const url = URL.createObjectURL(await response.blob());
          mediaUrls.current.push(url);
          patch((m) => ({
            ...m,
            text: "Generated speech",
            media: { kind: "audio", url },
          }));
          break;
        }
        case "stt": {
          if (!file) throw new Error("Choose an audio file.");
          const body = new FormData();
          body.set("file", file);
          body.set("model", model.id);
          body.set("response_format", "json");
          const result = transcriptionSchema.parse(
            await (
              await api("/v1/audio/transcriptions", key, {
                method: "POST",
                body,
                signal: abort.signal,
              })
            ).json(),
          );
          patch((m) => ({ ...m, text: result.text }));
          break;
        }
      }
    } catch (e) {
      setError(
        abort.signal.aborted
          ? "Stopped. You can retry this request."
          : e instanceof Error
            ? e.message
            : "Request failed. Try again.",
      );
    } finally {
      setBusy(false);
      controller.current = null;
      void refresh();
    }
  }
  if (!active) return null;
  return (
    <div className="app">
      <header className="topbar">
        <button
          className="icon"
          aria-label="Open history"
          onClick={() => setDrawer("history")}
        >
          ☰
        </button>
        <div className="brand">
          <span className="brand-mark">L</span>LocalBase
        </div>
        <span className="local-label">PLAYGROUND</span>
        <button className="settings" onClick={() => setDrawer("settings")}>
          Settings
        </button>
      </header>
      <div className="workspace">
        <div className="toolbar">
          <button className="model-picker" onClick={() => setDrawer("models")}>
            <span
              className={`status-dot ${connection === "Gateway ready" ? "ready" : ""}`}
              aria-label={
                connection === "Gateway ready"
                  ? "Gateway ready"
                  : "Gateway not ready"
              }
            />
            <span>
              {model?.catalog.name ?? "Choose a model"}
              <small>
                {model
                  ? `${labels[active.mode]} · ${model.catalog.quantization}`
                  : "Connect your gateway"}
              </small>
            </span>
            <span>⌄</span>
          </button>
          <button
            disabled={busy}
            onClick={() => newChat()}
            aria-label="New conversation"
          >
            ＋ <span className="new-label">New</span>
          </button>
        </div>
        <main
          onScroll={(e) => {
            const el = e.currentTarget;
            nearBottom.current =
              el.scrollHeight - el.scrollTop - el.clientHeight < 100;
          }}
        >
          <div className="conversation">
            {!active.messages.length && (
              <section className="empty">
                <div className="empty-mark">
                  L<span>●</span>
                </div>
                <p className="eyebrow">{labels[active.mode]}</p>
                <h1>
                  {active.mode === "llm"
                    ? "What’s on your mind?"
                    : active.mode === "image"
                      ? "Describe your image"
                      : active.mode === "tts"
                        ? "Turn text into speech"
                        : "Turn audio into text"}
                </h1>
                <p>
                  {active.mode === "llm"
                    ? "Ask a question or start with an idea below."
                    : active.mode === "image"
                      ? "Write a prompt to generate an image."
                      : active.mode === "tts"
                        ? "Choose a voice and enter the words to read aloud."
                        : "Choose an audio file to transcribe."}
                </p>
                <div className="modes">
                  {(Object.keys(labels) as Mode[]).map((mode) => (
                    <button
                      key={mode}
                      className={active.mode === mode ? "selected" : ""}
                      disabled={busy}
                      onClick={() => newChat(mode)}
                    >
                      {labels[mode]}
                    </button>
                  ))}
                </div>
                {active.mode === "llm" && (
                  <div className="starters">
                    {starters.map((s) => (
                      <button key={s} onClick={() => setDraft(s)}>
                        {s}
                        <span>↗</span>
                      </button>
                    ))}
                  </div>
                )}
              </section>
            )}
            {active.messages.map((message) => (
              <article className={`message ${message.role}`} key={message.id}>
                <div className="message-label">
                  {message.role === "user" ? "You" : "LocalBase"}
                </div>
                <div className="message-body">
                  {message.text ? (
                    <Markdown
                      skipHtml
                      components={{
                        pre: CodeBlock,
                        img: () => null,
                        a: ({ children, href }) => (
                          <a
                            href={href}
                            target="_blank"
                            rel="noreferrer noopener"
                          >
                            {children}
                          </a>
                        ),
                      }}
                    >
                      {message.text}
                    </Markdown>
                  ) : busy ? (
                    <p className="thinking">
                      Working<span>…</span>
                    </p>
                  ) : (
                    <p className="muted">No response</p>
                  )}
                  {message.media?.kind === "image" && (
                    <img
                      className="generated-image"
                      src={message.media.url}
                      alt="Generated from your prompt"
                    />
                  )}
                  {message.media?.kind === "audio" && (
                    <audio controls src={message.media.url} />
                  )}
                </div>
                {message.role === "assistant" && message.text && (
                  <Copy text={message.text} />
                )}
              </article>
            ))}
            <div ref={bottom} />
          </div>
        </main>
        <footer className="composer-area">
          {error && (
            <div className="error" role="alert">
              <span>{error}</span>
              {active.messages.length > 0 && (
                <button
                  disabled={busy || !model || (active.mode === "stt" && !file)}
                  onClick={() => void send(true)}
                >
                  Retry
                </button>
              )}
              <button aria-label="Dismiss error" onClick={() => setError("")}>
                ✕
              </button>
            </div>
          )}
          {!model && (
            <p className="notice">
              {models.length
                ? `No selected, installed ${labels[active.mode].toLowerCase()} model. Configure one with the LocalBase CLI.`
                : connection}{" "}
              <button onClick={() => setDrawer("settings")}>Settings</button>
            </p>
          )}
          <form
            className="composer"
            onSubmit={(e) => {
              e.preventDefault();
              void send();
            }}
          >
            {active.mode === "stt" ? (
              <label className="upload">
                Audio file
                <input
                  type="file"
                  accept="audio/*,.wav,.mp3,.m4a,.ogg,.flac"
                  disabled={busy}
                  onChange={(e) => setFile(e.currentTarget.files?.[0] ?? null)}
                />
              </label>
            ) : (
              <textarea
                aria-label={
                  active.mode === "image" ? "Describe an image" : "Message"
                }
                placeholder={
                  active.mode === "image"
                    ? "Describe an image…"
                    : active.mode === "tts"
                      ? "Write something to read aloud…"
                      : "Message LocalBase…"
                }
                value={draft}
                maxLength={active.mode === "tts" ? 256 : undefined}
                disabled={busy}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (
                    e.key === "Enter" &&
                    !e.shiftKey &&
                    !e.nativeEvent.isComposing &&
                    matchMedia("(pointer: fine)").matches
                  ) {
                    e.preventDefault();
                    void send();
                  }
                }}
              />
            )}
            <div className="composer-bottom">
              <span>
                {labels[active.mode]}
                {active.mode === "tts" && ` · ${draft.length}/256`}
                {active.mode === "tts" && (
                  <select
                    aria-label="Voice"
                    value={selectedVoice}
                    disabled={busy}
                    onChange={(e) => setVoice(e.target.value)}
                  >
                    {voices.map((v) => (
                      <option key={v}>{v}</option>
                    ))}
                  </select>
                )}
              </span>
              {busy ? (
                <button
                  type="button"
                  className="send"
                  onClick={() => controller.current?.abort()}
                >
                  Stop ■
                </button>
              ) : (
                <button
                  className="send"
                  type="submit"
                  disabled={
                    !model || (active.mode === "stt" ? !file : !draft.trim())
                  }
                  aria-label="Send request"
                >
                  Send ↑
                </button>
              )}
            </div>
          </form>
          <p className="privacy">
            {persistent
              ? "Device-local text history is on"
              : "History stays in this session"}
            <span>•</span>Check important answers
          </p>
        </footer>
      </div>
      {drawer && (
        <Drawer
          title={
            drawer === "settings"
              ? "Settings"
              : drawer === "models"
                ? "Models"
                : "History"
          }
          close={() => setDrawer(null)}
        >
          {drawer === "settings" ? (
            <>
              <p className="muted">{connection}</p>
              <label>
                Gateway API key
                <input
                  type="password"
                  autoComplete="off"
                  spellCheck={false}
                  value={key}
                  placeholder="Paste your API key"
                  onChange={(e) => setKey(e.target.value)}
                />
              </label>
              <p className="hint">
                Kept in memory for this page only. Sent only to this gateway.
              </p>
              <button onClick={() => void refresh()}>Refresh connection</button>
              <hr />
              <label className="toggle">
                <input
                  type="checkbox"
                  checked={persistent}
                  onChange={(e) => {
                    const enabled = e.target.checked;
                    try {
                      if (!enabled) localStorage.removeItem(historyKey);
                      else writeHistory(conversations);
                      setPersistent(enabled);
                    } catch {
                      setError("Device storage is unavailable.");
                    }
                  }}
                />
                Save text history on this device
              </label>
              <p className="hint">
                Optional browser storage, visible to others using this browser.
                API keys and generated media are never saved. Turning this off
                removes saved history.
              </p>
              <button
                className="danger"
                disabled={busy}
                onClick={() => {
                  setPersistent(false);
                  mediaUrls.current.forEach((url) => URL.revokeObjectURL(url));
                  mediaUrls.current = [];
                  const c = fresh();
                  setConversations([c]);
                  setActiveId(c.id);
                  setDraft("");
                  setFile(null);
                  setError("");
                  try {
                    localStorage.removeItem(historyKey);
                  } catch {
                    setError(
                      "Session history cleared. Saved device history could not be removed; clear this site's browser data to remove it.",
                    );
                  }
                }}
              >
                Clear all history
              </button>
            </>
          ) : drawer === "models" ? (
            <>
              <p className="hint">
                Selected, installed models on this gateway. Choosing a model may
                load it on your next request.
              </p>
              <div className="modes">
                {(Object.keys(labels) as Mode[]).map((mode) => (
                  <button
                    disabled={busy}
                    className={active.mode === mode ? "selected" : ""}
                    key={mode}
                    onClick={() => {
                      newChat(mode);
                      setDrawer("models");
                    }}
                  >
                    {labels[mode]}
                  </button>
                ))}
              </div>
              {candidates.length ? (
                candidates.map((m) => (
                  <button
                    disabled={busy}
                    className={`model-card ${m.id === model?.id ? "selected" : ""}`}
                    key={m.id}
                    onClick={() => {
                      update(active.id, (c) => ({ ...c, model: m.id }));
                      setDrawer(null);
                    }}
                  >
                    <strong>{m.catalog.name}</strong>
                    <span>
                      {m.catalog.quantization} ·{" "}
                      {m.device.runtime?.state ?? "Loads on request"}
                    </span>
                    <small>
                      {m.catalog.inputModalities.join(", ")} →{" "}
                      {m.catalog.outputModalities.join(", ")}
                      {m.catalog.contextWindowTokens
                        ? ` · ${m.catalog.contextWindowTokens.toLocaleString()} context`
                        : ""}
                    </small>
                  </button>
                ))
              ) : (
                <p>
                  No selected, installed models for{" "}
                  {labels[active.mode].toLowerCase()}.
                </p>
              )}
            </>
          ) : (
            <>
              <button disabled={busy} onClick={() => newChat()}>
                ＋ New conversation
              </button>
              <p className="hint">
                {persistent
                  ? "Text is saved on this device. Media lasts for this session."
                  : "Conversations disappear when this page closes or reloads."}
              </p>
              {conversations.map((c) => (
                <button
                  disabled={busy}
                  className="history-item"
                  key={c.id}
                  onClick={() => {
                    setActiveId(c.id);
                    setDraft("");
                    setFile(null);
                    setError("");
                    setDrawer(null);
                    nearBottom.current = true;
                  }}
                >
                  <strong>{c.title}</strong>
                  <small>
                    {labels[c.mode]} · {c.messages.length} messages
                  </small>
                </button>
              ))}
            </>
          )}
        </Drawer>
      )}
    </div>
  );
}
const root = document.getElementById("root");
if (root) createRoot(root).render(<App />);
