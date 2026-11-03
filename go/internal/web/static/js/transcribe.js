import "./icon-names.js";

// The Transcribe studio (DESIGN §22.20): recording with a level, and a transcript
// whose times move the player. Converting and sending is audio.js. The level is
// reassurance that it can hear you, not a measurement.

const MAX_SECONDS = 13 * 60; // what fits the upload limit as 16 kHz WAV
const QUIET = 0.01;
const LOUD = 0.95;
const QUIET_AFTER_MS = 3000;

let session = null;
// What was last sent, kept until it has become a run: a failed send or a page change
// must not lose a recording. In memory only, for as long as this page lives.
let kept = null;

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

function showKept() {
  const box = q("[data-keep]");
  const start = q("[data-transcribe-start]");
  if (!box) return;
  const waiting = kept?.failed;
  box.hidden = !waiting;
  if (start && !session) start.hidden = !!waiting;
  if (waiting) {
    q("[data-keep-said]").textContent = kept.recorded ? "Your recording is still here, send again." : "Your file is still here, send again.";
    say("");
  }
}

// Put the kept audio back and send it. It is already converted.
function sendKept() {
  const input = q("[data-audio-input]");
  if (!kept || !input) return;
  const transfer = new DataTransfer();
  transfer.items.add(kept.file);
  input.files = transfer.files;
  kept.failed = false;
  showKept();
  say("Sending…");
  form().requestSubmit();
}

document.addEventListener("htmx:beforeRequest", (event) => {
  if (!event.detail.elt?.matches?.("form[data-transcribe]")) return;
  const file = q("[data-audio-input]")?.files?.[0];
  if (file) kept = { file, recorded: file.name.startsWith("recording"), failed: false };
});

document.addEventListener("htmx:afterRequest", (event) => {
  if (!event.detail.elt?.matches?.("form[data-transcribe]") || !kept) return;
  // A refusal comes back as an alert in the result; a run joins the tray.
  const refused = event.detail.successful && event.detail.target?.id === "lab-result";
  if (event.detail.successful && !refused) {
    kept = null;
    say("");
    return;
  }
  kept.failed = true;
  showKept();
  if (!event.detail.successful) {
    const said = q("[data-keep-said]");
    if (said) said.textContent += " LocalBase could not be reached.";
  }
});
document.addEventListener("htmx:afterSettle", () => {
  if (kept?.failed) showKept();
});

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
  if (t.matches("[data-keep-send]")) sendKept();
  else if (t.matches("[data-keep-drop]")) {
    kept = null;
    q("[data-keep]").hidden = true;
    q("[data-transcribe-start]").hidden = false;
    const input = q("[data-audio-input]");
    if (input) input.value = "";
  } else if (t.matches("[data-record-start]")) startRecording();
  else if (t.matches("[data-pick-audio]")) q("[data-audio-input]")?.click();
  else if (t.matches("[data-record-pause]")) togglePause(t);
  else if (t.matches("[data-record-stop]")) session?.recorder.stop();
  else if (t.matches("[data-seek]")) {
    const audio = t.closest("li")?.querySelector("[data-transcript-audio]");
    if (audio) {
      audio.currentTime = Number(t.dataset.seek);
      audio.play().catch(() => {});
    }
  } else if (t.closest?.("[data-toggle-times]")) {
    const off = t.getAttribute("aria-pressed") === "true";
    t.setAttribute("aria-pressed", String(!off));
    t.closest("li")?.querySelector("[data-times]")?.classList.toggle("times-off", off);
  }
});

// A model that hears English only has no language to choose and nothing to translate.
function syncLanguage() {
  const f = form();
  if (!f) return;
  const model = f.querySelector("input[name=model]:checked, input[type=hidden][name=model]");
  const english = !!model?.hasAttribute("data-english-only");
  const chip = f.querySelector("[data-open-sheet=lab-language]");
  const note = f.querySelector("[data-english-note]");
  if (chip) chip.hidden = english;
  if (note) note.hidden = !english;
  if (english) {
    const detect = f.querySelector("input[name=language][value=auto]");
    if (detect) detect.checked = true;
    const translate = f.querySelector("[name=translate]");
    if (translate) translate.checked = false;
    chip?.querySelector("[data-chip-label]")?.replaceChildren("Detect language");
  }
}
document.addEventListener("change", (event) => {
  if (event.target.matches?.("form[data-transcribe] input[name=model]")) syncLanguage();
});
document.addEventListener("htmx:afterSettle", syncLanguage);
syncLanguage();

// Leaving while recording: a boosted link waits, and a full navigation asks.
document.addEventListener("htmx:confirm", (event) => {
  if (!session || event.detail.elt?.closest?.("form[data-transcribe]")) return;
  if (event.detail.elt?.tagName === "A") {
    event.preventDefault();
    say("Stop the recording first, or it will be lost.");
  }
});
// A full reload ends this page's memory too, so a recording not yet sent also asks.
window.addEventListener("beforeunload", (event) => {
  if (session || kept) event.preventDefault();
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
