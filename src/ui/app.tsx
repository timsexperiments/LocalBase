import {
  Fragment,
  startTransition,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { createRoot } from "react-dom/client";
import { Copy, MessageMarkdown } from "./message-markdown";
import { ModelManagement } from "./model-management";
import { pollWhileVisible } from "./visibility";
import { AuthManagement } from "./auth-management";
import {
  attachmentAccept,
  attachmentStorageError,
  chatRequestError,
  conversationAttachmentError,
  readAttachments,
  type Attachment,
} from "./attachments";
import {
  DictationButton,
  DictationProvider,
  appendDictation,
} from "./dictation";
import { z } from "zod";
import { generationTools, generateVideo, runChat, toolModels } from "./tools";
import {
  Studio,
  imageRequestMode,
  imageResultLabel,
  imageResultText,
  type StudioRequest,
} from "./studio";
import { DecisionLab } from "./decision-lab";
import {
  chatContext,
  compact,
  contextWindow,
  learnContext,
  overflowContext,
  planCompaction,
  promptBudget,
  toolOverhead,
  visibleMessages,
} from "./compaction";
import {
  canSteer,
  deltaBuffer,
  interruptReply,
  steerAbortReason,
  streamingHistoryIntervalMs,
  type Phase,
} from "./steering";
import { CompactionDivider, ReasoningBlock, WorkingStatus } from "./chat-parts";
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
  canUseMode,
  discardLegacyFragmentCredential,
  availableModels,
  catalogModels,
  historyKey,
  hasPermission,
  imageResponseSchema,
  logoutSession,
  modelMemorySummary,
  speechVoiceCount,
  modelModalities,
  modelName,
  embeddingSummary,
  embeddingSummaryText,
  modelsSchema,
  modes,
  modelAcceptsInput,
  permittedModes,
  permittedUiModes,
  readHistory,
  readinessSchema,
  readSession,
  redirectToSignIn,
  sessionConnection,
  SessionRequiredError,
  signInFailed,
  type SessionState,
  type Session,
  transcriptionSchema,
  writeHistory,
  type Conversation,
  type Message,
  type Mode,
  type Model,
  type ModelsResponse,
  type Media,
} from "./client";
import "./style.css";
import {
  canOpenPanel,
  conversationNavigation,
  navigationUrl,
  readNavigation,
  resolveNavigation,
  startConversation,
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
  decision: "Decision",
};
const composerLabels: Record<Mode, string> = {
  llm: "Message",
  image: "Describe an image",
  video: "Describe the video",
  tts: "Text to read aloud",
  stt: "Message",
  embedding: "Text to embed",
  decision: "State",
};
const composerPlaceholders: Record<Mode, string> = {
  llm: "Message LocalBase…",
  image: "Describe an image…",
  video: "Describe the video…",
  tts: "Write something to read aloud…",
  stt: "Message LocalBase…",
  embedding: "Enter text to embed…",
  decision: "Describe the state to decide on…",
};
const navigationAbortReason = Symbol("navigation");
/** Messages rendered when a conversation opens, then mounted per step. */
const historyFirstMount = 12;
const historyMountStep = 40;
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
function EmbeddingCard({
  embedding,
}: {
  embedding: NonNullable<Message["embedding"]>;
}) {
  const { dimensions, preview, norm } = embeddingSummary(embedding.values);
  const [download, setDownload] = useState<string | null>(null);
  useEffect(() => {
    const url = URL.createObjectURL(
      new Blob(
        [
          JSON.stringify({
            model: embedding.model,
            dimensions,
            embedding: embedding.values,
          }),
        ],
        { type: "application/json" },
      ),
    );
    setDownload(url);
    return () => URL.revokeObjectURL(url);
  }, [embedding]);
  return (
    <section className="embedding-card" aria-label="Embedding">
      <dl>
        <div>
          <dt>Model</dt>
          <dd>{embedding.name}</dd>
        </div>
        <div>
          <dt>Dimensions</dt>
          <dd>{dimensions.toLocaleString()}</dd>
        </div>
        <div>
          <dt>L2 norm</dt>
          <dd>{norm.toFixed(4)}</dd>
        </div>
      </dl>
      <p className="embedding-preview">
        <span>First {preview.length} values</span>
        <code>
          [{preview.map((value) => value.toFixed(4)).join(", ")}
          {dimensions > preview.length ? ", …" : ""}]
        </code>
      </p>
      <div className="message-actions">
        <Copy
          label="Copy vector"
          text={() => JSON.stringify(embedding.values)}
        />
        {download && (
          <a
            className="copy"
            href={download}
            download={`localbase-embedding-${dimensions}d.json`}
          >
            Download (.json)
          </a>
        )}
      </div>
    </section>
  );
}
function AttachmentChips({
  attachments,
  remove,
}: {
  attachments: Attachment[];
  remove?: (id: string) => void;
}) {
  if (!attachments.length) return null;
  return (
    <ul className="attachment-list" aria-label="Attachments">
      {attachments.map((attachment) => (
        <li className="attachment-chip" key={attachment.id}>
          {attachment.kind === "image" && (
            <img src={attachment.url} alt={attachment.name} />
          )}
          <span className="attachment-info">
            <span title={attachment.name}>{attachment.name}</span>
            <small>{Math.max(1, Math.ceil(attachment.size / 1024))} KB</small>
          </span>
          {remove && (
            <button
              type="button"
              aria-label={`Remove ${attachment.name}`}
              onClick={() => remove(attachment.id)}
            >
              ✕
            </button>
          )}
        </li>
      ))}
    </ul>
  );
}
function Drawer({
  title,
  close,
  children,
  fullPage = false,
}: {
  title: string;
  close: () => void;
  children: ReactNode;
  fullPage?: boolean;
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
      className={fullPage ? "management-page" : undefined}
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
        <div className="drawer-header">
          <h2 id="drawer-title">{title}</h2>
          <button aria-label="Close drawer" onClick={close}>
            ✕
          </button>
        </div>
        {children}
      </div>
    </dialog>
  );
}
function AccountMenu({
  session,
  signingOut,
  signOut,
}: {
  session: Session;
  signingOut: boolean;
  signOut: () => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const label = session.verifiedEmail ?? "Signed in";
  const initial = session.verifiedEmail?.charAt(0).toUpperCase() ?? "U";
  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: PointerEvent) => {
      if (event.target instanceof Node && !root.current?.contains(event.target))
        setOpen(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setOpen(false);
      trigger.current?.focus();
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);
  return (
    <div className="account-menu" ref={root}>
      <button
        ref={trigger}
        className="account-trigger"
        aria-label={`Account: ${label}`}
        aria-controls="account-popover"
        aria-expanded={open}
        onClick={() => setOpen((current) => !current)}
      >
        <span aria-hidden="true">{initial}</span>
      </button>
      {open && (
        <div className="account-popover" id="account-popover">
          <span className="account-avatar" aria-hidden="true">
            {initial}
          </span>
          <div className="account-identity">
            <strong>Signed in</strong>
            <span title={label}>{label}</span>
          </div>
          <button disabled={signingOut} onClick={() => void signOut()}>
            {signingOut ? "Signing out…" : "Sign out"}
          </button>
        </div>
      )}
    </div>
  );
}
function App() {
  const [session, setSession] = useState<SessionState>(() => {
    discardLegacyFragmentCredential();
    return { kind: "checking" };
  });
  const credential = sessionConnection(session);
  const permissions = session.kind === "session" ? session.permissions : null;
  const canReadModels = hasPermission(permissions, "models:read");
  const inferenceModes = permittedModes(permissions);
  const allowedModes = permittedUiModes(permissions);
  const canDictate = hasPermission(permissions, "inference:transcription");
  const [models, setModels] = useState<Model[]>([]);
  const [hostMemory, setHostMemory] = useState<
    ModelsResponse["host"]["memory"] | null
  >(null);
  const [dictationModelId, setDictationModelId] = useState("");
  const sttModels = canDictate ? availableModels(models, "stt") : [];
  const dictationModel =
    sttModels.find((model) => model.id === dictationModelId) ?? sttModels[0];
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
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const [attachmentReadError, setAttachmentReadError] = useState("");
  const [readingAttachments, setReadingAttachments] = useState(false);
  const attachmentInput = useRef<HTMLInputElement>(null);
  const attachmentEpoch = useRef(0);
  const attachmentPickerEpoch = useRef<number | null>(null);
  const attachmentRead = useRef<number | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [generationPreferences, setGenerationPreferences] =
    useState<GenerationPreferences>({});
  const [error, setError] = useState(initial.error);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const [modelSearch, setModelSearch] = useState("");
  const [historySearch, setHistorySearch] = useState("");
  const [showLatest, setShowLatest] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const running = useRef<{ conversation: string; reply: string } | null>(null);
  const [compactingNow, setCompactingNow] = useState(false);
  const [phase, setPhase] = useState<{ phase: Phase; since: number } | null>(
    null,
  );
  const mediaUrls = useRef<string[]>([]);
  const app = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  const nearBottom = useRef(true);
  const active =
    conversations.find((c) => c.id === activeId) ?? conversations[0];
  // Opening a long conversation mounts its latest messages first, then
  // prepends older ones in small, interruptible steps so the open never
  // blocks on rendering (and parsing Markdown for) the whole history.
  const [mounted, setMounted] = useState<{ id?: string; from: number }>({
    from: 0,
  });
  const mountedFrom = !active
    ? 0
    : mounted.id === active.id
      ? Math.min(mounted.from, active.messages.length)
      : Math.max(0, active.messages.length - historyFirstMount);
  const mountAnchor = useRef<{ node: Element; top: number } | null>(null);
  const page = active?.workspace ?? "chat";
  const activeModeAllowed = active
    ? canReadModels && canUseMode(permissions, active.mode)
    : false;
  const candidates =
    canReadModels && activeModeAllowed
      ? availableModels(models, active?.mode ?? "llm")
      : [];
  const pickerModels =
    canReadModels && activeModeAllowed
      ? catalogModels(models, active?.mode ?? "llm")
      : [];
  const model = candidates.find((m) => m.id === active?.model) ?? candidates[0];
  const canOpenDrawer = (panel: Navigation["panel"]) =>
    canOpenPanel(panel, permissions) &&
    (panel !== "models" && panel !== "generation"
      ? true
      : canReadModels && activeModeAllowed);
  const visibleDrawer = canOpenDrawer(drawer) ? drawer : null;
  const canManageModels = canOpenPanel("catalog", permissions);
  const canReadAdministration = canOpenPanel("admin", permissions);
  const accept = active?.mode === "llm" ? attachmentAccept(model) : "";
  const storedAttachments = conversations.flatMap((conversation) =>
    conversation.messages.flatMap((message) => message.attachments ?? []),
  );
  const attachmentProblem =
    attachmentStorageError([...storedAttachments, ...attachments]) ??
    (active
      ? conversationAttachmentError(
          [
            ...active.messages,
            ...(draft.trim() || attachments.length
              ? [
                  {
                    id: "draft",
                    role: "user" as const,
                    text: draft.trim(),
                    attachments,
                  },
                ]
              : []),
          ],
          model,
        )
      : null);
  function invalidateAttachmentRead() {
    attachmentEpoch.current += 1;
    attachmentPickerEpoch.current = null;
    attachmentRead.current = null;
    setReadingAttachments(false);
    if (attachmentInput.current) attachmentInput.current.value = "";
  }
  function clearAttachments() {
    invalidateAttachmentRead();
    setAttachments([]);
    setAttachmentReadError("");
  }
  // Invalidate before paint, including automatic model fallback and auth loss.
  useLayoutEffect(() => {
    if (attachmentRead.current !== null)
      setAttachmentReadError(
        "File reading was canceled because the model or sign-in changed. Choose the files again.",
      );
    invalidateAttachmentRead();
    return () => {
      attachmentEpoch.current += 1;
      attachmentPickerEpoch.current = null;
    };
  }, [
    active?.id,
    active?.mode,
    model?.id,
    JSON.stringify(model?.catalog),
    session,
  ]);
  async function attachFiles(files: File[]) {
    if (
      !files.length ||
      !model ||
      !credential ||
      !accept ||
      busy ||
      attachmentRead.current !== null ||
      attachmentPickerEpoch.current !== attachmentEpoch.current
    )
      return;
    const epoch = attachmentEpoch.current;
    attachmentRead.current = epoch;
    setReadingAttachments(true);
    try {
      const added = await readAttachments(
        files,
        model,
        attachments,
        storedAttachments,
      );
      if (epoch !== attachmentEpoch.current) return;
      setAttachments((current) => [...current, ...added]);
      setAttachmentReadError("");
    } catch (cause) {
      if (epoch !== attachmentEpoch.current) return;
      setAttachmentReadError(
        cause instanceof Error
          ? cause.message
          : "Could not read the files. Choose them again.",
      );
    } finally {
      if (epoch === attachmentEpoch.current) {
        attachmentRead.current = null;
        setReadingAttachments(false);
      }
    }
  }
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
  const studio = active?.mode === "image" || active?.mode === "video";
  const decision = active?.mode === "decision";
  const update = (id: string, change: (c: Conversation) => Conversation) =>
    setConversations((items) =>
      items.map((c) => (c.id === id ? change(c) : c)),
    );
  function setDrawer(panel: Navigation["panel"]) {
    if (!active || !canOpenDrawer(panel)) return;
    setModelSearch("");
    setHistorySearch("");
    setPanel(panel);
    writeNavigation(
      conversationNavigation(active, panel, model?.id ?? active.model),
      "push",
    );
  }
  function sessionExpired(message: string) {
    setModels([]);
    setHostMemory(null);
    setSession({
      kind: "error",
      message: redirectToSignIn()
        ? "Your sign-in has expired. Taking you to sign in…"
        : signInFailed()
          ? "Sign-in didn't complete. Try signing in again."
          : message,
    });
  }
  async function checkSession(signal?: AbortSignal) {
    try {
      const verified = await readSession(signal);
      signal?.throwIfAborted();
      setSession(verified);
    } catch (e) {
      if (signal?.aborted) return;
      if (e instanceof SessionRequiredError) {
        sessionExpired(e.message);
        return;
      }
      setModels([]);
      setHostMemory(null);
      setSession({
        kind: "error",
        message:
          "Sign-in could not be verified. Check your connection and try again.",
      });
    }
  }
  async function signOut() {
    setSigningOut(true);
    try {
      await logoutSession();
      if (session.kind === "session" && session.logoutUrl) {
        location.assign(session.logoutUrl);
        return;
      }
      setModels([]);
      setHostMemory(null);
      setSession({ kind: "error", message: "You signed out." });
    } catch (cause) {
      setError(
        cause instanceof Error
          ? cause.message
          : "Could not sign out. Try again.",
      );
    } finally {
      setSigningOut(false);
    }
  }
  async function refresh(signal?: AbortSignal) {
    if (!credential) return;
    try {
      const [metadata, readiness] = await Promise.all([
        canReadModels
          ? api("/_localbase/models", credential, { signal })
              .then((r) => r.json())
              .then((v) => modelsSchema.parse(v))
          : null,
        fetch("/health/ready", { signal, cache: "no-store" })
          .then((r) => r.json())
          .then((v) => readinessSchema.parse(v)),
      ]);
      signal?.throwIfAborted();
      setModels(metadata?.data ?? []);
      setHostMemory(metadata?.host.memory ?? null);
      setConnection(
        readiness.status === "ready" ? "Gateway ready" : "Gateway not ready",
      );
    } catch (e) {
      if (!signal?.aborted) {
        setModels([]);
        setHostMemory(null);
        if (e instanceof SessionRequiredError) sessionExpired(e.message);
        else if (e instanceof z.ZodError) {
          console.error("Unexpected gateway response", e.issues);
          setConnection(
            "This page could not read the gateway's model list. Reload to pick up the latest version.",
          );
        } else
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
    if (session.kind !== "session" || canOpenDrawer(drawer) || !active) return;
    setPanel(null);
    writeNavigation(
      conversationNavigation(active, null, model?.id ?? active.model),
      "replace",
    );
  }, [session, drawer, active?.id, active?.model, model?.id]);
  useLayoutEffect(() => {
    if (
      session.kind !== "session" ||
      !active ||
      canUseMode(session.permissions, active.mode)
    )
      return;
    const fallback = allowedModes[0];
    if (!fallback) return;
    const { conversation, conversations: next } = startConversation({
      conversations,
      mode: fallback,
      workspace: fallback === "llm" ? "chat" : "lab",
      fresh,
    });
    setConversations(next);
    setActiveId(conversation.id);
    setPanel(null);
    clearAttachments();
    setDraft("");
    setFile(null);
    setError("");
    setWarnings([]);
    writeNavigation(conversationNavigation(conversation, null), "replace");
  }, [session, active?.id, active?.mode]);
  useEffect(() => {
    if (!credential) {
      setModels([]);
      setHostMemory(null);
      return;
    }
    const abort = new AbortController();
    void refresh(abort.signal);
    // Hidden tabs skip the poll; showing the tab again refreshes at once.
    const stop = pollWhileVisible(() => void refresh(abort.signal), 15000);
    return () => {
      abort.abort();
      stop();
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
      }
      if (restored.conversation.id !== activeId) {
        clearAttachments();
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
  const historySaved = useRef(0);
  useEffect(() => {
    if (!persistent) return;
    const save = () => {
      historySaved.current = Date.now();
      try {
        writeHistory(conversations);
      } catch {
        setError(
          "Device storage is full or unavailable. History could not be saved.",
        );
      }
    };
    // Serializing all history per streamed delta can starve the stream on
    // slower devices; while busy, save at most once per interval. The final
    // state is saved as soon as busy clears.
    const wait = busy
      ? historySaved.current + streamingHistoryIntervalMs - Date.now()
      : 0;
    if (wait <= 0) return save();
    const timer = setTimeout(save, wait);
    return () => clearTimeout(timer);
  }, [conversations, persistent, busy]);
  function scrollToLatest() {
    const element = transcript.current;
    if (element) element.scrollTop = element.scrollHeight;
    nearBottom.current = true;
    setShowLatest(false);
  }
  useLayoutEffect(() => {
    if (nearBottom.current) scrollToLatest();
  }, [active?.id, active?.messages, activeModeAllowed, studio]);
  function rememberMountAnchor() {
    const first = transcript.current?.querySelector(
      ".conversation > article.message",
    );
    mountAnchor.current = first
      ? { node: first, top: first.getBoundingClientRect().top }
      : null;
  }
  useEffect(() => {
    if (!active) return;
    if (mounted.id !== active.id) {
      setMounted({ id: active.id, from: mountedFrom });
      return;
    }
    if (!mountedFrom) return;
    const timer = setTimeout(() =>
      startTransition(() =>
        setMounted((current) =>
          current.id === active.id
            ? {
                id: current.id,
                from: Math.max(0, mountedFrom - historyMountStep),
              }
            : current,
        ),
      ),
    );
    return () => clearTimeout(timer);
  }, [active?.id, mounted.id, mountedFrom]);
  useLayoutEffect(() => {
    // Keep the reader's place when older messages mount above it (Safari
    // has no scroll anchoring), or stay pinned to the latest message.
    const element = transcript.current;
    const anchor = mountAnchor.current;
    if (nearBottom.current) scrollToLatest();
    else if (element && anchor?.node.isConnected) {
      element.scrollTop += anchor.node.getBoundingClientRect().top - anchor.top;
    }
    if (mountedFrom) rememberMountAnchor();
    else mountAnchor.current = null;
  }, [mountedFrom]);
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
    if (busy || !allowedModes.includes(mode)) return;
    const { conversation: c, conversations: next } = startConversation({
      conversations,
      mode,
      workspace,
      fresh,
    });
    setConversations(next);
    setActiveId(c.id);
    clearAttachments();
    setDraft("");
    setFile(null);
    setError("");
    setWarnings([]);
    setPanel(panel);
    writeNavigation(conversationNavigation(c, panel), "push");
    nearBottom.current = true;
  }
  async function send(retry = false, request?: StudioRequest) {
    // Sending while a chat reply streams steers: the reply is interrupted
    // (its partial text kept) and a new turn starts right away.
    const steering =
      !!active &&
      canSteer({
        busy,
        mode: active.mode,
        draft,
        retry,
        studio: !!request,
      }) &&
      running.current?.conversation === active.id;
    if (
      !active ||
      !canUseMode(permissions, active.mode) ||
      !credential ||
      !model ||
      (busy && !steering) ||
      readingAttachments ||
      attachmentRead.current !== null ||
      unsupportedVideo ||
      (!retry &&
        active.mode !== "stt" &&
        !draft.trim() &&
        !attachments.length) ||
      (!retry && active.mode === "tts" && draft.length > 256) ||
      (active.mode === "video" &&
        (generationSettings.video.negative_prompt?.length ?? 0) > 16384) ||
      (active.mode === "stt" && !file)
    )
      return;
    if (attachmentProblem) return;
    const previousUser = active.messages.reduce(
      (last, m, index) => (m.role === "user" ? index : last),
      -1,
    );
    const interrupted = steering ? running.current?.reply : undefined;
    const interrupt = (m: Message) =>
      m.id === interrupted ? interruptReply(m, "Interrupted") : m;
    const base =
      retry && previousUser >= 0
        ? active.messages.slice(0, previousUser)
        : active.messages.map(interrupt);
    const text =
      retry && previousUser >= 0
        ? (active.messages[previousUser]?.text ?? "")
        : draft.trim();
    const user: Message = {
      id: crypto.randomUUID(),
      role: "user",
      text: active.mode === "stt" ? (file?.name ?? "Audio file") : text,
      attachments:
        retry && previousUser >= 0
          ? active.messages[previousUser]?.attachments
          : attachments,
    };
    const requestProblem = chatRequestError([...base, user], model);
    if (requestProblem) {
      setAttachmentReadError(requestProblem);
      return;
    }
    const reply: Message = {
      id: crypto.randomUUID(),
      role: "assistant",
      text: "",
    };
    if (steering) controller.current?.abort(steerAbortReason);
    const abort = new AbortController();
    controller.current = abort;
    running.current = { conversation: active.id, reply: reply.id };
    setPhase({ phase: "waiting", since: Date.now() });
    setBusy(true);
    setWarnings([]);
    setError("");
    if (!retry && !studio) {
      setDraft("");
      invalidateAttachmentRead();
      setAttachments([]);
    }
    nearBottom.current = true;
    update(active.id, (c) => ({
      ...c,
      model: model.id,
      title: base.length ? c.title : (user.text || "Attachments").slice(0, 60),
      // When steering, keep the interrupted reply's latest streamed text.
      messages: steering
        ? [...c.messages.map(interrupt), user, reply]
        : [...base, user, reply],
    }));
    const patch = (change: (m: Message) => Message) =>
      update(active.id, (c) => ({
        ...c,
        messages: c.messages.map((m) => (m.id === reply.id ? change(m) : m)),
      }));
    const stream = deltaBuffer(({ text, reasoning }) =>
      patch((m) => ({
        ...m,
        text: m.text + text,
        ...(reasoning ? { reasoning: (m.reasoning ?? "") + reasoning } : {}),
      })),
    );
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
          const history = [...base, user];
          const conversation = active.id;
          let compaction = active.compaction;
          const compactHistory = async (context: number, force: boolean) => {
            const plan = planCompaction({
              messages: history,
              compaction,
              budget: promptBudget(context, generationSettings.llm.max_tokens),
              overhead: page === "chat" ? toolOverhead : 0,
              force,
            });
            if (!plan) return false;
            setPhase({ phase: "compacting", since: Date.now() });
            const result = await compact({
              connection: credential,
              model,
              messages: history,
              compaction,
              ...plan,
              context,
              signal: abort.signal,
            });
            compaction = result.compaction;
            update(conversation, (c) => ({ ...c, compaction }));
            if (result.error)
              setWarnings((items) => [
                ...items,
                `Couldn't summarize earlier messages (${result.error}); they were dropped from the model's context.`,
              ]);
            setPhase({ phase: "waiting", since: Date.now() });
            return true;
          };
          await compactHistory(contextWindow(model), false);
          let streamed = false;
          const chat = () =>
            runChat({
              preferences: generationPreferences,
              model,
              models,
              connection: credential,
              signal: abort.signal,
              messages: chatContext(history, compaction),
              toolsEnabled: page === "chat",
              permissions: permissions ?? [],
              append: (text) => {
                streamed = true;
                stream.text(text);
              },
              reasoning: (text) => {
                streamed = true;
                setPhase((current) =>
                  current?.phase === "thinking"
                    ? current
                    : { phase: "thinking", since: Date.now() },
                );
                stream.reasoning(text);
              },
              artifact: (artifact) => {
                stream.flush();
                patch((m) => ({
                  ...m,
                  artifacts: [
                    ...(m.artifacts ?? []).filter(
                      (item) => item.id !== artifact.id,
                    ),
                    artifact,
                  ],
                }));
              },
              register: (url) => mediaUrls.current.push(url),
              warning: (detail) => setWarnings((items) => [...items, detail]),
            });
          let protocol;
          try {
            protocol = await chat();
          } catch (e) {
            // An overflow reports the real runtime context: learn it, compact
            // to fit, and retry once if nothing was streamed yet.
            const size =
              e instanceof Error && !streamed && !abort.signal.aborted
                ? overflowContext(e.message)
                : null;
            if (!size) throw e;
            learnContext(model.id, size);
            if (!(await compactHistory(size, true))) throw e;
            protocol = await chat();
          }
          stream.flush();
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
            image: request?.firstFrame,
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
            source: request?.sourceUrl,
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
          const values = result.data[0]?.embedding ?? [];
          patch((m) => ({
            ...m,
            text: embeddingSummaryText(modelName(model), values),
            embedding: { model: model.id, name: modelName(model), values },
          }));
          break;
        }
        case "image": {
          const n = generationSettings.image.n ?? 1;
          const tuning = imageParameters(generationSettings.image, model);
          let init: RequestInit;
          const imageMode = imageRequestMode(request, model);
          let path = "/v1/images/generations";
          if (request?.source && imageMode !== "text-to-image") {
            path = "/v1/images/edits";
            const body = new FormData();
            body.set("image", request.source, "source.png");
            if (request.mask) body.set("mask", request.mask, "mask.png");
            body.set("model", model.id);
            body.set("prompt", text);
            body.set("n", String(n));
            body.set("response_format", "b64_json");
            for (const [name, value] of Object.entries({
              ...tuning,
              size: request.size ?? tuning.size,
              strength: generationSettings.image.strength ?? 0.75,
            }))
              if (value !== undefined) body.set(name, String(value));
            init = { method: "POST", signal: abort.signal, body };
          } else
            init = json({
              model: model.id,
              prompt: text,
              n,
              ...tuning,
              response_format: "b64_json",
            });
          const result = imageResponseSchema.parse(
            await (await api(path, credential, init)).json(),
          );
          abort.signal.throwIfAborted();
          patch((m) => ({
            ...m,
            text: imageResultText(imageMode, result.data.length),
            source: path.endsWith("edits") ? request?.sourceUrl : undefined,
            artifacts: result.data.map((image, index) => ({
              id: crypto.randomUUID(),
              label: imageResultLabel(imageMode, index, result.data.length),
              state: "complete",
              media: {
                kind: "image",
                url: `data:image/png;base64,${image.b64_json}`,
              },
            })),
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
      stream.flush();
      if (e instanceof SessionRequiredError) {
        authenticationFailed = true;
        sessionExpired(e.message);
      }
      const steered = abort.signal.reason === steerAbortReason;
      patch((m) =>
        abort.signal.aborted && active.mode === "llm"
          ? // Keep the partial reply (not its tool protocol) as context.
            interruptReply(m, steered ? "Interrupted" : "Stopped")
          : {
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
            },
      );
      if (abort.signal.reason !== navigationAbortReason && !steered)
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
      stream.flush();
      // A steering send already owns the controller and busy state.
      if (controller.current === abort) {
        setBusy(false);
        setPhase(null);
        controller.current = null;
        running.current = null;
        if (!authenticationFailed) void refresh();
      }
    }
  }
  async function compactNow() {
    if (!active || active.mode !== "llm" || busy || !credential || !model)
      return;
    const conversation = active.id;
    const context = contextWindow(model);
    const plan = planCompaction({
      messages: active.messages,
      compaction: active.compaction,
      budget: promptBudget(context, generationSettings.llm.max_tokens),
      overhead: page === "chat" ? toolOverhead : 0,
      force: true,
    });
    if (!plan) return;
    const abort = new AbortController();
    controller.current = abort;
    running.current = null;
    setBusy(true);
    setCompactingNow(true);
    setPhase({ phase: "compacting", since: Date.now() });
    setWarnings([]);
    setError("");
    try {
      const result = await compact({
        connection: credential,
        model,
        messages: active.messages,
        compaction: active.compaction,
        ...plan,
        context,
        signal: abort.signal,
      });
      update(conversation, (c) => ({ ...c, compaction: result.compaction }));
      if (result.error)
        setWarnings([
          `Couldn't summarize earlier messages (${result.error}); they were dropped from the model's context.`,
        ]);
    } catch (e) {
      if (e instanceof SessionRequiredError) sessionExpired(e.message);
      else if (!abort.signal.aborted)
        setError(e instanceof Error ? e.message : "Compaction failed.");
    } finally {
      if (controller.current === abort) {
        setBusy(false);
        setPhase(null);
        controller.current = null;
      }
      setCompactingNow(false);
    }
  }
  if (!active) return null;
  const notices = (
    <>
      {session.kind === "error" && (
        <div className="error session-notice" role="alert">
          <span>{session.message}</span>
          <a className="download" href="/app/login">
            Sign in
          </a>
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
          {active.messages.length > 0 && active.mode !== "stt" && !studio && (
            <button
              disabled={
                busy ||
                !credential ||
                !model ||
                unsupportedVideo ||
                readingAttachments ||
                !!attachmentProblem
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
      {attachmentReadError && (
        <div className="error" role="alert">
          <span>{attachmentReadError}</span>
          <button
            type="button"
            aria-label="Dismiss attachment error"
            onClick={() => setAttachmentReadError("")}
          >
            ✕
          </button>
        </div>
      )}
      {attachmentProblem && (
        <p className="error" role="alert">
          {attachmentProblem}
        </p>
      )}
      {readingAttachments && (
        <p className="notice" role="status">
          Reading files. Wait before sending.
        </p>
      )}
      {!model && session.kind !== "error" && (
        <p className="notice" role="status">
          {models.length
            ? `No ${labels[active.mode].toLowerCase()} model is installed and enabled.`
            : connectionLabel}{" "}
          {!models.length ? (
            <button onClick={() => setDrawer("settings")}>Connect</button>
          ) : canManageModels ? (
            <button onClick={() => setDrawer("catalog")}>Manage models</button>
          ) : (
            "Ask an administrator to install one."
          )}
        </p>
      )}
    </>
  );
  const view = (
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
          aria-label="LocalBase"
          disabled={busy || !allowedModes.length}
          onClick={() => {
            const mode = canUseMode(permissions, "llm")
              ? "llm"
              : allowedModes[0];
            if (mode) newChat(mode, mode === "llm" ? "chat" : "lab");
          }}
        >
          <span className="brand-mark" aria-hidden="true">
            L
          </span>
          <span className="brand-name">LocalBase</span>
        </button>
        {allowedModes.length > 0 && (
          <>
            <label className="workspace-picker">
              {/* The native select stays on top for the platform picker; this
                  span only sizes the control to the current workspace. */}
              <span aria-hidden="true">
                {page === "chat" ? "Chat" : `${labels[active.mode]} lab`}
              </span>
              <span aria-hidden="true">⌄</span>
              <select
                aria-label="Workspace"
                value={page === "chat" ? "chat" : active.mode}
                disabled={busy}
                onChange={(event) => {
                  const mode = modes.find(
                    (mode) => mode === event.target.value,
                  );
                  newChat(mode ?? "llm", mode ? "lab" : "chat");
                }}
              >
                {canUseMode(permissions, "llm") && (
                  <option value="chat">Chat</option>
                )}
                <optgroup label="Model Lab">
                  {allowedModes.map((mode) => (
                    <option value={mode} key={mode}>
                      {labels[mode]} lab
                    </option>
                  ))}
                </optgroup>
              </select>
            </label>
            <button
              disabled={busy}
              onClick={() => newChat()}
              aria-label="New conversation"
              title="New conversation"
            >
              ＋
            </button>
          </>
        )}
        {session.kind === "session" && (
          <AccountMenu
            session={session}
            signingOut={signingOut}
            signOut={signOut}
          />
        )}
      </header>
      <div className="workspace">
        {session.kind === "session" && !activeModeAllowed && (
          <main className="conversation">
            <section className="empty">
              <h1>
                {inferenceModes.length
                  ? "No model access"
                  : "No inference access"}
              </h1>
              <p>
                {inferenceModes.length
                  ? "Your account cannot discover the models needed by this interface."
                  : "Your account has no permission to use an inference modality."}
              </p>
            </section>
          </main>
        )}
        {decision && activeModeAllowed && (
          <DecisionLab
            key={active.id}
            model={model}
            credential={credential}
            onChooseModel={() => setDrawer("models")}
            onSessionExpired={sessionExpired}
          />
        )}
        {studio && activeModeAllowed && (
          <Studio
            key={active.id}
            mode={active.mode === "video" ? "video" : "image"}
            model={model}
            messages={active.messages}
            busy={busy}
            progress={
              busy
                ? (active.messages
                    .at(-1)
                    ?.artifacts?.flatMap((artifact) =>
                      artifact.state === "working" ? [artifact.detail] : [],
                    )
                    .at(-1) ?? "")
                : ""
            }
            prompt={draft}
            setPrompt={setDraft}
            settings={generationSettings}
            onSettings={(settings) =>
              model && setModelSettings(model.id, settings)
            }
            canDictate={canDictate}
            canGenerate={
              !!credential &&
              !!model &&
              !unsupportedVideo &&
              !readingAttachments
            }
            notices={notices}
            onGenerate={(request) => void send(false, request)}
            onStop={() => controller.current?.abort()}
            onChooseModel={() => setDrawer("models")}
          />
        )}
        <main
          hidden={!activeModeAllowed || studio || decision}
          ref={transcript}
          aria-label="Conversation"
          onScroll={(e) => {
            const el = e.currentTarget;
            nearBottom.current =
              el.scrollHeight - el.scrollTop - el.clientHeight < 100;
            setShowLatest(!nearBottom.current);
            if (mountedFrom) rememberMountAnchor();
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
                        ? "Enter text to see its vector size, a preview of its values, and its norm. Copy or download the full vector."
                        : active.mode === "image"
                          ? "Write a prompt to generate an image."
                          : active.mode === "tts"
                            ? speechVoiceCount(model) > 1
                              ? "Enter the words to read aloud. Pick a voice in generation controls."
                              : "Enter the words to read aloud."
                            : "Choose an audio file to transcribe."}
                </p>
              </section>
            )}
            {active.messages.slice(mountedFrom).map((message) => {
              const live = busy && message.id === active.messages.at(-1)?.id;
              return (
                <Fragment key={message.id}>
                  <article className={`message ${message.role}`}>
                    <div className="message-label">
                      {message.role === "user" ? "You" : "LocalBase"}
                    </div>
                    <div className="message-body">
                      {message.role === "assistant" && message.reasoning && (
                        <ReasoningBlock
                          text={message.reasoning}
                          live={live}
                          answering={!!message.text}
                        />
                      )}
                      {message.embedding ? (
                        <EmbeddingCard embedding={message.embedding} />
                      ) : message.text ? (
                        <MessageMarkdown text={message.text} />
                      ) : message.artifacts?.length ||
                        (message.role === "user" &&
                          message.attachmentsMissing) ||
                        message.attachments?.length ? null : live ? (
                        message.reasoning ? null : (
                          <WorkingStatus
                            phase={phase?.phase ?? "waiting"}
                            since={phase?.since ?? Date.now()}
                          />
                        )
                      ) : message.interrupted ? (
                        <p className="muted">Interrupted before a response.</p>
                      ) : (
                        <p className="muted">No response received.</p>
                      )}
                      {message.role === "user" && (
                        <>
                          <AttachmentChips
                            attachments={message.attachments ?? []}
                          />
                          {message.attachmentsMissing && (
                            <p className="muted">
                              Attachments from the earlier session are no longer
                              available.
                            </p>
                          )}
                        </>
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
                              role={
                                artifact.state === "error" ? "alert" : "status"
                              }
                            >
                              {artifact.detail}
                            </p>
                          )}
                        </section>
                      ))}
                    </div>
                    {message.role === "assistant" && (
                      <div className="message-actions">
                        {message.interrupted && message.text && (
                          <span className="interrupted-tag">Interrupted</span>
                        )}
                        {message.text && !message.embedding && (
                          <Copy
                            label={
                              active.mode === "stt"
                                ? "Copy transcript"
                                : active.mode === "llm"
                                  ? "Copy response"
                                  : "Copy text"
                            }
                            text={message.text}
                          />
                        )}
                        {!busy &&
                          active.mode !== "stt" &&
                          message.id === active.messages.at(-1)?.id && (
                            <button
                              className="copy"
                              disabled={
                                !credential ||
                                !model ||
                                unsupportedVideo ||
                                readingAttachments ||
                                !!attachmentProblem
                              }
                              onClick={() => void send(true)}
                            >
                              Try again
                            </button>
                          )}
                        {!busy &&
                          active.mode === "llm" &&
                          message.id === active.messages.at(-1)?.id &&
                          visibleMessages(
                            active.messages,
                            active.compaction,
                          ).filter(({ role }) => role === "user").length >=
                            2 && (
                            <button
                              className="copy"
                              disabled={!credential || !model}
                              onClick={() => void compactNow()}
                            >
                              Compact now
                            </button>
                          )}
                      </div>
                    )}
                  </article>
                  {active.compaction?.through === message.id && (
                    <CompactionDivider compaction={active.compaction} />
                  )}
                </Fragment>
              );
            })}
            {compactingNow && (
              <WorkingStatus
                phase="compacting"
                since={phase?.since ?? Date.now()}
              />
            )}
          </div>
        </main>
        <div
          className="composer-area"
          hidden={!activeModeAllowed || studio || decision}
        >
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
            {notices}
            <form
              className="composer"
              onSubmit={(e) => {
                e.preventDefault();
                void send();
              }}
            >
              <AttachmentChips
                attachments={attachments}
                remove={
                  busy
                    ? undefined
                    : (id) =>
                        setAttachments((items) =>
                          items.filter((item) => item.id !== id),
                        )
                }
              />
              {active.mode === "stt" ? (
                <label className={`upload ${busy ? "disabled" : ""}`}>
                  <input
                    key={file ? "chosen" : "empty"}
                    type="file"
                    aria-label="Audio file"
                    accept="audio/*,.wav,.mp3,.m4a,.ogg,.flac"
                    disabled={busy}
                    onChange={(e) =>
                      setFile(e.currentTarget.files?.[0] ?? null)
                    }
                  />
                  <strong>{file ? file.name : "Choose audio file"}</strong>
                  <span>
                    {file
                      ? `${(file.size / 1024 / 1024).toFixed(1)} MiB · choose another`
                      : "WAV, MP3, M4A, OGG or FLAC"}
                  </span>
                </label>
              ) : (
                <textarea
                  ref={input}
                  rows={1}
                  aria-label={composerLabels[active.mode]}
                  placeholder={composerPlaceholders[active.mode]}
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
                {accept && (
                  <>
                    <input
                      ref={attachmentInput}
                      type="file"
                      hidden
                      multiple
                      accept={accept}
                      disabled={busy || readingAttachments || !credential}
                      aria-label="Choose attachments"
                      onChange={(event) => {
                        const files = Array.from(
                          event.currentTarget.files ?? [],
                        );
                        event.currentTarget.value = "";
                        void attachFiles(files);
                      }}
                    />
                    <button
                      type="button"
                      className="attachment-button"
                      aria-label="Attach files"
                      title="Attach files"
                      disabled={busy || readingAttachments || !credential}
                      onClick={() => {
                        attachmentPickerEpoch.current = attachmentEpoch.current;
                        attachmentInput.current?.click();
                      }}
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
                        <path d="m8 13 7-7a3 3 0 0 1 4 4l-9 9a5 5 0 0 1-7-7l9-9a1 1 0 0 1 4 4l-9 9a1 1 0 0 1-2-2l8-8" />
                      </svg>
                    </button>
                  </>
                )}
                {canReadModels && (
                  <>
                    <button
                      type="button"
                      className="model-picker"
                      disabled={busy}
                      aria-label={`Choose model: ${model ? modelName(model) : "none selected"}`}
                      onClick={() => setDrawer("models")}
                      title={model ? modelName(model) : "Choose a model"}
                    >
                      <span
                        className={`status-dot ${connectionLabel === "Gateway ready" ? "ready" : ""}`}
                        aria-hidden="true"
                      />
                      <span>{model ? modelName(model) : "Choose model"}</span>
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
                  </>
                )}
                {canDictate && active.mode !== "stt" && (
                  <DictationButton
                    label={active.mode === "llm" ? "message" : "prompt"}
                    disabled={busy}
                    onText={(text) =>
                      setDraft((value) => appendDictation(value, text))
                    }
                  />
                )}
                {busy && (
                  <button
                    type="button"
                    className={`send${draft.trim() ? " stop" : ""}`}
                    aria-label="Stop generation"
                    onClick={() => controller.current?.abort()}
                  >
                    ■
                  </button>
                )}
                {busy ? (
                  canSteer({ busy, mode: active.mode, draft }) &&
                  !compactingNow && (
                    <button
                      className="send"
                      type="submit"
                      disabled={
                        !model ||
                        !credential ||
                        readingAttachments ||
                        !!attachmentProblem
                      }
                      aria-label="Send now (interrupts reply)"
                      title="Send now (interrupts reply)"
                    >
                      ↑
                    </button>
                  )
                ) : (
                  <button
                    className="send"
                    type="submit"
                    disabled={
                      !model ||
                      !credential ||
                      readingAttachments ||
                      !!attachmentProblem ||
                      unsupportedVideo ||
                      (active.mode === "tts" && draft.length > 256) ||
                      (active.mode === "video" &&
                        (generationSettings.video.negative_prompt?.length ??
                          0) > 16384) ||
                      (active.mode === "stt"
                        ? !file
                        : !draft.trim() && !attachments.length)
                    }
                    aria-label="Send request"
                  >
                    ↑
                  </button>
                )}
              </div>
            </form>
            {accept && (
              <details className="attachment-hint">
                <summary>Text/code files · Up to 4 · 128 KiB each</summary>
                <p>
                  UTF-8 text/code: 256 KiB total.
                  {model &&
                    modelAcceptsInput(model, "image") &&
                    " PNG, JPEG, WebP: 5 MiB each."}{" "}
                  No PDF or Office files. Attachments are session-only.
                </p>
              </details>
            )}
            {active.mode === "tts" && (
              <p className="input-count">
                {draft.length}/256 characters
                {draft.length > 256
                  ? " · Shorten the text before sending."
                  : ""}
              </p>
            )}
          </div>
        </div>
      </div>
      <footer className="license-notice">
        AGPL-3.0-or-later ·{" "}
        <a href="https://github.com/timsexperiments/LocalBase">Source</a>
      </footer>
      {visibleDrawer && (
        <Drawer
          fullPage={visibleDrawer === "catalog" || visibleDrawer === "admin"}
          title={
            visibleDrawer === "catalog"
              ? "Manage models"
              : visibleDrawer === "admin"
                ? "Access & API keys"
                : visibleDrawer === "generation"
                  ? "Generation settings"
                  : visibleDrawer === "settings"
                    ? "Settings"
                    : visibleDrawer === "models"
                      ? "Models"
                      : "History"
          }
          close={() => setDrawer(null)}
        >
          {visibleDrawer === "catalog" ? (
            <ModelManagement
              connection={credential}
              models={models}
              refreshModels={refresh}
              openSettings={() => setDrawer("settings")}
              canDictate={canDictate}
            />
          ) : visibleDrawer === "admin" ? (
            <AuthManagement
              connection={credential}
              permissions={permissions ?? []}
            />
          ) : visibleDrawer === "generation" ? (
            <div className="generation-settings">
              {page === "chat" && model && (
                <p className="generation-settings-hint">
                  {generationTools(models, model, permissions ?? []).length
                    ? `Chat tools: ${generationTools(
                        models,
                        model,
                        permissions ?? [],
                      )
                        .map((tool) => tool.function.name.replaceAll("_", " "))
                        .join(", ")}.`
                    : "Text chat only. Tools need a tool-calling chat model and installed media models."}
                </p>
              )}
              <p className="generation-settings-hint">
                These apply to the model wherever it runs: chat, its tools,
                Model Lab and the studios. They are not saved and reset when the
                page reloads. Blank fields use model defaults.
              </p>
              {model ? (
                <div>
                  <h3>{modelName(model)}</h3>
                  <GenerationSettingsFields
                    mode={active.mode}
                    model={model}
                    settings={generationSettings}
                    canDictate={canDictate}
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
                generationTools(models, model, permissions ?? []).flatMap(
                  (tool) => {
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
                          {modelName(target)} · {labels[mode]}
                        </summary>
                        <GenerationSettingsFields
                          mode={mode}
                          model={target}
                          settings={modelGenerationSettings(
                            generationPreferences,
                            target.id,
                          )}
                          canDictate={canDictate}
                          onChange={(settings) =>
                            setModelSettings(target.id, settings)
                          }
                        />
                      </details>
                    ));
                  },
                )}
            </div>
          ) : visibleDrawer === "settings" ? (
            <>
              <p className="muted">{connectionLabel}</p>
              <button disabled={busy} onClick={() => void checkSession()}>
                Refresh connection
              </button>
              {session.kind === "error" && (
                <a className="download" href="/app/login">
                  Sign in
                </a>
              )}
              <hr />
              {canDictate && (
                <>
                  <label>
                    Dictation model
                    <select
                      value={dictationModel?.id ?? ""}
                      disabled={!sttModels.length}
                      onChange={(event) =>
                        setDictationModelId(event.target.value)
                      }
                    >
                      {!sttModels.length && (
                        <option value="">
                          No installed, enabled transcription model
                        </option>
                      )}
                      {sttModels.map((model) => (
                        <option value={model.id} key={model.id}>
                          {model.id} · {modelMemorySummary(model, hostMemory)}
                        </option>
                      ))}
                    </select>
                  </label>
                  <p className="hint">
                    Microphone buttons add text without sending. Recordings last
                    up to one minute and are sent only to this LocalBase
                    gateway. Use HTTPS or localhost for microphone access.
                  </p>
                </>
              )}
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
                Credentials, attachments, and generated media are never saved.
                Turning this off removes saved history.
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
                  clearAttachments();
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
              {canReadAdministration && (
                <>
                  <hr />
                  <button onClick={() => setDrawer("admin")}>
                    Access & API keys
                  </button>
                </>
              )}
            </>
          ) : visibleDrawer === "models" ? (
            <>
              <div className="dictation-field">
                <input
                  type="search"
                  aria-label="Search models"
                  placeholder="Search models…"
                  value={modelSearch}
                  onChange={(event) => setModelSearch(event.target.value)}
                />
                {canDictate && (
                  <DictationButton
                    label="model search"
                    onText={(text) =>
                      setModelSearch((value) => appendDictation(value, text))
                    }
                  />
                )}
              </div>
              <p className="hint">Install or enable a model to use it here.</p>
              {canManageModels && (
                <a
                  className="manage-models-link"
                  href={navigationUrl(
                    location.href,
                    conversationNavigation(active, "catalog"),
                  )}
                  onClick={(event) => {
                    if (
                      event.metaKey ||
                      event.ctrlKey ||
                      event.shiftKey ||
                      event.altKey
                    )
                      return;
                    event.preventDefault();
                    setDrawer("catalog");
                  }}
                >
                  Manage models →
                </a>
              )}
              {page === "lab" && (
                <div className="modes">
                  {allowedModes.map((mode) => (
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
              {pickerModels.length ? (
                pickerModels
                  .filter((candidate) =>
                    `${modelName(candidate)} ${candidate.catalog.name} ${candidate.id} ${candidate.catalog.quantization}`
                      .toLowerCase()
                      .includes(modelSearch.toLowerCase()),
                  )
                  .map((m) => (
                    <button
                      disabled={
                        busy || !m.device.installed || !m.device.selected
                      }
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
                      <strong>{modelName(m)}</strong>
                      <span>
                        {m.catalog.quantization} ·{" "}
                        {m.device.installed
                          ? m.device.selected
                            ? "Installed · Enabled"
                            : "Installed · Disabled"
                          : "Not installed"}
                      </span>
                      <small className="model-id">{m.id}</small>
                      <small>
                        {modelModalities(m)}
                        {m.catalog.contextWindowTokens
                          ? ` · ${m.catalog.contextWindowTokens.toLocaleString()} context`
                          : ""}
                      </small>
                      <small>{modelMemorySummary(m, hostMemory)}</small>
                    </button>
                  ))
              ) : (
                <p>
                  No catalog models for {labels[active.mode].toLowerCase()}.
                </p>
              )}
              {pickerModels.length > 0 &&
                !pickerModels.some((candidate) =>
                  `${modelName(candidate)} ${candidate.catalog.name} ${candidate.id} ${candidate.catalog.quantization}`
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
                {canManageModels && (
                  <button onClick={() => setDrawer("catalog")}>
                    Manage models
                  </button>
                )}
                {canReadAdministration && (
                  <button onClick={() => setDrawer("admin")}>
                    Access & API keys
                  </button>
                )}
              </div>
              <div className="dictation-field">
                <input
                  type="search"
                  aria-label="Search conversations"
                  placeholder="Search conversations…"
                  value={historySearch}
                  onChange={(event) => setHistorySearch(event.target.value)}
                />
                {canDictate && (
                  <DictationButton
                    label="conversation search"
                    onText={(text) =>
                      setHistorySearch((value) => appendDictation(value, text))
                    }
                  />
                )}
              </div>
              <p className="hint">
                {persistent
                  ? "Text is saved on this device. Media lasts for this session."
                  : "Conversations disappear when this page closes or reloads."}
              </p>
              {conversations
                .filter((conversation) =>
                  canUseMode(permissions, conversation.mode),
                )
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
                      clearAttachments();
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
  return (
    <DictationProvider
      connection={credential}
      modelId={dictationModel?.id}
      scope={`${active.id}:${active.mode}:${active.model}:${drawer ?? ""}`}
    >
      {view}
    </DictationProvider>
  );
}
const root = document.getElementById("root");
if (root) createRoot(root).render(<App />);
