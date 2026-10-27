// The app shell (DESIGN §22): bottom sheets, keyboard-safe layout and the
// "Generating…" pill. Everything is delegated from document because boosted
// navigation swaps <body>.
import * as runs from "./runs.js";

const root = document.documentElement;

// --- sheets ---------------------------------------------------------------------
// A [data-open-sheet=id] button opens <dialog id> as a modal; [data-close-sheet]
// and a tap on the backdrop close it. The dialog returns focus on close.
document.addEventListener("click", (event) => {
  const opener = event.target.closest?.("[data-open-sheet]");
  if (opener) {
    document.getElementById(opener.dataset.openSheet)?.showModal();
    return;
  }
  if (event.target.closest?.("[data-close-sheet]")) {
    event.target.closest("dialog")?.close();
    return;
  }
  if (event.target.matches?.("dialog.sheet")) event.target.close(); // the backdrop
});

// A form marked data-close-sheet-on-success closes its sheet once the server
// accepts it (the answer shows no alert).
document.addEventListener("htmx:afterRequest", (event) => {
  const form = event.detail.elt?.closest?.("form[data-close-sheet-on-success]");
  if (!form || !event.detail.successful) return;
  // Look once the answer is in place.
  document.addEventListener("htmx:afterSettle", () => {
    const target = document.querySelector(form.getAttribute("hx-target"));
    if (!target?.querySelector("[role=alert]")) form.closest("dialog")?.close();
  }, { once: true });
});

// --- the on-screen keyboard -------------------------------------------------------
// With interactive-widget=resizes-content (Chrome on Android) the layout
// viewport shrinks for the keyboard. iOS Safari overlays it instead, so the
// visual viewport is measured: --lb-kb is the part of the layout viewport the
// keyboard covers. While a text field has focus on a touch screen,
// html[data-typing] hides the tab bar, and a field the keyboard covers is
// scrolled into view.
const touch = matchMedia("(pointer: coarse)");
const vv = window.visualViewport;
const nonText = /^(checkbox|radio|button|submit|reset|file|range|color|image|hidden)$/;

function typesText(el) {
  if (!el) return false;
  if (el.isContentEditable || el.tagName === "TEXTAREA") return !el.readOnly;
  return el.tagName === "INPUT" && !nonText.test(el.type) && !el.readOnly;
}

function syncKeyboard() {
  let inset = 0;
  if (vv && Math.abs(vv.scale - 1) < 0.01) inset = Math.max(0, Math.round(window.innerHeight - vv.height - vv.offsetTop));
  root.style.setProperty("--lb-kb", `${inset}px`);
  root.toggleAttribute("data-typing", touch.matches && typesText(document.activeElement));
}

function reveal(el) {
  // Docked composers (chat, the Lab prompt) sit above the keyboard already.
  if (!typesText(el) || !el.isConnected || el.closest("[data-docked]")) return;
  const top = vv ? vv.offsetTop : 0;
  const bottom = top + (vv ? vv.height : window.innerHeight);
  const r = el.getBoundingClientRect();
  if (r.top >= top + 8 && r.bottom <= bottom - 8) return;
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  el.scrollIntoView({ block: r.height > bottom - top - 16 ? "start" : "center", behavior: reduce ? "auto" : "smooth" });
}

let revealTimer;
document.addEventListener("focusin", (event) => {
  syncKeyboard();
  if (!touch.matches || !typesText(event.target)) return;
  // The keyboard animates in; check again once it has settled.
  clearTimeout(revealTimer);
  revealTimer = setTimeout(() => reveal(event.target), 350);
});
document.addEventListener("focusout", () => setTimeout(syncKeyboard, 0));
vv?.addEventListener("resize", () => {
  syncKeyboard();
  if (root.hasAttribute("data-typing")) reveal(document.activeElement);
});
document.addEventListener("htmx:afterSettle", syncKeyboard);

// --- polled status ----------------------------------------------------------------
// A polled region says what it shows in data-announce; the sentence goes to
// the live region named by data-announce-into only when it changes, so a
// refresh every few seconds stays quiet for screen readers.
const announced = new Map();
function announceChanges() {
  for (const el of document.querySelectorAll("[data-announce][data-announce-into]")) {
    const key = el.dataset.announceInto;
    const text = el.dataset.announce;
    if (!announced.has(key)) announced.set(key, text); // the first view is not news
    else if (announced.get(key) !== text) {
      announced.set(key, text);
      const box = document.getElementById(key);
      if (box) box.textContent = text;
    }
  }
}
document.addEventListener("htmx:afterSettle", (event) => {
  // A new page (boosted navigation) starts over; a poll compares.
  if (event.detail?.target === document.body) announced.clear();
  announceChanges();
});
document.addEventListener("htmx:historyRestore", () => announced.clear());
// A polled region holding an open sheet (a confirmation) waits: a refresh
// would replace the sheet under the person's finger.
document.addEventListener("htmx:beforeRequest", (event) => {
  const elt = event.detail.elt;
  if (elt?.hasAttribute?.("data-poll") && elt.querySelector("dialog[open]")) event.preventDefault();
});
announceChanges();

// --- "Generating…" --------------------------------------------------------------
// A run started here keeps going while the person is elsewhere; the pill (in
// the sidebar and above the tab bar) shows it and links back to it. A page
// that shows the run itself marks <body data-run-page="href">.
async function syncRuns() {
  let list = [];
  try {
    list = await runs.active();
  } catch {
    // Unreachable or signed out: session.js reports it; keep the pill hidden.
  }
  const here = document.body.dataset.runPage;
  // A run started in another browser has no place here: send it to its page.
  const shown = list
    .map((run) => ({
      href: run.place?.href || (run.target?.includes("chat") ? "/app/chat" : "/app/lab"),
      label: run.place?.label || run.label || "Generating",
    }))
    .filter((run) => run.href !== here);
  for (const pill of document.querySelectorAll("[data-run-pill]")) {
    pill.hidden = !shown.length;
    if (!shown.length) continue;
    pill.href = shown[0].href;
    const label = shown[0].label;
    pill.querySelector("[data-run-label]").textContent =
      shown.length > 1 ? `${shown.length} running · ${label}` : `${label}…`;
  }
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden) syncRuns();
});
document.addEventListener("htmx:afterSettle", syncRuns);
document.addEventListener("localbase:runs", syncRuns); // a page started or ended one
syncKeyboard();
syncRuns();
