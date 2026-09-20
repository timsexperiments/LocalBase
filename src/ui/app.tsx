import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { createRoot } from "react-dom/client";
import Markdown from "react-markdown";
import { z } from "zod";
import { generationTools, generateVideo, runChat, toolModels } from "./tools";
import {
  GenerationSettingsFields,
  modelGenerationSettings,
  type GenerationPreferences,
  type GenerationSettings,
  embeddingParameters,
  imageParameters,
  speechParameters,
  transcriptionParameters,
} from "./generation-settings";
import {
  api,
  discardLegacyFragmentCredential,
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
import {
  conversationNavigation,
  readNavigation,
  resolveNavigation,
  writeNavigation,
  type Navigation,
} from "./navigation";

const labels: Record<Mode, string> = {
  llm: "Chat",
  image: "Image",
  tts: "Speech",
  stt: "Transcribe",
  video: "Video",
  embedding: "Embeddings",
};
const navigationAbortReason = Symbol("navigation");
function fresh(
  mode: Mode = "llm",
  workspace: Conversation["workspace"] = "chat",
): Conversation {
  return {
    id: crypto.randomUUID(),
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
      onCancel={(event) => {
        event.preventDefault();
        close();
      }}
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
  const [session, setSession] = useState<SessionState>(() => {
    discardLegacyFragmentCredential();
    return { kind: "checking" };
  });
  const credential = sessionConnection(session);
  const [models, setModels] = useState<Model[]>([]);
  const [connection, setConnection] = useState("Connecting");
  const connectionLabel =
    session.kind === "error"
      ? session.message
      : session.kind === "checking"
        ? "Checking sign-in"
        : connection;
  const [initial] = useState(() => {
    let saved: Conversation[] | null = null;
    let error = "";
    try {
      saved = readHistory();
    } catch {
      error = "Saved history could not be read. You can clear it in Settings.";
    }
    return {
      ...resolveNavigation({
        navigation: readNavigation(location.search),
        conversations: saved ?? [],
        fresh,
      }),
      persistent: saved !== null,
      error,
    };
  });
  const [drawer, setPanel] = useState(initial.navigation.panel);
  const [conversations, setConversations] = useState(initial.conversations);
  const [activeId, setActiveId] = useState(initial.conversation.id);
  const [persistent, setPersistent] = useState(initial.persistent);
  const [draft, setDraft] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [generationPreferences, setGenerationPreferences] =
    useState<GenerationPreferences>({});
  const [error, setError] = useState(initial.error);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [modelSearch, setModelSearch] = useState("");
  const [historySearch, setHistorySearch] = useState("");
  const [showLatest, setShowLatest] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const mediaUrls = useRef<string[]>([]);
  const app = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  const nearBottom = useRef(true);
  const active =
    conversations.find((c) => c.id === activeId) ?? conversations[0];
  const page = active?.workspace ?? "chat";
  const candidates = availableModels(models, active?.mode ?? "llm");
  const model = candidates.find((m) => m.id === active?.model) ?? candidates[0];
  const generationSettings = modelGenerationSettings(
    generationPreferences,
    model?.id,
  );
  function setModelSettings(modelId: string, settings: GenerationSettings) {
    setGenerationPreferences((preferences) => ({
      ...preferences,
      [modelId]: settings,
    }));
  }
  const capabilities = model?.catalog.capabilities;
  const unsupportedVideo =
    active?.mode === "video" &&
    capabilities?.kind === "video" &&
    capabilities.mode === "s2v";
  const update = (id: string, change: (c: Conversation) => Conversation) =>
    setConversations((items) =>
      items.map((c) => (c.id === id ? change(c) : c)),
    );
  function setDrawer(panel: Navigation["panel"]) {
    if (!active) return;
    setModelSearch("");
    setHistorySearch("");
    setPanel(panel);
    writeNavigation(
      conversationNavigation(active, panel, model?.id ?? active.model),
      "push",
    );
  }
  async function checkSession(signal?: AbortSignal) {
    try {
      const verified = await readSession(signal);
      signal?.throwIfAborted();
      setSession(verified);
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
  }, [session]);
  useEffect(() => {
    writeNavigation(initial.navigation, "replace");
    return () => {
      controller.current?.abort();
      mediaUrls.current.forEach((url) => URL.revokeObjectURL(url));
    };
  }, []);
  useEffect(() => {
    const restore = () => {
      const restored = resolveNavigation({
        navigation: readNavigation(location.search),
        conversations,
        fresh,
      });
      if (
        restored.conversation.id !== activeId ||
        restored.conversation.model !== active?.model
      ) {
        controller.current?.abort(navigationAbortReason);
        setDraft("");
        setFile(null);
        setError("");
        setWarnings([]);
        nearBottom.current = true;
      }
      setConversations(restored.conversations);
      setActiveId(restored.conversation.id);
      setPanel(restored.navigation.panel);
      writeNavigation(restored.navigation, "replace");
    };
    window.addEventListener("popstate", restore);
    return () => window.removeEventListener("popstate", restore);
  }, [conversations, activeId]);
  useEffect(() => {
    if (!active || !model || active.model === model.id) return;
    update(active.id, (conversation) => ({ ...conversation, model: model.id }));
    writeNavigation(
      conversationNavigation(active, drawer, model.id),
      "replace",
    );
  }, [active?.id, active?.model, model?.id, drawer]);
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
  function scrollToLatest() {
    const element = transcript.current;
    if (element) element.scrollTop = element.scrollHeight;
    nearBottom.current = true;
    setShowLatest(false);
  }
  useLayoutEffect(() => {
    if (nearBottom.current) scrollToLatest();
  }, [active?.id, active?.messages]);
  useLayoutEffect(() => {
    const element = input.current;
    if (!element) return;
    element.style.height = "auto";
    element.style.height = `${element.scrollHeight}px`;
    if (nearBottom.current) scrollToLatest();
  }, [draft, active?.mode]);
  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return;
    const resize = () => {
      if (!app.current || viewport.scale !== 1) return;
      app.current.style.height = `${viewport.height}px`;
      app.current.style.top = `${viewport.offsetTop}px`;
      if (nearBottom.current) scrollToLatest();
    };
    resize();
    viewport.addEventListener("resize", resize);
    viewport.addEventListener("scroll", resize);
    return () => {
      viewport.removeEventListener("resize", resize);
      viewport.removeEventListener("scroll", resize);
    };
  }, []);
  function newChat(
    mode: Mode = active?.mode ?? "llm",
    workspace = page,
    panel: Navigation["panel"] = null,
  ) {
    if (busy) return;
    const c = fresh(mode, workspace);
    setConversations((items) => [c, ...items].slice(0, 30));
    setActiveId(c.id);
    setDraft("");
    setFile(null);
    setError("");
    setWarnings([]);
    setPanel(panel);
    writeNavigation(conversationNavigation(c, panel), "push");
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
    setWarnings([]);
    setError("");
    if (!retry) setDraft("");
    nearBottom.current = true;
    update(active.id, (c) => ({
      ...c,
      model: model.id,
      title: base.length ? c.title : user.text.slice(0, 60),
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
            preferences: generationPreferences,
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
          const id = crypto.randomUUID();
          const video = await generateVideo({
            settings: generationSettings,
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
                    ...embeddingParameters(model, generationSettings.embedding),
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
                  ...imageParameters(generationSettings.image),
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
              ...speechParameters(model, generationSettings.tts),
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
          for (const [name, value] of Object.entries(
            transcriptionParameters(model, generationSettings.stt),
          ))
            body.set(name, value);
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
      if (abort.signal.reason !== navigationAbortReason)
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
    <div className="app" ref={app}>
      <header className="topbar">
        <button
          className="icon"
          aria-label="Open menu and history"
          onClick={() => setDrawer("history")}
        >
          ☰
        </button>
        <button
          type="button"
          className="brand"
          disabled={busy}
          onClick={() => newChat("llm", "chat")}
        >
          <span className="brand-mark">L</span>LocalBase
        </button>
        <select
          className="workspace-picker"
          aria-label="Workspace"
          value={page === "chat" ? "chat" : active.mode}
          disabled={busy}
          onChange={(event) => {
            const mode = modes.find((mode) => mode === event.target.value);
            newChat(mode ?? "llm", mode ? "lab" : "chat");
          }}
        >
          <option value="chat">Chat</option>
          <optgroup label="Model Lab">
            {modes.map((mode) => (
              <option value={mode} key={mode}>
                {labels[mode]} lab
              </option>
            ))}
          </optgroup>
        </select>
        <button
          disabled={busy}
          onClick={() => newChat()}
          aria-label="New conversation"
          title="New conversation"
        >
          ＋
        </button>
      </header>
      <div className="workspace">
        <main
          ref={transcript}
          aria-label="Conversation"
          onScroll={(e) => {
            const el = e.currentTarget;
            nearBottom.current =
              el.scrollHeight - el.scrollTop - el.clientHeight < 100;
            setShowLatest(!nearBottom.current);
          }}
        >
          <div className="conversation">
            {!active.messages.length && (
              <section className="empty">
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
                  ) : message.artifacts?.length ? null : busy &&
                    message.id === active.messages.at(-1)?.id ? (
                    <p className="thinking" role="status">
                      Working<span>…</span>
                    </p>
                  ) : (
                    <p className="muted">No response received.</p>
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
                {message.role === "assistant" && (
                  <div className="message-actions">
                    {message.text && <Copy text={message.text} />}
                    {!busy &&
                      active.mode !== "stt" &&
                      message.id === active.messages.at(-1)?.id && (
                        <button
                          className="copy"
                          disabled={!credential || !model || unsupportedVideo}
                          onClick={() => void send(true)}
                        >
                          Try again
                        </button>
                      )}
                  </div>
                )}
              </article>
            ))}
          </div>
        </main>
        <footer className="composer-area">
          {showLatest && (
            <button
              className="jump-latest"
              onClick={scrollToLatest}
              aria-label="Jump to latest message"
            >
              ↓ Latest
            </button>
          )}
          <div className="composer-content">
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
            {unsupportedVideo && (
              <p className="notice" role="status">
                Choose a text-to-video model. Portrait and audio inputs are not
                supported here yet.
              </p>
            )}
            {error && (
              <div className="error" role="alert">
                <span>{error}</span>
                {active.messages.length > 0 && active.mode !== "stt" && (
                  <button
                    disabled={busy || !credential || !model || unsupportedVideo}
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
            {!model && session.kind !== "error" && (
              <p className="notice" role="status">
                {models.length
                  ? `No selected, installed ${labels[active.mode].toLowerCase()} model. Configure one with the LocalBase CLI.`
                  : connectionLabel}{" "}
                <button onClick={() => setDrawer("settings")}>Connect</button>
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
                    onChange={(e) =>
                      setFile(e.currentTarget.files?.[0] ?? null)
                    }
                  />
                </label>
              ) : (
                <textarea
                  ref={input}
                  rows={1}
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
                <button
                  type="button"
                  className="model-picker"
                  disabled={busy}
                  aria-label={`Choose model: ${model?.catalog.name ?? "none selected"}`}
                  onClick={() => setDrawer("models")}
                  title={model?.catalog.name ?? "Choose a model"}
                >
                  <span
                    className={`status-dot ${connectionLabel === "Gateway ready" ? "ready" : ""}`}
                    aria-hidden="true"
                  />
                  <span>{model?.catalog.name ?? "Choose model"}</span>
                  <span aria-hidden="true">⌄</span>
                </button>
                <button
                  type="button"
                  className="generation-settings-button"
                  disabled={busy || !model}
                  onClick={() => setDrawer("generation")}
                  aria-label="Generation controls"
                  title="Generation controls"
                >
                  <svg
                    width="20"
                    height="20"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.6"
                    aria-hidden="true"
                  >
                    <path d="M4 7h7m4 0h5M4 17h3m4 0h9" />
                    <circle cx="13" cy="7" r="2" />
                    <circle cx="9" cy="17" r="2" />
                  </svg>
                </button>
                {busy ? (
                  <button
                    type="button"
                    className="send"
                    aria-label="Stop generation"
                    onClick={() => controller.current?.abort()}
                  >
                    ■
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
                    ↑
                  </button>
                )}
              </div>
            </form>
            {active.mode === "tts" && (
              <p className="input-count">{draft.length}/256 characters</p>
            )}
          </div>
        </footer>
      </div>
      {drawer && (
        <Drawer
          title={
            drawer === "generation"
              ? "Generation settings"
              : drawer === "settings"
                ? "Settings"
                : drawer === "models"
                  ? "Models"
                  : "History"
          }
          close={() => setDrawer(null)}
        >
          {drawer === "generation" ? (
            <div className="generation-settings">
              {page === "chat" && model && (
                <p className="generation-settings-hint">
                  {generationTools(models, model).length
                    ? `Chat tools: ${generationTools(models, model)
                        .map((tool) => tool.function.name.replaceAll("_", " "))
                        .join(", ")}.`
                    : "Text chat only. Tools need a tool-calling chat model and installed media models."}
                </p>
              )}
              <p className="generation-settings-hint">
                Saved per model for this session, shared by chat and Model Lab.
                Blank values use model defaults.
              </p>
              {model ? (
                <div>
                  <h3>{model.catalog.name}</h3>
                  <GenerationSettingsFields
                    mode={active.mode}
                    model={model}
                    settings={generationSettings}
                    onChange={(settings) =>
                      setModelSettings(model.id, settings)
                    }
                  />
                </div>
              ) : (
                <p className="generation-settings-hint">
                  Choose an installed model to adjust its controls.
                </p>
              )}
              {page === "chat" &&
                model &&
                generationTools(models, model).flatMap((tool) => {
                  const name = tool.function.name;
                  const mode =
                    name === "generate_image"
                      ? "image"
                      : name === "generate_video"
                        ? "video"
                        : "tts";
                  return toolModels(models, name).map((target) => (
                    <details className="tool-settings" key={target.id}>
                      <summary>
                        {target.catalog.name} · {labels[mode]}
                      </summary>
                      <GenerationSettingsFields
                        mode={mode}
                        model={target}
                        settings={modelGenerationSettings(
                          generationPreferences,
                          target.id,
                        )}
                        onChange={(settings) =>
                          setModelSettings(target.id, settings)
                        }
                      />
                    </details>
                  ));
                })}
            </div>
          ) : drawer === "settings" ? (
            <>
              <p className="muted">{connectionLabel}</p>
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
                  writeNavigation(
                    conversationNavigation(c, "settings"),
                    "push",
                  );
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
              <input
                type="search"
                aria-label="Search models"
                placeholder="Search models…"
                value={modelSearch}
                onChange={(event) => setModelSearch(event.target.value)}
              />
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
                        newChat(mode, page, "models");
                      }}
                    >
                      {labels[mode]}
                    </button>
                  ))}
                </div>
              )}
              {candidates.length ? (
                candidates
                  .filter((candidate) =>
                    `${candidate.catalog.name} ${candidate.catalog.quantization}`
                      .toLowerCase()
                      .includes(modelSearch.toLowerCase()),
                  )
                  .map((m) => (
                    <button
                      disabled={busy}
                      className={`model-card ${m.id === model?.id ? "selected" : ""}`}
                      key={m.id}
                      aria-pressed={m.id === model?.id}
                      onClick={() => {
                        update(active.id, (c) => ({ ...c, model: m.id }));
                        setPanel(null);
                        writeNavigation(
                          conversationNavigation(active, null, m.id),
                          "push",
                        );
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
              {candidates.length > 0 &&
                !candidates.some((candidate) =>
                  `${candidate.catalog.name} ${candidate.catalog.quantization}`
                    .toLowerCase()
                    .includes(modelSearch.toLowerCase()),
                ) && <p className="hint">No models match your search.</p>}
            </>
          ) : (
            <>
              <div className="menu-actions">
                <button disabled={busy} onClick={() => newChat()}>
                  ＋ New conversation
                </button>
                <button onClick={() => setDrawer("settings")}>Settings</button>
              </div>
              <input
                type="search"
                aria-label="Search conversations"
                placeholder="Search conversations…"
                value={historySearch}
                onChange={(event) => setHistorySearch(event.target.value)}
              />
              <p className="hint">
                {persistent
                  ? "Text is saved on this device. Media lasts for this session."
                  : "Conversations disappear when this page closes or reloads."}
              </p>
              {conversations
                .filter((c) =>
                  c.title.toLowerCase().includes(historySearch.toLowerCase()),
                )
                .map((c) => (
                  <button
                    disabled={busy}
                    className="history-item"
                    key={c.id}
                    aria-current={c.id === activeId ? "page" : undefined}
                    onClick={() => {
                      setActiveId(c.id);
                      setDraft("");
                      setFile(null);
                      setError("");
                      setWarnings([]);
                      setPanel(null);
                      writeNavigation(conversationNavigation(c, null), "push");
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
