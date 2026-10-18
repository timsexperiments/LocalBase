import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  customVoiceListSchema,
  VOICE_MAX_SECONDS,
  VOICE_MIN_SECONDS,
  VOICE_SAMPLE_RATE_HZ,
  type CustomVoice,
  type CustomVoiceList,
} from "../domains/voices/voice-contract";
import { api, type Connection } from "./client";
import { startRecording, toWav, type WavCapture } from "./dictation-audio";

// The browser converts every take to the runtime's own format before upload.
const capture: WavCapture = {
  sampleRate: VOICE_SAMPLE_RATE_HZ,
  maxSeconds: VOICE_MAX_SECONDS,
};

type Take = { blob: Blob; url: string; seconds: number };

function seconds(durationMs: number) {
  return `${(durationMs / 1000).toFixed(1)} s`;
}

function message(error: unknown, fallback: string) {
  return error instanceof Error ? error.message : fallback;
}

/**
 * Lists custom voices for everyone who can choose voices; model managers can
 * also record or upload a reference, play it back, rename and delete.
 */
export function VoiceProfiles({
  connection,
  onChanged,
}: {
  connection: Connection;
  /** Called after a change so model metadata and voice pickers refresh. */
  onChanged: () => void;
}) {
  const [list, setList] = useState<CustomVoiceList | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [name, setName] = useState("");
  const [consent, setConsent] = useState(false);
  const [take, setTake] = useState<Take | null>(null);
  const [recorder, setRecorder] = useState<{ stop(): void } | null>(null);
  const [renaming, setRenaming] = useState<{ id: string; name: string }>();
  const [deleting, setDeleting] = useState("");
  const [playback, setPlayback] = useState<{ id: string; url: string }>();
  const abort = useRef<AbortController | null>(null);
  const file = useRef<HTMLInputElement>(null);

  async function load() {
    const response = await api("/_localbase/voices", connection);
    setList(customVoiceListSchema.parse(await response.json()));
  }

  useEffect(() => {
    load().catch((cause: unknown) =>
      setError(message(cause, "Could not load voices.")),
    );
    return () => abort.current?.abort();
  }, [connection]);
  useEffect(() => {
    if (take) return () => URL.revokeObjectURL(take.url);
  }, [take]);
  useEffect(() => {
    if (playback) return () => URL.revokeObjectURL(playback.url);
  }, [playback]);

  async function run(label: string, action: () => Promise<string>) {
    setBusy(label);
    setError("");
    setStatus("");
    try {
      setStatus(await action());
      await load();
      onChanged();
    } catch (cause) {
      setError(message(cause, "The voice could not be updated."));
    } finally {
      setBusy("");
    }
  }

  function keep(blob: Blob) {
    const length = (blob.size - 44) / 2 / VOICE_SAMPLE_RATE_HZ;
    setTake({ blob, url: URL.createObjectURL(blob), seconds: length });
    setError(
      length < VOICE_MIN_SECONDS
        ? `This take is ${length.toFixed(1)} s. Record at least ${VOICE_MIN_SECONDS} s of speech.`
        : "",
    );
  }

  async function record() {
    if (recorder) {
      recorder.stop();
      return;
    }
    setError("");
    setTake(null);
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    try {
      const session = await startRecording({
        signal: controller.signal,
        capture,
      });
      setRecorder(session);
      keep(await session.finished);
    } catch (cause) {
      if (!controller.signal.aborted)
        setError(message(cause, "Recording failed. Try again."));
    } finally {
      setRecorder(null);
    }
  }

  async function choose(selected: File | undefined) {
    if (!selected) return;
    setError("");
    setTake(null);
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    try {
      keep(await toWav(selected, controller.signal, capture));
    } catch (cause) {
      if (!controller.signal.aborted)
        setError(message(cause, "This file could not be read as audio."));
    } finally {
      if (file.current) file.current.value = "";
    }
  }

  function save(event: FormEvent) {
    event.preventDefault();
    if (!take || !consent || !name.trim()) return;
    const form = new FormData();
    form.set("name", name);
    form.set("consent", "true");
    form.set(
      "audio",
      new File([take.blob], "voice.wav", { type: "audio/wav" }),
    );
    void run("save", async () => {
      const response = await api("/_localbase/voices", connection, {
        method: "POST",
        body: form,
      });
      const created = (await response.json()) as CustomVoice;
      setName("");
      setConsent(false);
      setTake(null);
      return `Added ${created.name}. Choose it from the voice list above.`;
    });
  }

  async function play(voice: CustomVoice) {
    setError("");
    try {
      const response = await api(
        `/_localbase/voices/${voice.id}/audio`,
        connection,
      );
      setPlayback({
        id: voice.id,
        url: URL.createObjectURL(await response.blob()),
      });
    } catch (cause) {
      setError(message(cause, "Could not load the recording."));
    }
  }

  const canManage = list?.canManage ?? false;
  const voices = list?.data ?? [];
  const tooShort = take !== null && take.seconds < VOICE_MIN_SECONDS;
  return (
    <details className="voice-profiles" open={voices.length === 0 && canManage}>
      <summary>My voices{list ? ` (${voices.length})` : ""}</summary>
      <div className="voice-profiles-body">
        {list && !voices.length && (
          <p className="generation-settings-hint">
            {canManage
              ? "No custom voices yet. Add one from a short recording."
              : "No custom voices yet. An administrator can add them."}
          </p>
        )}
        {voices.length > 0 && (
          <ul className="voice-list">
            {voices.map((voice) => (
              <li key={voice.id}>
                {renaming?.id === voice.id ? (
                  <form
                    className="voice-row"
                    onSubmit={(event) => {
                      event.preventDefault();
                      void run("rename", async () => {
                        await api(
                          `/_localbase/voices/${voice.id}`,
                          connection,
                          {
                            method: "PATCH",
                            headers: { "content-type": "application/json" },
                            body: JSON.stringify({ name: renaming.name }),
                          },
                        );
                        setRenaming(undefined);
                        return "Renamed the voice.";
                      });
                    }}
                  >
                    <input
                      aria-label="New voice name"
                      maxLength={48}
                      value={renaming.name}
                      disabled={!!busy}
                      onChange={(event) =>
                        setRenaming({ id: voice.id, name: event.target.value })
                      }
                    />
                    <button type="submit" disabled={!!busy}>
                      Save
                    </button>
                    <button
                      type="button"
                      onClick={() => setRenaming(undefined)}
                    >
                      Cancel
                    </button>
                  </form>
                ) : (
                  <div className="voice-row">
                    <span className="voice-name">
                      {voice.name}
                      <small> · {seconds(voice.durationMs)}</small>
                    </span>
                    {canManage && (
                      <span className="voice-actions">
                        <button type="button" onClick={() => void play(voice)}>
                          Play
                        </button>
                        <button
                          type="button"
                          disabled={!!busy}
                          onClick={() =>
                            setRenaming({ id: voice.id, name: voice.name })
                          }
                        >
                          Rename
                        </button>
                        {deleting === voice.id ? (
                          <>
                            <button
                              type="button"
                              className="danger"
                              disabled={!!busy}
                              onClick={() =>
                                void run("delete", async () => {
                                  await api(
                                    `/_localbase/voices/${voice.id}`,
                                    connection,
                                    { method: "DELETE" },
                                  );
                                  setDeleting("");
                                  return `Deleted ${voice.name} and its recording.`;
                                })
                              }
                            >
                              Confirm delete
                            </button>
                            <button
                              type="button"
                              onClick={() => setDeleting("")}
                            >
                              Keep
                            </button>
                          </>
                        ) : (
                          <button
                            type="button"
                            className="danger"
                            disabled={!!busy}
                            onClick={() => setDeleting(voice.id)}
                          >
                            Delete
                          </button>
                        )}
                      </span>
                    )}
                  </div>
                )}
                {playback?.id === voice.id && (
                  <audio controls autoPlay src={playback.url} />
                )}
              </li>
            ))}
          </ul>
        )}
        {canManage && list && (
          <form className="voice-add" onSubmit={save}>
            <p className="generation-settings-hint">
              Record or upload {VOICE_MIN_SECONDS} to {VOICE_MAX_SECONDS}{" "}
              seconds of clear speech from one speaker. No transcript is needed;
              longer files are trimmed to {VOICE_MAX_SECONDS} s.
            </p>
            <label className="voice-field">
              <span>Name</span>
              <input
                maxLength={48}
                value={name}
                disabled={!!busy}
                onChange={(event) => setName(event.target.value)}
              />
            </label>
            <div className="voice-actions">
              <button
                type="button"
                disabled={!!busy}
                aria-pressed={recorder !== null}
                onClick={() => void record()}
              >
                {recorder ? "Stop recording" : "Record"}
              </button>
              <button
                type="button"
                disabled={!!busy || recorder !== null}
                onClick={() => file.current?.click()}
              >
                Upload file
              </button>
              <input
                ref={file}
                type="file"
                accept="audio/*"
                hidden
                onChange={(event) => void choose(event.target.files?.[0])}
              />
            </div>
            {take && (
              <div className="voice-take">
                <audio controls src={take.url} />
                <small>{take.seconds.toFixed(1)} s</small>
              </div>
            )}
            <label className="toggle voice-consent">
              <input
                type="checkbox"
                checked={consent}
                disabled={!!busy}
                onChange={(event) => setConsent(event.target.checked)}
              />
              <span>{list.consentStatement}</span>
            </label>
            <button
              type="submit"
              className="primary-action"
              disabled={!!busy || !take || tooShort || !consent || !name.trim()}
            >
              {busy === "save" ? "Saving…" : "Save voice"}
            </button>
          </form>
        )}
        {status && (
          <p className="notice" role="status">
            {status}
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
