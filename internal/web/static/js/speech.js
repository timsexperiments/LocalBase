// The Speech studio (DESIGN §22.19): the count beside the script, voice samples,
// and the drawn waveform following the native player. Scripts are made on the
// gateway; nothing here is needed for that but the count and the samples.

import "./icon-names.js";

const LIMIT = 5000;
const PART = 200;
const format = new Intl.NumberFormat();

// How many parts the gateway will make, packing sentences the way it does
// (splitScript in labspeech.go). Only a count: the gateway does the cutting.
export function countParts(text, limit = PART) {
  let parts = 0;
  let room = 0;
  for (const sentence of text.split(/(?<=[.!?…。！？\n])\s*/u)) {
    let size = [...sentence.trim()].length;
    if (!size) continue;
    if (size > limit) {
      parts += Math.ceil(size / limit) - 1;
      size = size % limit || limit;
      room = 0;
    }
    if (room && room + 1 + size <= limit) room += 1 + size;
    else {
      parts += 1;
      room = size;
    }
  }
  return parts;
}

// Mostly digits, and more than a few of them.
export function mostlyDigits(text) {
  const marks = [...text].filter((c) => !/\s/.test(c));
  const digits = marks.filter((c) => /\p{Nd}/u.test(c)).length;
  return digits >= 12 && digits * 2 > marks.length;
}

function syncCount() {
  const area = document.querySelector("form[aria-label='Make speech'] textarea");
  const count = document.querySelector("[data-script-count]");
  if (!area || !count) return;
  const note = document.querySelector("[data-digits-note]");
  if (note) note.hidden = !mostlyDigits(area.value);
  const n = [...area.value].length;
  count.hidden = n <= PART;
  if (n <= PART) return;
  const parts = countParts(area.value);
  count.classList.toggle("text-danger", n > LIMIT);
  count.textContent = n >= LIMIT * 0.8 ? `${format.format(n)} / ${format.format(LIMIT)}` : `About ${parts} ${parts === 1 ? "part" : "parts"}`;
}

document.addEventListener("input", (event) => {
  if (event.target.matches?.("form[aria-label='Make speech'] textarea")) syncCount();
});
document.addEventListener("htmx:afterSettle", syncCount);
syncCount();

// One sample at a time. Choosing a voice and playing it are separate controls.
const sample = new Audio();
let playing = null;

function show(button, on) {
  button?.setAttribute("aria-pressed", String(on));
}

function sayVoice(text) {
  const status = document.querySelector("[data-voice-status]");
  if (status) status.textContent = text;
}

document.addEventListener("click", (event) => {
  const button = event.target.closest?.("[data-voice-preview]");
  if (button) {
    if (playing === button && !sample.paused) {
      sample.pause();
      return;
    }
    show(playing, false);
    playing = button;
    sample.src = button.dataset.voicePreview;
    show(button, true);
    sayVoice("Getting the sample…");
    sample.play().then(
      () => sayVoice("Playing the sample."),
      () => {
        show(button, false);
        sayVoice("This sample could not be made. Is the model available?");
      },
    );
    return;
  }
  const wave = event.target.closest?.("[data-wave]");
  if (wave) {
    const audio = wave.parentElement.querySelector("audio");
    if (!audio?.duration) return;
    const box = wave.getBoundingClientRect();
    audio.currentTime = audio.duration * Math.min(1, Math.max(0, (event.clientX - box.left) / box.width));
  }
});
sample.addEventListener("pause", () => show(playing, false));
sample.addEventListener("ended", () => {
  show(playing, false);
  sayVoice("");
});
// A closed sheet stops its sample.
document.addEventListener("close", (event) => {
  if (event.target.id === "lab-voice") sample.pause();
}, true);

// The waveform follows the player. timeupdate does not bubble: capture it.
document.addEventListener("timeupdate", (event) => {
  const audio = event.target;
  const wave = audio.parentElement?.querySelector?.("[data-wave]");
  if (wave && audio.duration) wave.style.setProperty("--played", `${(audio.currentTime / audio.duration) * 100}%`);
}, true);
