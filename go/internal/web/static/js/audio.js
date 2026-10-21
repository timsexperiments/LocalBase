// Audio inputs on the Lab and My voices pages (forms marked data-audio-form):
// record from the microphone or pick a file, then replace the file input's
// value with a mono 16-bit WAV, the only format the server decodes. The
// browser does the decoding, so anything it can play works.
// data-audio-rate on the form resamples (transcription wants 16 kHz).

function encodeWav(samples, rate) {
  const out = new DataView(new ArrayBuffer(44 + samples.length * 2));
  const ascii = (at, s) => [...s].forEach((c, i) => out.setUint8(at + i, c.charCodeAt(0)));
  ascii(0, "RIFF");
  out.setUint32(4, 36 + samples.length * 2, true);
  ascii(8, "WAVEfmt ");
  out.setUint32(16, 16, true);
  out.setUint16(20, 1, true);
  out.setUint16(22, 1, true);
  out.setUint32(24, rate, true);
  out.setUint32(28, rate * 2, true);
  out.setUint16(32, 2, true);
  out.setUint16(34, 16, true);
  ascii(36, "data");
  out.setUint32(40, samples.length * 2, true);
  samples.forEach((s, i) => {
    const v = Math.max(-1, Math.min(1, s));
    out.setInt16(44 + i * 2, v < 0 ? v * 32768 : v * 32767, true);
  });
  return new Blob([out], { type: "audio/wav" });
}

async function toWav(blob, rate) {
  const context = new AudioContext();
  let decoded;
  try {
    decoded = await context.decodeAudioData(await blob.arrayBuffer());
  } finally {
    context.close();
  }
  const target = rate || decoded.sampleRate;
  const frames = Math.ceil(decoded.duration * target);
  const offline = new OfflineAudioContext(1, Math.max(frames, 1), target);
  const source = offline.createBufferSource();
  source.buffer = decoded;
  source.connect(offline.destination); // mixes down to the one channel
  source.start();
  const rendered = await offline.startRendering();
  return { wav: encodeWav(rendered.getChannelData(0), target), seconds: decoded.duration };
}

function setup(form) {
  const input = form.querySelector("[data-audio-input]");
  const status = form.querySelector("[data-audio-status]");
  const preview = form.querySelector("[data-audio-preview]");
  const record = form.querySelector("[data-record]");
  const rate = Number(form.dataset.audioRate) || 0;
  let recorder;

  const say = (text) => { if (status) status.textContent = text; };
  const use = async (blob, name) => {
    say("Converting…");
    try {
      const { wav, seconds } = await toWav(blob, rate);
      const transfer = new DataTransfer();
      transfer.items.add(new File([wav], name.replace(/\.[^.]*$/, "") + ".wav", { type: "audio/wav" }));
      input.files = transfer.files;
      if (preview) {
        if (preview.src) URL.revokeObjectURL(preview.src);
        preview.src = URL.createObjectURL(wav);
        preview.classList.remove("hidden");
      }
      say(`Ready: ${seconds.toFixed(1)} s, converted to WAV.`);
    } catch {
      input.value = "";
      say("This browser could not read that audio. Try a WAV or MP3 file.");
    }
  };

  input?.addEventListener("change", () => {
    const file = input.files?.[0];
    if (file && (rate || file.type !== "audio/wav")) use(file, file.name);
    else if (file) say("Ready.");
  });

  if (record) {
    if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) {
      record.hidden = true;
    } else {
      record.addEventListener("click", async () => {
        if (recorder?.state === "recording") {
          recorder.stop();
          return;
        }
        let stream;
        try {
          stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        } catch {
          say("Microphone access was not allowed.");
          return;
        }
        const chunks = [];
        recorder = new MediaRecorder(stream);
        recorder.addEventListener("dataavailable", (e) => chunks.push(e.data));
        recorder.addEventListener("stop", () => {
          stream.getTracks().forEach((t) => t.stop());
          record.textContent = "Record";
          record.removeAttribute("aria-pressed");
          use(new Blob(chunks, { type: recorder.mimeType }), "recording");
        });
        recorder.start();
        record.textContent = "Stop";
        record.setAttribute("aria-pressed", "true");
        say("Recording… press Stop when done.");
      });
    }
  }

  if (form.hasAttribute("data-reset-on-success")) {
    form.addEventListener("htmx:afterRequest", (event) => {
      if (event.detail.successful && !document.querySelector("#voice-list [role=alert]")) {
        form.reset();
        preview?.classList.add("hidden");
        say("");
      }
    });
  }
}

const init = () => document.querySelectorAll("form[data-audio-form]:not([data-audio-ready])").forEach((form) => {
  form.setAttribute("data-audio-ready", "");
  setup(form);
});
init();
document.addEventListener("htmx:load", init);
