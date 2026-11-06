// Generation controls (TS GenerationSettingsFields) for the chat model and
// each tool model. Blank fields use the model's defaults. Settings stay in
// memory for this page, as in TS.
import { imageLora, imageSizes } from "./tools.js";

const el = (tag, props = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") node.className = v;
    else if (k in node) node[k] = v;
    else node.setAttribute(k, v);
  }
  node.append(...children.filter((c) => c !== null && c !== undefined && c !== false));
  return node;
};

let fieldId = 0;
function field(label, control) {
  control.id = `gen-${++fieldId}`;
  return el("div", { class: "grid gap-1" }, el("label", { class: "text-sm font-semibold", htmlFor: control.id }, label), control);
}

const hint = (text, alert = false) => el("p", { class: `text-sm ${alert ? "text-danger" : "text-muted"}`, ...(alert ? { role: "alert" } : {}) }, text);

// A number input: blank means the model default; out-of-range values are
// clamped when the field loses focus (TS NumberField).
function number(label, value, { min, max, step = 1 }, onChange) {
  const input = el("input", { class: "input", type: "number", inputMode: step === 1 ? "numeric" : "decimal", min, step, placeholder: "Model default", value: value ?? "" });
  if (max !== undefined) input.max = max;
  input.addEventListener("input", () => {
    if (!input.value) onChange(undefined);
    else if (Number.isFinite(input.valueAsNumber)) onChange(input.valueAsNumber);
  });
  input.addEventListener("blur", () => {
    if (!input.value || !Number.isFinite(input.valueAsNumber)) return;
    const n = Math.min(max ?? Number.MAX_SAFE_INTEGER, Math.max(min, step === 1 ? Math.round(input.valueAsNumber) : input.valueAsNumber));
    input.value = String(n);
    onChange(n);
  });
  return field(label, input);
}

function voiceLabel(model, voice) {
  const cap = model.catalog.capabilities;
  if (cap?.kind !== "speech") return voice;
  return ("custom" in cap.voice ? cap.voice.custom?.find(({ requestValue }) => requestValue === voice)?.name : undefined) ?? voice;
}

/** The fields for one model; onChange receives the whole updated settings. */
export function settingsFields(mode, model, initial, report) {
  const cap = model.catalog.capabilities;
  // Fields share the latest settings, so one edit never undoes another.
  let settings = initial;
  const onChange = (next) => {
    settings = next;
    report(next);
  };
  const set = (part, values) => onChange({ ...settings, [part]: { ...settings[part], ...values } });
  const nodes = [];
  if (mode === "llm") {
    nodes.push(
      number("Temperature", settings.llm.temperature, { min: 0, max: 2, step: 0.1 }, (temperature) => set("llm", { temperature })),
      number("Maximum output tokens", settings.llm.max_tokens, { min: 1 }, (max_tokens) => set("llm", { max_tokens })),
    );
    if (model.catalog.contextWindowTokens)
      nodes.push(hint(`Catalog context window: ${model.catalog.contextWindowTokens.toLocaleString()} tokens, shared by input and output. Runtime limits may be lower.`));
  }
  if (mode === "image") {
    const size = el("select", { class: "input" }, el("option", { value: "" }, "Model default"),
      ...imageSizes.map((s) => el("option", { value: s.value, selected: settings.image.size === s.value }, s.label)));
    size.addEventListener("change", () => set("image", { size: size.value || undefined }));
    const negative = el("textarea", { class: "input min-h-20", maxLength: 16384, placeholder: "What to avoid, e.g. blurry, extra fingers", value: settings.image.negative_prompt ?? "" });
    negative.addEventListener("input", () => set("image", { negative_prompt: negative.value || undefined }));
    nodes.push(field("Image size", size), field("Negative prompt", negative),
      number("Steps", settings.image.steps, { min: 1, max: 150 }, (steps) => set("image", { steps })),
      number("CFG scale", settings.image.cfg_scale, { min: 0, max: 30, step: 0.5 }, (cfg_scale) => set("image", { cfg_scale })),
      number("Seed", settings.image.seed, { min: 0, max: 2147483647 }, (seed) => set("image", { seed })));
    const lora = imageLora(model);
    if (lora) {
      nodes.push(number("LoRA strength", settings.image.lora_strength, { min: lora.minimum, max: lora.maximum, step: 0.05 }, (lora_strength) => set("image", { lora_strength })));
      const note = hint(`Blank uses ${lora.defaultStrength}; 0 turns the LoRA off.`);
      if (lora.trigger) note.append(" Trigger words: ", el("code", {}, lora.trigger));
      nodes.push(note);
    }
    nodes.push(hint("Blank fields use the model defaults; a blank seed is random."));
  }
  if (mode === "video" && cap?.kind === "video") {
    nodes.push(hint(`${cap.width} × ${cap.height}, ${cap.frames} frames at ${cap.fps} fps, ${(cap.frames / cap.fps).toFixed(2)} seconds. Size, duration and seed are fixed by the qualified profile.`));
    if (cap.mode !== "s2v") {
      const negative = el("textarea", { class: "input min-h-20", maxLength: 16384, placeholder: "Model default", value: settings.video.negative_prompt ?? "" });
      negative.addEventListener("input", () => onChange({ ...settings, video: { negative_prompt: negative.value || undefined } }));
      nodes.push(field("Negative prompt", negative));
    }
  }
  if (mode === "tts") {
    const voices = cap?.kind === "speech" ? cap.voice.requestValues : [];
    if (settings.tts.voice && !voices.includes(settings.tts.voice))
      nodes.push(hint(`Selected voice ${settings.tts.voice} is not available for this model. Reset to model defaults before generating.`, true));
    if (voices.length > 1) {
      const voice = el("select", { class: "input" }, el("option", { value: "" }, "Model default"),
        ...voices.map((v) => el("option", { value: v, selected: settings.tts.voice === v }, voiceLabel(model, v))));
      voice.addEventListener("change", () => onChange({ ...settings, tts: { voice: voice.value || undefined } }));
      nodes.push(field("Voice", voice));
    } else nodes.push(hint("Only the default voice is available."));
    nodes.push(hint("WAV output. Speed is fixed at 1."));
  }
  return el("div", { class: "grid gap-3" }, ...nodes);
}
