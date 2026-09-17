import { useEffect, useRef, useState, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import Markdown from "react-markdown";
import { z } from "zod";
import { generationTools, generateVideo, runChat } from "./tools";
import {
  api,
  consumeFragmentKey,
  createUiId,
  availableModels,
  historyKey,
  imageResponseSchema,
  modelsSchema,
  modes,
  readHistory,
  readinessSchema,
  readSession,
  sessionConnection,
  SessionRequiredError,
  type SessionState,
  transcriptionSchema,
  writeHistory,
  type Conversation,
  type Message,
  type Mode,
  type Model,
  type Media,
  type ChatMessage,
} from "./client";
import "./style.css";

const labels: Record<Mode, string> = {
  llm: "Chat",
  image: "Image",
  tts: "Speech",
  stt: "Transcribe",
  video: "Video",
  embedding: "Embeddings",
};
const starters = [
  "Explain something simply",
  "Help me write a first draft",
  "Think through an idea",
];
function fresh(
  mode: Mode = "llm",
  workspace: Conversation["workspace"] = "chat",
): Conversation {
  return {
    id: createUiId(),
    title: "New conversation",
    mode,
    workspace,
    model: "",
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
function MediaCard({ media }: { media: Media }) {
  const [playbackFailed, setPlaybackFailed] = useState(false);
  return (
    <div className="artifact">
      {media.kind === "image" && (
        <img
          className="generated-image"
          src={media.url}
          alt="Generated from your prompt"
        />
      )}
      {media.kind === "audio" && <audio controls src={media.url} />}
      {media.kind === "video" && (
        <>
          <video
            controls
            playsInline
            preload="metadata"
            src={media.url}
            aria-label="Generated video"
            onError={() => setPlaybackFailed(true)}
            onLoadedMetadata={() => setPlaybackFailed(false)}
          />
          {playbackFailed && (
            <p role="status">
              This browser could not play the video. Download the MP4 to play it
              in another player.
            </p>
          )}
        </>
      )}
      <a
        className="download"
        href={media.url}
        download={`localbase.${media.kind === "image" ? "png" : media.kind === "audio" ? "wav" : media.format}`}
      >
        Download {media.kind}
      </a>
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
  const [key, setKey] = useState(() => consumeFragmentKey());
  const [session, setSession] = useState<SessionState>({ kind: "checking" });
  const credential = sessionConnection(session, key);
  const [models, setModels] = useState<Model[]>([]);
  const [connection, setConnection] = useState("Connecting");
  const connectionLabel =
    session.kind === "error"
      ? session.message
      : session.kind === "checking"
        ? "Checking sign-in"
        : !credential
          ? "Enter a gateway API key in Settings."
          : connection;
  const [drawer, setDrawer] = useState<
    "settings" | "models" | "history" | null
  >(null);
  const [conversations, setConversations] = useState<Conversation[]>([fresh()]);
  const [activeId, setActiveId] = useState("");
  const [persistent, setPersistent] = useState(false);
  const [draft, setDraft] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [voice, setVoice] = useState("default");
  const [dimensions, setDimensions] = useState(0);
  const [error, setError] = useState("");
  const [warnings, setWarnings] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const mediaUrls = useRef<string[]>([]);
  const bottom = useRef<HTMLDivElement>(null);
  const nearBottom = useRef(true);
  const active =
    conversations.find((c) => c.id === activeId) ?? conversations[0];
  const page = active?.workspace ?? "chat";
  const candidates = availableModels(models, active?.mode ?? "llm");
  const model = candidates.find((m) => m.id === active?.model) ?? candidates[0];
  const capabilities = model?.catalog.capabilities;
  const voices: string[] =
    capabilities?.kind === "speech"
      ? capabilities.voice.requestValues
      : ["default"];
  const selectedVoice = voices.includes(voice) ? voice : "default";
  const selectedDimensions =
    capabilities?.kind === "embedding"
      ? Math.max(
          capabilities.dimensions.minimum,
          Math.min(
            dimensions || capabilities.dimensions.maximum,
            capabilities.dimensions.maximum,
          ),
        )
      : 0;
  const unsupportedVideo =
    active?.mode === "video" &&
    capabilities?.kind === "video" &&
    capabilities.mode === "s2v";
  const update = (id: string, change: (c: Conversation) => Conversation) =>
    setConversations((items) =>
      items.map((c) => (c.id === id ? change(c) : c)),
    );
  async function checkSession(signal?: AbortSignal) {
    try {
      const verified = await readSession(signal);
      signal?.throwIfAborted();
      setSession(verified);
      if (verified.kind === "session") setKey("");
    } catch (e) {
      if (!signal?.aborted) {
        setModels([]);
        setSession({
          kind: "error",
          message:
            e instanceof SessionRequiredError
              ? e.message
              : "Sign-in could not be verified. Reload this page to sign in, or refresh to try again.",
        });
      }
    }
  }
  async function refresh(signal?: AbortSignal) {
    if (!credential) return;
    try {
      const [metadata, readiness] = await Promise.all([
        api("/_localbase/models", credential, { signal })
          .then((r) => r.json())
          .then((v) => modelsSchema.parse(v)),
        fetch("/health/ready", { signal, cache: "no-store" })
          .then((r) => r.json())
          .then((v) => readinessSchema.parse(v)),
      ]);
      signal?.throwIfAborted();
      setModels(metadata.data);
      setConnection(
        readiness.status === "ready" ? "Gateway ready" : "Gateway not ready",
      );
    } catch (e) {
      if (!signal?.aborted) {
        setModels([]);
        if (e instanceof SessionRequiredError)
          setSession({ kind: "error", message: e.message });
        else
          setConnection(e instanceof Error ? e.message : "Unable to connect");
      }
    }
  }
  useEffect(() => {
    const abort = new AbortController();
    void checkSession(abort.signal);
    return () => abort.abort();
  }, []);
  useEffect(() => {
    if (!credential) {
      setModels([]);
      return;
    }
    const abort = new AbortController();
    void refresh(abort.signal);
    const timer = setInterval(() => void refresh(abort.signal), 15000);
    return () => {
      abort.abort();
      clearInterval(timer);
    };
  }, [session, key]);
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
  function newChat(mode: Mode = active?.mode ?? "llm", workspace = page) {
    if (busy) return;
    const c = fresh(mode, workspace);
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
      !credential ||
      !model ||
      busy ||
      unsupportedVideo ||
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
      id: createUiId(),
      role: "user",
      text: active.mode === "stt" ? (file?.name ?? "Audio file") : text,
    };
    const reply: Message = {
      id: createUiId(),
      role: "assistant",
      text: "",
    };
    const abort = new AbortController();
    controller.current = abort;
    setBusy(true);
    setWarnings([]);
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
    let authenticationFailed = false;
    try {
      switch (active.mode) {
        case "llm": {
          const messages = [...base, user].flatMap(
            (m): ChatMessage[] =>
              m.protocol ?? (m.text ? [{ role: m.role, content: m.text }] : []),
          );
          const protocol = await runChat({
            model,
            models,
            connection: credential,
            signal: abort.signal,
            messages,
            toolsEnabled: page === "chat",
            append: (text) => patch((m) => ({ ...m, text: m.text + text })),
            artifact: (artifact) =>
              patch((m) => ({
                ...m,
                artifacts: [
                  ...(m.artifacts ?? []).filter(
                    (item) => item.id !== artifact.id,
                  ),
                  artifact,
                ],
              })),
            register: (url) => mediaUrls.current.push(url),
            warning: (detail) => setWarnings((items) => [...items, detail]),
          });
          patch((m) => ({ ...m, protocol }));
          break;
        }
        case "video": {
          const id = createUiId();
          const video = await generateVideo({
            model,
            connection: credential,
            signal: abort.signal,
            prompt: text,
            warning: (detail) => setWarnings((items) => [...items, detail]),
            progress: (detail) =>
              patch((m) => ({
                ...m,
                artifacts: [{ id, label: "Video", state: "working", detail }],
              })),
          });
          abort.signal.throwIfAborted();
          const url = URL.createObjectURL(video.blob);
          mediaUrls.current.push(url);
          patch((m) => ({
            ...m,
            text: "Generated video",
            artifacts: [
              {
                id,
                label: "Video",
                state: "complete",
                media: { kind: "video", url, format: video.format },
              },
            ],
          }));
          break;
        }
        case "embedding": {
          const result = z
            .object({
              data: z.array(
                z.object({ embedding: z.array(z.number()), index: z.number() }),
              ),
            })
            .parse(
              await (
                await api(
                  "/v1/embeddings",
                  credential,
                  json({
                    model: model.id,
                    input: text,
                    dimensions: selectedDimensions,
                    encoding_format: "float",
                  }),
                )
              ).json(),
            );
          patch((m) => ({
            ...m,
            text: `\`\`\`json\n${JSON.stringify(result, null, 2)}\n\`\`\``,
          }));
          break;
        }
        case "image": {
          const result = imageResponseSchema.parse(
            await (
              await api(
                "/v1/images/generations",
                credential,
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
            credential,
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
              await api("/v1/audio/transcriptions", credential, {
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
      if (e instanceof SessionRequiredError) {
        authenticationFailed = true;
        setModels([]);
        setSession({ kind: "error", message: e.message });
      }
      patch((m) => ({
        ...m,
        protocol: [],
        artifacts: m.artifacts?.map((artifact) =>
          artifact.state === "working"
            ? {
                id: artifact.id,
                label: artifact.label,
                state: "error",
                detail: abort.signal.aborted
                  ? "Stopped"
                  : e instanceof Error
                    ? e.message
                    : "Request failed",
              }
            : artifact,
        ),
      }));
      setError(
        e instanceof SessionRequiredError
          ? ""
          : abort.signal.aborted
            ? "Stopped. You can retry this request."
            : e instanceof Error
              ? e.message
              : "Request failed. Try again.",
      );
    } finally {
      setBusy(false);
      controller.current = null;
      if (!authenticationFailed) void refresh();
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
        <a className="brand" href="/app">
          <span className="brand-mark">L</span>LocalBase
        </a>
        <span className="local-label">PLAYGROUND</span>
        <button className="settings" onClick={() => setDrawer("settings")}>
          Settings
        </button>
      </header>
      <div className="workspace">
        <nav className="primary-nav" aria-label="Workspace">
          <button
            disabled={busy}
            aria-current={page === "chat" ? "page" : undefined}
            onClick={() => {
              newChat("llm", "chat");
            }}
          >
            Chat
          </button>
          <button
            disabled={busy}
            aria-current={page === "lab" ? "page" : undefined}
            onClick={() => {
              newChat("llm", "lab");
            }}
          >
            Model Lab
          </button>
        </nav>
        {page === "lab" && (
          <div className="modes lab-modes" aria-label="Direct API mode">
            {modes.map((mode) => (
              <button
                key={mode}
                disabled={busy}
                className={active.mode === mode ? "selected" : ""}
                onClick={() => newChat(mode)}
              >
                {labels[mode]}
              </button>
            ))}
          </div>
        )}
        <div className="toolbar">
          <button className="model-picker" onClick={() => setDrawer("models")}>
            <span
              className={`status-dot ${connectionLabel === "Gateway ready" ? "ready" : ""}`}
              aria-label={connectionLabel}
            />
            <span>
              {model?.catalog.name ?? "Choose a model"}
              <small>
                {model
                  ? `${labels[active.mode]} · ${model.catalog.quantization}`
                  : connectionLabel}
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
                    ? page === "chat"
                      ? "What’s on your mind?"
                      : "Try a model directly"
                    : active.mode === "video"
                      ? "Describe your video"
                      : active.mode === "embedding"
                        ? "Inspect text embeddings"
                        : active.mode === "image"
                          ? "Describe your image"
                          : active.mode === "tts"
                            ? "Turn text into speech"
                            : "Turn audio into text"}
                </h1>
                <p>
                  {active.mode === "llm"
                    ? page === "chat"
                      ? "Ask a question, or ask for an image, video, or speech using available tools."
                      : "Direct chat completion. Generation tools are disabled in Model Lab."
                    : active.mode === "video"
                      ? "Generate a clip using the model’s qualified video profile."
                      : active.mode === "embedding"
                        ? "Enter text to view its embedding vector."
                        : active.mode === "image"
                          ? "Write a prompt to generate an image."
                          : active.mode === "tts"
                            ? "Choose a voice and enter the words to read aloud."
                            : "Choose an audio file to transcribe."}
                </p>
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
                  ) : message.artifacts?.length ? null : busy ? (
                    <p className="thinking">
                      Working<span>…</span>
                    </p>
                  ) : (
                    <p className="muted">No response</p>
                  )}
                  {message.media && <MediaCard media={message.media} />}
                  {message.artifacts?.map((artifact) => (
                    <section
                      className="artifact"
                      key={artifact.id}
                      aria-live="polite"
                    >
                      <strong>{artifact.label.replaceAll("_", " ")}</strong>
                      {artifact.state === "complete" ? (
                        <MediaCard media={artifact.media} />
                      ) : (
                        <p
                          role={artifact.state === "error" ? "alert" : "status"}
                        >
                          {artifact.detail}
                        </p>
                      )}
                    </section>
                  ))}
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
          {session.kind === "error" && (
            <div className="error session-notice" role="alert">
              <span>{session.message}</span>
              <a className="download" href="/app">
                Sign in again
              </a>
              <button disabled={busy} onClick={() => void checkSession()}>
                Refresh sign-in
              </button>
            </div>
          )}
          {warnings.length > 0 && (
            <div className="error" role="alert">
              <span>{warnings.join(" ")}</span>
              <button
                aria-label="Dismiss cleanup warnings"
                onClick={() => setWarnings([])}
              >
                ✕
              </button>
            </div>
          )}
          {page === "chat" && model && (
            <p className="notice">
              {generationTools(models, model).length
                ? `Available tools: ${generationTools(models, model)
                    .map((tool) => tool.function.name.replaceAll("_", " "))
                    .join(", ")}`
                : "Text chat only. Generation tools require a tool-calling chat model and selected, installed media models."}
            </p>
          )}
          {active.mode === "video" && (
            <p className="notice">
              {capabilities?.kind === "video"
                ? `${capabilities.mode.toUpperCase()} · ${capabilities.width} × ${capabilities.height} · ${capabilities.frames} frames · ${capabilities.fps} fps · MP4 delivery. Profile fixed by model qualification.`
                : "No qualified video profile available."}{" "}
              {unsupportedVideo &&
                "Speech-to-video portrait and audio inputs are not supported in Model Lab yet. Select a text-to-video model."}
            </p>
          )}
          {active.mode === "image" && (
            <p className="notice">PNG · 512 × 512 · one image per request</p>
          )}
          {error && (
            <div className="error" role="alert">
              <span>{error}</span>
              {active.messages.length > 0 && (
                <button
                  disabled={
                    busy ||
                    !credential ||
                    !model ||
                    unsupportedVideo ||
                    (active.mode === "stt" && !file)
                  }
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
                : connectionLabel}{" "}
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
                {capabilities?.kind === "embedding" && (
                  <label>
                    {" "}
                    Dimensions{" "}
                    <input
                      type="number"
                      min={capabilities.dimensions.minimum}
                      max={capabilities.dimensions.maximum}
                      value={selectedDimensions}
                      disabled={busy}
                      onChange={(e) =>
                        setDimensions(e.currentTarget.valueAsNumber || 0)
                      }
                    />
                  </label>
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
                    !model ||
                    !credential ||
                    unsupportedVideo ||
                    (active.mode === "stt" ? !file : !draft.trim())
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
              <p className="muted">{connectionLabel}</p>
              {session.kind === "api-key" && (
                <>
                  <label>
                    Gateway API key
                    <input
                      type="password"
                      autoComplete="off"
                      spellCheck={false}
                      value={key}
                      placeholder="Paste your API key"
                      disabled={busy}
                      onChange={(e) => setKey(e.target.value)}
                    />
                  </label>
                  <p className="hint">
                    Kept in memory for this page only. Sent only to this
                    gateway.
                  </p>
                </>
              )}
              <button disabled={busy} onClick={() => void checkSession()}>
                Refresh connection
              </button>
              {session.kind === "error" && (
                <a className="download" href="/app">
                  Sign in again
                </a>
              )}
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
                Credentials and generated media are never saved. Turning this
                off removes saved history.
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
              {page === "lab" && (
                <div className="modes">
                  {modes.map((mode) => (
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
              )}
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
                    {c.workspace === "lab" ? "Model Lab" : "Chat"} ·{" "}
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
