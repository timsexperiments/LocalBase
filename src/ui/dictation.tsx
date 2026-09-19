import {
  createContext,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { api, transcriptionSchema, type Connection } from "./client";
import { startRecording } from "./dictation-audio";

export function appendDictation(value: string, text: string): string {
  const spoken = text.trim();
  if (!spoken) return value;
  return `${value}${value && !/\s$/.test(value) ? " " : ""}${spoken}`;
}

export async function transcribeDictation(
  audio: Blob,
  modelId: string,
  connection: Connection,
  signal: AbortSignal,
): Promise<string> {
  const form = new FormData();
  form.set("model", modelId);
  form.set("response_format", "json");
  form.set("file", audio, "dictation.wav");
  const response = await api("/v1/audio/transcriptions", connection, {
    method: "POST",
    body: form,
    signal,
  });
  return transcriptionSchema.parse(await response.json()).text;
}

type Activity = {
  id: string;
  phase: "requesting" | "recording" | "transcribing";
};
type Dictation = {
  activity: Activity | null;
  unavailable: string;
  start: (
    id: string,
    onText: (text: string) => void,
    onError: (message: string) => void,
  ) => void;
  stop: () => void;
  cancel: () => void;
};
const context = createContext<Dictation | null>(null);

export function DictationProvider({
  connection,
  modelId,
  scope,
  children,
}: {
  connection: Connection | null;
  modelId: string | undefined;
  scope: string;
  children: ReactNode;
}) {
  const [activity, setActivity] = useState<Activity | null>(null);
  const active = useRef<{ abort: AbortController; stop?: () => void } | null>(
    null,
  );
  const connectionKey =
    connection?.kind === "api-key" ? connection.key : connection?.kind;
  const unavailable = !globalThis.isSecureContext
    ? "Microphone access needs HTTPS or localhost. Open the HTTPS app to dictate."
    : !navigator.mediaDevices?.getUserMedia ||
        typeof MediaRecorder === "undefined"
      ? "This browser does not support microphone recording."
      : !connection
        ? "Connect to LocalBase to dictate."
        : !modelId
          ? "Enable and install a transcription model in Manage models."
          : "";

  function cancel() {
    active.current?.abort.abort();
    active.current = null;
    setActivity(null);
  }
  useEffect(() => {
    cancel();
    return cancel;
  }, [connectionKey, modelId, scope]);
  useEffect(() => {
    const hide = () => {
      if (document.hidden) cancel();
    };
    document.addEventListener("visibilitychange", hide);
    return () => document.removeEventListener("visibilitychange", hide);
  }, []);

  async function start(
    id: string,
    onText: (text: string) => void,
    onError: (message: string) => void,
  ) {
    if (unavailable || !connection || !modelId) return;
    cancel();
    const abort = new AbortController();
    const operation: { abort: AbortController; stop?: () => void } = { abort };
    active.current = operation;
    setActivity({ id, phase: "requesting" });
    try {
      const recording = await startRecording({ signal: abort.signal });
      if (abort.signal.aborted) {
        recording.cancel();
        return;
      }
      operation.stop = () => {
        recording.stop();
      };
      setActivity({ id, phase: "recording" });
      const audio = await recording.finished;
      if (abort.signal.aborted) return;
      setActivity({ id, phase: "transcribing" });
      const text = await transcribeDictation(
        audio,
        modelId,
        connection,
        abort.signal,
      );
      if (!abort.signal.aborted) {
        if (!text.trim()) onError("No speech was recognized. Try again.");
        else onText(text);
      }
    } catch (error) {
      if (!abort.signal.aborted)
        onError(
          error instanceof Error
            ? error.message
            : "Dictation failed. Try again.",
        );
    } finally {
      abort.abort();
      if (active.current === operation) {
        active.current = null;
        setActivity(null);
      }
    }
  }

  return (
    <context.Provider
      value={{
        activity,
        unavailable,
        start: (id, onText, onError) => {
          void start(id, onText, onError);
        },
        stop: () => active.current?.stop?.(),
        cancel,
      }}
    >
      {children}
    </context.Provider>
  );
}

export function DictationButton({
  label,
  onText,
  disabled = false,
}: {
  label: string;
  onText: (text: string) => void;
  disabled?: boolean;
}) {
  const dictation = useContext(context);
  const id = useId();
  const [error, setError] = useState("");
  const latest = useRef(onText);
  latest.current = onText;
  const cancel = useRef<(() => void) | undefined>(undefined);
  const own = dictation?.activity?.id === id;
  cancel.current = own ? dictation.cancel : undefined;
  useEffect(() => () => cancel.current?.(), []);
  useEffect(() => {
    if (disabled) cancel.current?.();
  }, [disabled]);
  if (!dictation) return null;
  const phase = own ? dictation.activity?.phase : undefined;
  const help = error;
  return (
    <span className="dictation-control">
      <button
        type="button"
        className={`dictation-button ${phase ?? ""}`}
        disabled={disabled || Boolean(dictation.activity && !own)}
        aria-label={
          phase === "recording"
            ? `Stop recording for ${label}`
            : phase
              ? `Cancel dictation for ${label}`
              : `Dictate ${label}`
        }
        title={
          help ||
          dictation.unavailable ||
          "Dictate with LocalBase. Tap again to stop. Text is added without sending."
        }
        aria-describedby={help ? `${id}-help` : undefined}
        onClick={() => {
          if (dictation.unavailable) {
            setError(dictation.unavailable);
            return;
          }
          setError("");
          if (phase === "recording") dictation.stop();
          else if (phase) dictation.cancel();
          else dictation.start(id, (text) => latest.current(text), setError);
        }}
      >
        {phase ? (
          phase === "recording" ? (
            "■"
          ) : (
            "×"
          )
        ) : (
          <svg
            width="20"
            height="20"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.7"
            aria-hidden="true"
          >
            <rect x="9" y="2" width="6" height="13" rx="3" />
            <path d="M5 10v2a7 7 0 0 0 14 0v-2M12 19v3m-4 0h8" />
          </svg>
        )}
      </button>
      {phase && (
        <span className="dictation-status" role="status">
          {phase === "requesting"
            ? "Waiting for microphone…"
            : phase === "recording"
              ? "Recording · tap stop when finished"
              : "Transcribing…"}
          {phase === "recording" && (
            <button type="button" onClick={dictation.cancel}>
              Discard recording
            </button>
          )}
        </span>
      )}
      {help && (
        <span
          id={`${id}-help`}
          className="dictation-help"
          role={error ? "alert" : undefined}
        >
          {help}
        </span>
      )}
    </span>
  );
}
