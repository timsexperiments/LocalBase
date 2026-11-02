// The Transcribe studio (DESIGN §22.20): recording with a level, and a transcript
// whose times move the player. Converting and sending is audio.js. The level is
// reassurance that it can hear you, not a measurement.

const MAX_SECONDS = 13 * 60; // what fits the upload limit as 16 kHz WAV
const QUIET = 0.01;
const LOUD = 0.95;
const QUIET_AFTER_MS = 3000;

let session = null;

const form = () => document.querySelector("form[data-transcribe]");
const q = (selector) => form()?.querySelector(selector);

function say(text) {
  const status = q("[data-audio-status]");
  if (status) status.textContent = text;
}

function showPanel(on) {
  const start = q("[data-transcribe-start]");
  const panel = q("[data-record-panel]");
  if (start) start.hidden = on;
  if (panel) panel.hidden = !on;
}

const clock = (seconds) => `${Math.floor(seconds / 60)}:${String(Math.floor(seconds % 60)).padStart(2, "0")}`;

function elapsed(s) {
  return (s.banked + (s.resumedAt ? performance.now() - s.resumedAt : 0)) / 1000;
}

function paint() {
  const s = session;
  if (!s) return;
  const seconds = elapsed(s);
  const time = q("[data-record-time]");
  if (time) time.textContent = clock(seconds);
  if (seconds >= MAX_SECONDS) {
    say("That is the most that fits at a time, so it stopped.");
    s.recorder.stop();
    return;
  }
  if (s.recorder.state === "recording") {
    s.analyser.getFloatTimeDomainData(s.samples);
    let sum = 0, peak = 0;
    for (const v of s.samples) {
      sum += v * v;
      peak = Math.max(peak, Math.abs(v));
    }
    const rms = Math.sqrt(sum / s.samples.length);
    // Up at once, down calmly.
    s.level = Math.max(Math.min(1, rms * 4), s.level * 0.92);
    const bar = q("[data-record-level]");
    if (bar) bar.style.width = `${Math.round(s.level * 100)}%`;
    const now = performance.now();
    if (rms >= QUIET) {
      s.heardAt = now;
      s.heard = true;
    }
    const word = peak >= LOUD ? "Too loud" : now - s.heardAt > QUIET_AFTER_MS ? "Very quiet" : s.heard ? "Hearing you" : "Listening…";
    const box = q("[data-record-word]");
    if (box && box.textContent !== word) box.textContent = word;
  }
  s.frame = requestAnimationFrame(paint);
}

function finish() {
  const s = session;
  if (!s) return;
  session = null;
  cancelAnimationFrame(s.frame);
  s.stream.getTracks().forEach((t) => t.stop());
  s.context.close().catch(() => {});
  showPanel(false);
  return s;
}

async function startRecording() {
  if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) {
    say("This browser can’t record here. Choose a file instead.");
    return;
  }
  let stream;
  say("Allow the microphone to start recording.");
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch (error) {
    say(error?.name === "NotAllowedError"
      ? "The microphone is off for this site. Turn it on in the browser’s site settings, or choose a file."
      : "No microphone was found. Choose a file instead.");
    return;
  }
  const context = new AudioContext();
  const analyser = context.createAnalyser();
  analyser.fftSize = 1024;
  context.resume().catch(() => {});
  context.createMediaStreamSource(stream).connect(analyser);
  const chunks = [];
  const recorder = new MediaRecorder(stream);
  session = { stream, context, analyser, recorder, samples: new Float32Array(analyser.fftSize), level: 0, banked: 0, resumedAt: performance.now(), heardAt: performance.now(), frame: 0 };
  recorder.addEventListener("dataavailable", (e) => chunks.push(e.data));
  recorder.addEventListener("stop", () => {
    const s = finish();
    if (!s || s.discard) return;
    const input = q("[data-audio-input]");
    const transfer = new DataTransfer();
    transfer.items.add(new File([new Blob(chunks, { type: recorder.mimeType })], "recording", { type: recorder.mimeType }));
    input.files = transfer.files;
    input.dispatchEvent(new Event("change", { bubbles: true }));
  });
  recorder.start();
  say("");
  const pause = q("[data-record-pause]");
  if (pause) pause.textContent = "Pause";
  const word = q("[data-record-word]");
  if (word) word.textContent = "Listening…";
  showPanel(true);
  q("[data-record-stop]")?.focus();
  paint();
}

function togglePause(button) {
  const s = session;
  if (!s) return;
  if (s.recorder.state === "recording") {
    s.recorder.pause();
    s.banked += performance.now() - s.resumedAt;
    s.resumedAt = 0;
    button.textContent = "Resume";
    const word = q("[data-record-word]");
    if (word) word.textContent = "Paused";
    const bar = q("[data-record-level]");
    if (bar) bar.style.width = "0";
  } else {
    s.recorder.resume();
    s.resumedAt = performance.now();
    s.heardAt = performance.now();
    button.textContent = "Pause";
  }
}

document.addEventListener("click", (event) => {
  const t = event.target.closest?.("button");
  if (!t) return;
  if (t.matches("[data-record-start]")) startRecording();
  else if (t.matches("[data-pick-audio]")) q("[data-audio-input]")?.click();
  else if (t.matches("[data-record-pause]")) togglePause(t);
  else if (t.matches("[data-record-stop]")) session?.recorder.stop();
  else if (t.matches("[data-seek]")) {
    const audio = t.closest("li")?.querySelector("[data-transcript-audio]");
    if (audio) {
      audio.currentTime = Number(t.dataset.seek);
      audio.play().catch(() => {});
    }
  } else if (t.matches("[data-toggle-times]")) {
    const off = t.getAttribute("aria-pressed") === "true";
    t.setAttribute("aria-pressed", String(!off));
    t.closest("li")?.querySelector("[data-times]")?.classList.toggle("times-off", off);
  }
});

// Leaving while recording: a boosted link waits, and a full navigation asks.
document.addEventListener("htmx:confirm", (event) => {
  if (!session || event.detail.elt?.closest?.("form[data-transcribe]")) return;
  if (event.detail.elt?.tagName === "A") {
    event.preventDefault();
    say("Stop the recording first, or it will be lost.");
  }
});
window.addEventListener("beforeunload", (event) => {
  if (session) event.preventDefault();
});

// A file dropped anywhere on a wide screen.
document.addEventListener("dragover", (event) => {
  if (form() && event.dataTransfer?.types?.includes("Files")) event.preventDefault();
});
document.addEventListener("drop", (event) => {
  const file = event.dataTransfer?.files?.[0];
  const input = q("[data-audio-input]");
  if (!file || !input) return;
  event.preventDefault();
  const transfer = new DataTransfer();
  transfer.items.add(file);
  input.files = transfer.files;
  input.dispatchEvent(new Event("change", { bubbles: true }));
});
