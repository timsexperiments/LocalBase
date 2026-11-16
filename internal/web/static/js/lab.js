import { personKey } from "./person.js";
import { fill, grow, syncChips } from "./lab-prompt.js";

// The Lab's docked prompt (DESIGN §22.10): option chips show what their sheet
// has selected, and the prompt grows with its text up to a cap. Results and
// runs are server-rendered (htmx); nothing here is needed to submit.

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
const fillKey = () => `localbase.lab.fill.v2.${personKey()}`;
const currentTab = () => location.pathname.split("/").pop();

document.addEventListener("click", (event) => {
  const button = event.target.closest?.("[data-fill]");
  if (!button) return;
  const tab = button.dataset.fillTab;
  if (tab && tab !== currentTab()) {
    sessionStorage.setItem(fillKey(), JSON.stringify({ tab, text: button.dataset.fill }));
    location.assign(`/app/studio/${encodeURIComponent(tab)}`);
    return;
  }
  fill(button.dataset.fill);
});

function fillFromStorage() {
  try {
    const saved = JSON.parse(sessionStorage.getItem(fillKey()) ?? "null");
    if (saved?.tab === currentTab()) {
      sessionStorage.removeItem(fillKey());
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
