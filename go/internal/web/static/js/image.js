// Image only (loaded on the Image tab, as video.js is on Video): the picture viewer, and Edit
// with a picture as its source. Every other tool loads lab.js without it.
import { fill, syncChips } from "./lab-prompt.js";

// --- the picture viewer ---------------------------------------------------------------
// One <dialog> per picture; previous and next move between them.
const viewers = () => [...document.querySelectorAll("#lab-items dialog.viewer")];
const MAT = "localbase.viewer.mat.v1";

// Inspect: a tap, the button or Z doubles the picture, centered where it was tapped. Pinching is
// the browser's own. It's always the whole picture again when you move on.
function setZoom(dialog, on, x = 0.5, y = 0.5) {
  const stage = dialog.querySelector(".viewer-stage");
  const button = dialog.querySelector("[data-view-zoom]");
  if (!stage || !button) return;
  stage.classList.toggle("viewer-zoomed", on);
  button.setAttribute("aria-pressed", String(on));
  button.setAttribute("aria-label", on ? "Zoom out" : "Zoom in");
  if (on) {
    stage.scrollLeft = stage.scrollWidth * x - stage.clientWidth / 2;
    stage.scrollTop = stage.scrollHeight * y - stage.clientHeight / 2;
  }
}

// The mat is a way of looking at pictures, so it stays with this browser, not one picture.
function matOn() {
  try {
    return localStorage.getItem(MAT) === "1";
  } catch {
    return false;
  }
}

function paintMat(dialog, on) {
  dialog.querySelector(".viewer-stage")?.classList.toggle("viewer-matted", on);
  const button = dialog.querySelector("[data-view-mat]");
  if (!button) return;
  button.setAttribute("aria-pressed", String(on));
  button.setAttribute("aria-label", on ? "Show without a mat" : "Show on a mat");
}

function paintViewer(dialog) {
  setZoom(dialog, false);
  paintMat(dialog, matOn());
  const all = viewers();
  const at = all.indexOf(dialog);
  const count = dialog.querySelector("[data-view-count]");
  if (count) count.textContent = all.length > 1 ? `${at + 1} of ${all.length}` : "";
  for (const b of dialog.querySelectorAll("[data-view-step]")) b.hidden = all.length < 2;
}

function stepViewer(dialog, by) {
  const all = viewers();
  if (all.length < 2) return;
  const next = all[(all.indexOf(dialog) + by + all.length) % all.length];
  dialog.dataset.stepping = "1";
  dialog.close();
  next.showModal();
  paintViewer(next);
}

// Leaving for the prompt: closing must not send focus back to the picture.
function leaveViewer(dialog) {
  if (!dialog) return;
  dialog.dataset.stepping = "1";
  dialog.close();
}

// Closing goes back to the picture you were on, even after moving between them.
document.addEventListener("close", (event) => {
  const dialog = event.target;
  if (!dialog.matches?.("dialog.viewer")) return;
  setZoom(dialog, false);
  if (dialog.dataset.stepping) {
    delete dialog.dataset.stepping;
    return;
  }
  document.querySelector(`.image-open[data-open-sheet="${dialog.id}"]`)?.focus();
}, true);

document.addEventListener("click", (event) => {
  // Details: a plain button and panel, so the layout is the same in every browser.
  const details = event.target.closest?.("[data-view-details]");
  if (details) {
    const open = details.getAttribute("aria-expanded") !== "true";
    details.setAttribute("aria-expanded", String(open));
    document.getElementById(details.getAttribute("aria-controls")).hidden = !open;
    return;
  }
  const zoom = event.target.closest?.("[data-view-zoom]");
  if (zoom) {
    setZoom(zoom.closest("dialog"), zoom.getAttribute("aria-pressed") !== "true");
    return;
  }
  const mat = event.target.closest?.("[data-view-mat]");
  if (mat) {
    const on = mat.getAttribute("aria-pressed") !== "true";
    try {
      localStorage.setItem(MAT, on ? "1" : "0");
    } catch {
      // Blocked storage: it still works until the page is left.
    }
    paintMat(mat.closest("dialog"), on);
    return;
  }
  if (event.target.matches?.(".viewer-img")) {
    const dialog = event.target.closest("dialog");
    const box = event.target.getBoundingClientRect();
    setZoom(dialog, !dialog.querySelector(".viewer-zoomed"), (event.clientX - box.left) / box.width, (event.clientY - box.top) / box.height);
    return;
  }
  const step = event.target.closest?.("[data-view-step]");
  if (step) {
    stepViewer(step.closest("dialog"), Number(step.dataset.viewStep));
    return;
  }
  const open = event.target.closest?.(".image-open");
  if (open) {
    const dialog = document.getElementById(open.dataset.openSheet);
    if (dialog) paintViewer(dialog);
    return;
  }
  const copy = event.target.closest?.("[data-copy-text]");
  if (copy) {
    navigator.clipboard?.writeText(copy.dataset.copyText);
    const before = copy.textContent;
    copy.textContent = "Copied";
    setTimeout(() => (copy.textContent = before), 1500);
    return;
  }
  // Reuse settings: the form takes this picture's prompt, shape, look and seed,
  // and what it was edited from while that is kept.
  const reuse = event.target.closest?.("[data-reuse]");
  if (reuse) {
    const { prompt: text, shape, look, seed, from, change } = reuse.dataset;
    const form = document.querySelector("form.lab-dock");
    if (!form) return;
    chooseRadios(form, [["shape", shape], ["look", look], ["change", change]]);
    if (form.seed) form.seed.value = seed;
    setSource(form, from);
    syncChips(form);
    leaveViewer(reuse.closest("dialog"));
    fill(text);
    return;
  }
  // Edit: this picture becomes the start of a new one, with its words to change.
  const edit = event.target.closest?.("[data-edit-picture]");
  if (edit) {
    const form = document.querySelector("form.lab-dock");
    if (!canEdit(form)) {
      document.getElementById("image-needs-edit")?.showModal();
      return;
    }
    chooseRadios(form, [["look", edit.dataset.look], ["change", edit.dataset.change]]);
    setSource(form, edit.dataset.editPicture);
    leaveViewer(edit.closest("dialog"));
    fill(edit.dataset.prompt ?? "");
    return;
  }
  const clear = event.target.closest?.("[data-source-clear]");
  if (clear) {
    setSource(clear.closest("form"));
    document.getElementById("lab-prompt")?.focus({ preventScroll: true });
  }
});

function chooseRadios(form, pairs) {
  for (const [name, value] of pairs) {
    if (value === undefined) continue;
    const radio = [...form.querySelectorAll(`input[type=radio][name="${name}"]`)].find((r) => r.value === value);
    if (radio) radio.checked = true;
  }
}

const canEdit = (form) => !!form?.querySelector('[name="model"][data-edits]');

// The picture being edited, or none. While editing, only models that can edit
// are on offer, and stopping goes back to the model that was chosen.
function setSource(form, url = "") {
  const row = form.querySelector("[data-image-source]");
  if (!row) return;
  const editing = !!url && canEdit(form);
  const thumb = row.querySelector("img");
  if (editing) thumb.src = url;
  else thumb.removeAttribute("src");
  row.hidden = !editing;
  form.elements.source.value = editing ? url.split("/").pop() : "";
  form.toggleAttribute("data-editing", editing);
  document.getElementById("lab-prompt").placeholder = editing ? "Describe what to change" : "Describe the picture";
  form.querySelector("[data-source-status]").textContent = editing ? "Editing this picture. Change the words below." : "";
  const models = [...form.querySelectorAll('input[type=radio][name="model"]')];
  const chosen = models.find((r) => r.checked);
  if (editing && chosen && !chosen.hasAttribute("data-edits")) {
    form.dataset.modelBefore = chosen.value;
    models.find((r) => r.hasAttribute("data-edits")).checked = true;
  } else if (!editing && form.dataset.modelBefore) {
    const before = models.find((r) => r.value === form.dataset.modelBefore);
    if (before) before.checked = true;
    delete form.dataset.modelBefore;
  }
  for (const radio of models) {
    const off = editing && !radio.hasAttribute("data-edits");
    radio.disabled = off;
    const note = radio.closest("label")?.querySelector("[data-cant-edit]");
    if (note) note.hidden = !off;
  }
  syncChips(form);
}

// Once a run has started, the next picture starts fresh.
document.addEventListener("htmx:afterRequest", (event) => {
  const form = event.target;
  if (!form.matches?.("form.lab-dock") || !form.elements.source?.value) return;
  if (event.detail.xhr?.getResponseHeader("HX-Retarget") === "#lab-items") setSource(form);
});

document.addEventListener("keydown", (event) => {
  const dialog = document.querySelector("dialog.viewer[open]");
  if (!dialog || event.target.closest?.("input, textarea")) return;
  const zoomed = dialog.querySelector(".viewer-zoomed");
  if (event.key === "z") setZoom(dialog, !zoomed);
  // Zoomed in, the arrows move around the picture; otherwise between pictures.
  if (zoomed && event.key.startsWith("Arrow")) {
    event.preventDefault();
    const by = { ArrowLeft: [-80, 0], ArrowRight: [80, 0], ArrowUp: [0, -80], ArrowDown: [0, 80] }[event.key];
    if (by) zoomed.scrollBy(by[0], by[1]);
    return;
  }
  if (event.key === "ArrowLeft") stepViewer(dialog, -1);
  if (event.key === "ArrowRight") stepViewer(dialog, 1);
});
