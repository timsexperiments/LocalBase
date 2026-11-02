// The Lab's docked prompt (DESIGN §22.10): option chips show what their sheet
// has selected, and the prompt grows with its text up to a cap. Results and
// runs are server-rendered (htmx); nothing here is needed to submit.

// A chip ([data-chip-label=name]) shows the label of the checked radio of
// the same name in its form.
function syncChips(root = document) {
  for (const label of root.querySelectorAll("[data-chip-label]")) {
    const form = label.closest("form");
    const checked = form?.querySelector(`input[type=radio][name="${CSS.escape(label.dataset.chipLabel)}"]:checked`);
    label.textContent = checked?.dataset.label ?? checked?.value ?? "None";
  }
}

function grow(area) {
  area.style.height = "auto";
  area.style.height = `${Math.min(area.scrollHeight, window.innerHeight * 0.3)}px`;
}

document.addEventListener("change", (event) => {
  if (event.target.matches?.("input[type=radio]")) syncChips(event.target.form ?? document);
});
document.addEventListener("input", (event) => {
  if (event.target.matches?.("textarea[data-grow]")) grow(event.target);
});
// A new voice list (after the speech model changes) selects its first voice.
document.addEventListener("htmx:afterSettle", () => syncChips());
syncChips();

// --- examples and Try again --------------------------------------------------------
// [data-fill] puts its text in the prompt (switching to its tool first when
// data-fill-tab names another one) and focuses it, without sending.
const fillKey = "localbase.lab.fill";
const currentTab = () => location.pathname.split("/").pop();

function fill(text) {
  const prompt = document.getElementById("lab-prompt");
  if (!prompt) return;
  prompt.value = text;
  grow(prompt);
  prompt.focus();
}

document.addEventListener("click", (event) => {
  const button = event.target.closest?.("[data-fill]");
  if (!button) return;
  const tab = button.dataset.fillTab;
  if (tab && tab !== currentTab()) {
    sessionStorage.setItem(fillKey, JSON.stringify({ tab, text: button.dataset.fill }));
    location.assign(`/app/studio/${encodeURIComponent(tab)}`);
    return;
  }
  fill(button.dataset.fill);
});

function fillFromStorage() {
  try {
    const saved = JSON.parse(sessionStorage.getItem(fillKey) ?? "null");
    if (saved?.tab === currentTab()) {
      sessionStorage.removeItem(fillKey);
      fill(saved.text);
    }
  } catch {
    // Unreadable: nothing to fill.
  }
}

// --- results: share and copy -------------------------------------------------------
// Share appears where the browser can share files (phones, mostly).
async function canShareFiles() {
  try {
    return !!navigator.canShare?.({ files: [new File([""], "x.png", { type: "image/png" })] });
  } catch {
    return false;
  }
}

async function revealShare() {
  if (!(await canShareFiles())) {
    // Where files cannot be shared, the Details say to use Save instead.
    for (const n of document.querySelectorAll("[data-no-share]")) n.hidden = false;
    return;
  }
  for (const b of document.querySelectorAll("[data-share][hidden]")) b.hidden = false;
}

// A generated PNG carries its prompt and settings in text chunks. Drawing it to
// a canvas and encoding it again keeps every pixel and its size, and drops them.
async function withoutPrompt(blob) {
  const bitmap = await createImageBitmap(blob);
  const canvas = document.createElement("canvas");
  canvas.width = bitmap.width;
  canvas.height = bitmap.height;
  canvas.getContext("2d").drawImage(bitmap, 0, 0);
  bitmap.close?.();
  const png = await new Promise((resolve) => canvas.toBlob(resolve, "image/png"));
  if (!png) throw new Error("could not encode");
  return png;
}

function shareStatus(from, text) {
  const box = from.closest("dialog, .lab-item")?.querySelector("[data-share-status]");
  if (!box) return;
  box.textContent = text;
  box.hidden = !text;
}

async function shareResult(button) {
  const isPicture = button.dataset.shareName.endsWith(".png");
  try {
    let blob = await (await fetch(button.dataset.share)).blob();
    if (isPicture) blob = await withoutPrompt(blob);
    const file = new File([blob], button.dataset.shareName, { type: blob.type });
    if (isPicture) shareStatus(button, "Shared without the prompt.");
    await navigator.share({ files: [file] });
  } catch (error) {
    // The person closing the share sheet is not a failure.
    if (error?.name === "AbortError") {
      shareStatus(button, "");
      return;
    }
    button.hidden = true;
    shareStatus(button, "This could not be shared here. Use Save instead.");
  }
}

document.addEventListener("click", async (event) => {
  const share = event.target.closest?.("[data-share]");
  if (share) {
    await shareResult(share);
    return;
  }
  const copy = event.target.closest?.("[data-copy-transcript]");
  if (copy) {
    const text = copy.closest("li, .lab-item")?.querySelector("[data-transcript]")?.textContent ?? "";
    await navigator.clipboard?.writeText(text);
    copy.textContent = "Copied";
    setTimeout(() => (copy.textContent = "Copy"), 1500);
  }
});

document.addEventListener("htmx:afterSettle", revealShare);
fillFromStorage();
revealShare();

// --- the picture viewer ---------------------------------------------------------------
// One <dialog> per picture; previous and next move between them.
const viewers = () => [...document.querySelectorAll("#lab-items dialog.viewer")];

// Inspect: a tap, the button or Z doubles the picture, centered where it was tapped. Pinching
// is the browser's own. It's always the whole picture again when you move on.
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

function paintViewer(dialog) {
  setZoom(dialog, false);
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
  setZoom(dialog, false);
  dialog.close();
  next.showModal();
  paintViewer(next);
}

document.addEventListener("click", (event) => {
  const zoom = event.target.closest?.("[data-view-zoom]");
  if (zoom) {
    const dialog = zoom.closest("dialog");
    setZoom(dialog, zoom.getAttribute("aria-pressed") !== "true");
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
  // Reuse settings: the form takes this picture's prompt, shape, look and seed.
  const reuse = event.target.closest?.("[data-reuse]");
  if (reuse) {
    const { prompt: text, shape, look, seed } = reuse.dataset;
    const form = document.querySelector("form.lab-dock");
    if (!form) return;
    for (const [name, value] of [["shape", shape], ["look", look]]) {
      const radio = [...form.querySelectorAll(`input[type=radio][name="${name}"]`)].find((r) => r.value === value);
      if (radio) radio.checked = true;
    }
    if (form.seed) form.seed.value = seed;
    syncChips(form);
    reuse.closest("dialog")?.close();
    fill(text);
  }
});

document.addEventListener("keydown", (event) => {
  const dialog = document.querySelector("dialog.viewer[open]");
  if (!dialog || event.target.closest?.("input, textarea")) return;
  if (event.key === "ArrowLeft") stepViewer(dialog, -1);
  if (event.key === "ArrowRight") stepViewer(dialog, 1);
  if (event.key === "z") setZoom(dialog, !dialog.querySelector(".viewer-zoomed"));
});
