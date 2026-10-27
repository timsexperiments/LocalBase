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
const currentTab = () => new URLSearchParams(location.search).get("tab") || "image";

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
    location.assign(`/app/lab?tab=${encodeURIComponent(tab)}`);
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
  if (!(await canShareFiles())) return;
  for (const b of document.querySelectorAll("[data-share][hidden]")) b.hidden = false;
}

document.addEventListener("click", async (event) => {
  const share = event.target.closest?.("[data-share]");
  if (share) {
    const blob = await (await fetch(share.dataset.share)).blob();
    const file = new File([blob], share.dataset.shareName, { type: blob.type });
    navigator.share({ files: [file] }).catch(() => {});
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
