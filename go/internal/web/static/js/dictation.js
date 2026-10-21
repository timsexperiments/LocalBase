// Dictation (TS dictation.tsx and dictation-audio.ts): record from the
// microphone, convert to 16 kHz mono PCM16 WAV in the browser, transcribe
// it as the signed-in person, and add the text to the draft without
// sending.

const SAMPLE_RATE = 16_000;
const MAX_SECONDS = 60;
const MAX_BYTES = 16 * 1024 * 1024;
const dictationCapture = { sampleRate: SAMPLE_RATE, maxSeconds: MAX_SECONDS };

export function appendDictation(value, text) {
  const spoken = text.trim();
  if (!spoken) return value;
  return `${value}${value && !/\s$/.test(value) ? " " : ""}${spoken}`;
}

/** Encode mono samples (16 kHz unless stated) as little-endian PCM16 WAV. */
export function encodePcm16Wav(samples, { sampleRate, maxSeconds } = dictationCapture) {
  if (!samples.length || samples.length > sampleRate * maxSeconds)
    throw new Error(`Recordings must contain between 0 and ${maxSeconds} seconds of audio.`);
  const buffer = new ArrayBuffer(44 + samples.length * 2);
  const view = new DataView(buffer);
  const text = (offset, value) => {
    for (let i = 0; i < value.length; i++) view.setUint8(offset + i, value.charCodeAt(i));
  };
  text(0, "RIFF");
  view.setUint32(4, buffer.byteLength - 8, true);
  text(8, "WAVEfmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  text(36, "data");
  view.setUint32(40, samples.length * 2, true);
  samples.forEach((value, index) => {
    const sample = Number.isFinite(value) ? Math.max(-1, Math.min(1, value)) : 0;
    view.setInt16(44 + index * 2, Math.round(sample * (sample < 0 ? 32768 : 32767)), true);
  });
  return new Blob([buffer], { type: "audio/wav" });
}

const aborted = () => new DOMException("Dictation cancelled.", "AbortError");
const stopTracks = (stream) => {
  for (const track of stream.getTracks()) track.stop();
};

function microphone(signal) {
  return new Promise((resolve, reject) => {
    const cancel = () => reject(aborted());
    signal.addEventListener("abort", cancel, { once: true });
    let permission;
    try {
      permission = navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (error) {
      signal.removeEventListener("abort", cancel);
      reject(error);
      return;
    }
    permission.then(
      (stream) => {
        signal.removeEventListener("abort", cancel);
        if (signal.aborted) {
          stopTracks(stream);
          reject(aborted());
        } else resolve(stream);
      },
      (error) => {
        signal.removeEventListener("abort", cancel);
        reject(error);
      },
    );
  });
}

/** Decodes browser-supported audio to mono PCM16 WAV at the capture rate, trimmed to the limit. */
export async function toWav(blob, signal, capture = dictationCapture) {
  const { sampleRate, maxSeconds } = capture;
  signal.throwIfAborted();
  const context = new AudioContext();
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    context.close().catch(() => {});
  };
  signal.addEventListener("abort", close, { once: true });
  try {
    const decoded = await context.decodeAudioData(await blob.arrayBuffer());
    signal.throwIfAborted();
    if (!Number.isFinite(decoded.duration) || decoded.duration <= 0) throw new Error("No audio was recorded.");
    // Trim encoder padding and delayed final chunks at the recording limit.
    const frames = Math.min(sampleRate * maxSeconds, Math.ceil(decoded.duration * sampleRate));
    const offline = new OfflineAudioContext(1, frames, sampleRate);
    const source = offline.createBufferSource();
    source.buffer = decoded;
    try {
      source.connect(offline.destination);
      source.start();
      const rendered = await offline.startRendering();
      signal.throwIfAborted();
      return encodePcm16Wav(rendered.getChannelData(0), capture);
    } finally {
      source.disconnect();
    }
  } finally {
    signal.removeEventListener("abort", close);
    close();
  }
}

/** Manual and automatic stops resolve finished; cancellation rejects with AbortError. */
export async function startRecording({ signal, capture = dictationCapture }) {
  signal.throwIfAborted();
  if (typeof navigator === "undefined" || !navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === "undefined" ||
    typeof AudioContext === "undefined" || typeof OfflineAudioContext === "undefined")
    throw new Error("Microphone dictation requires a supported browser on HTTPS or localhost.");
  const mimeType = ["audio/webm;codecs=opus", "audio/webm", "audio/mp4"].find((type) => MediaRecorder.isTypeSupported(type));
  if (!mimeType) throw new Error("This browser cannot record supported microphone audio.");
  const stream = await microphone(signal);
  if (signal.aborted) {
    stopTracks(stream);
    throw aborted();
  }
  let recorder;
  try {
    recorder = new MediaRecorder(stream, { mimeType });
  } catch (error) {
    stopTracks(stream);
    throw error;
  }
  const conversion = new AbortController();
  const chunks = [];
  let size = 0;
  let settled = false;
  let stopping = false;
  let timer;
  let resolveResult, rejectResult;
  const result = new Promise((resolve, reject) => {
    resolveResult = resolve;
    rejectResult = reject;
  });
  // Recorder events may fail before the caller awaits finished.
  result.catch(() => {});
  let released = false;
  const release = () => {
    clearTimeout(timer);
    if (released) return;
    released = true;
    stopTracks(stream);
  };
  const finish = (error) => {
    if (settled) return;
    settled = true;
    signal.removeEventListener("abort", cancel);
    conversion.abort();
    recorder.ondataavailable = null;
    recorder.onstop = null;
    recorder.onerror = null;
    chunks.length = 0;
    try {
      if (recorder.state !== "inactive") recorder.stop();
    } catch {
      // Tracks still need releasing if the recorder has failed.
    }
    release();
    rejectResult(error);
  };
  const cancel = () => finish(aborted());
  const stop = () => {
    if (!stopping && !settled) {
      stopping = true;
      try {
        if (recorder.state !== "inactive") recorder.stop();
      } catch (error) {
        finish(error);
      }
      release();
    }
  };
  recorder.ondataavailable = ({ data }) => {
    if (settled) return;
    size += data.size;
    if (size > MAX_BYTES) {
      finish(new Error("Dictation exceeds the 16 MiB recording limit."));
      return;
    }
    if (data.size) chunks.push(data);
  };
  recorder.onerror = () => finish(new Error("Microphone recording failed. Please try again."));
  recorder.onstop = () => {
    release();
    recorder.onstop = null;
    recorder.ondataavailable = null;
    const blob = new Blob(chunks, { type: recorder.mimeType });
    chunks.length = 0;
    if (!blob.size) {
      finish(new Error("No audio was recorded."));
      return;
    }
    toWav(blob, conversion.signal, capture).then((wav) => {
      if (settled) return;
      settled = true;
      recorder.onerror = null;
      signal.removeEventListener("abort", cancel);
      resolveResult(wav);
    }, finish);
  };
  signal.addEventListener("abort", cancel, { once: true });
  if (signal.aborted) {
    cancel();
    throw aborted();
  }
  try {
    recorder.start(250);
    if (!settled) timer = setTimeout(stop, capture.maxSeconds * 1000);
  } catch (error) {
    finish(error);
    throw error;
  }
  return { stop, cancel, finished: result };
}

/** Why dictation can't start here, or "" when it can. */
export function dictationUnavailable(modelId) {
  return !globalThis.isSecureContext
    ? "Microphone access needs HTTPS or localhost. Open the HTTPS app to dictate."
    : !navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === "undefined"
      ? "This browser does not support microphone recording."
      : !modelId
        ? "Enable and install a transcription model in Manage models."
        : "";
}

/** Transcribes a recording as the signed-in person; resolves to the text. */
export async function transcribe(audio, modelId, signal, apiError) {
  const form = new FormData();
  form.set("model", modelId);
  form.set("response_format", "json");
  form.set("file", audio, "dictation.wav");
  const response = await fetch("/app/api/v1/audio/transcriptions", {
    method: "POST",
    body: form,
    signal,
    credentials: "same-origin",
    cache: "no-store",
    headers: { "X-Localbase-UI": "1" },
  });
  if (!response.ok) throw new Error(await apiError(response));
  const body = await response.json();
  if (typeof body?.text !== "string") throw new Error("The gateway returned an invalid transcription.");
  return body.text;
}
