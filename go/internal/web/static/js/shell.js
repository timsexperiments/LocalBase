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

// --- generated pictures -------------------------------------------------------------
// Results are kept for a while. A picture whose file is gone, or that will not
// decode, shows its drawing and "Expired" instead of a broken-image icon.
function pictureGone(img) {
  img.hidden = true;
  img.parentElement?.querySelector("[data-result-gone]")?.removeAttribute("hidden");
}
function checkPictures() {
  for (const img of document.querySelectorAll("img[data-result-img]")) if (img.complete && !img.naturalWidth) pictureGone(img);
}
// Load errors do not bubble: listen in the capture phase. Pictures that failed
// before this module ran are found by checkPictures.
document.addEventListener("error", (event) => {
  if (event.target.matches?.("img[data-result-img]")) pictureGone(event.target);
}, true);
// A player left open past the result's keep says so instead of showing a dead control.
document.addEventListener("error", (event) => {
  const player = event.target;
  if (!player.matches?.("audio[src^='/app/lab/'], video[src^='/app/lab/']")) return;
  const note = document.createElement("p");
  note.className = "player-gone";
  note.textContent = "Expired";
  player.replaceWith(note);
}, true);
document.addEventListener("htmx:afterSettle", checkPictures);
checkPictures();

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
  // The strip has nothing to say to a page nobody is looking at.
  if (elt?.hasAttribute?.("data-strip") && (document.hidden || Date.now() < stripNextTry)) event.preventDefault();
});
// The strip refreshes every 3 s while it can reach the computer, and backs off to a minute
// while it can't.
let stripNextTry = 0;
let stripWait = 3000;
document.addEventListener("htmx:afterOnLoad", (event) => {
  if (!event.detail.elt?.hasAttribute?.("data-strip")) return;
  stripNextTry = 0;
  stripWait = 3000;
  showRefused();
  askAdmission();
});
// Back can bring a page out of the browser's memory exactly as it was, whoever is signed in now,
// and no-store doesn't prevent it. So a restored page is never shown:
// it's blanked and fetched again, as whoever is here now.
addEventListener("pageshow", (event) => {
  if (!event.persisted) return;
  document.documentElement.style.visibility = "hidden";
  location.reload();
});

// A request this computer just refused says more than "Ready" does (chat.js). It lasts until the
// person moves on, or until the gateway's preview would admit that model. Then "try again" still
// waits for the gateway to stop being short or settling, because a retry before then is refused too.
// The preview is a good guess, not a promise: its reading can be a second old, and it counts running
// models as if stopped. So the words say "looks", and a retry refused after them isn't told the same
// thing again until the reading has really moved.
let refusedWord = null;
let refusedModel = null;
let refusedAdmitted = false;
let askingAdmission = false;
let lookedFree = null; // { model, available }: the reading we last said "looks free" on
let notBefore = null; // { model, available }: refused after saying so; wait for more than this
const moreThanDrift = 2 ** 30; // A judgment, not measured: what's free moves about this much by itself.
function showRefused() {
  if (!refusedWord) return;
  const strip = document.querySelector("[data-strip] .strip");
  const word = strip?.querySelector("[data-strip-word]");
  if (!word) return;
  if (refusedAdmitted) {
    if (strip.classList.contains("strip-settling")) return;
    // Short by the gateway's own say, not because this function marked it after the refusal.
    if (!strip.classList.contains("strip-short") || strip.dataset.refusedHere) {
      strip.classList.remove("strip-short");
      word.textContent = "Memory looks free. Try again.";
      return;
    }
  }
  word.textContent = refusedWord;
  if (!strip.classList.contains("strip-short")) strip.dataset.refusedHere = "1";
  strip.classList.add("strip-short");
}
// The gateway's preview, which changes nothing. Asked once per strip refresh, only while a refusal
// stands, one at a time. No answer changes nothing.
async function askAdmission() {
  const model = refusedModel;
  if (!refusedWord || !model || askingAdmission) return;
  askingAdmission = true;
  try {
    const res = await fetch(`/app/api/_localbase/models/${encodeURIComponent(model)}/admission`, { headers: { "x-localbase-ui": "1" } });
    if (!res.ok) return;
    const answer = await res.json();
    if (model !== refusedModel) return;
    const available = answer.availableBytes ?? null;
    const held = notBefore?.model === model && (available === null || available < notBefore.available + moreThanDrift);
    refusedAdmitted = answer.admitted === true && !held;
    if (refusedAdmitted) lookedFree = { model, available };
    showRefused();
  } catch {
    // No answer: the words stand.
  } finally {
    askingAdmission = false;
  }
}
document.addEventListener("localbase:refused", (event) => {
  const cleared = refusedWord && !event.detail;
  refusedWord = event.detail?.word ?? null;
  refusedModel = event.detail?.model ?? null;
  refusedAdmitted = false;
  // Refused after we said it looked free: don't say so again on the same reading.
  if (refusedWord && lookedFree?.model === refusedModel && lookedFree.available !== null) notBefore = lookedFree;
  if (refusedWord) lookedFree = null;
  showRefused();
  if (cleared) {
    stripNextTry = 0;
    document.querySelector("[data-strip]")?.removeAttribute("hx-headers");
  }
});
document.addEventListener("htmx:afterSettle", showRefused);
// Reached, but it can't read its own state for a moment (a held store, say). What the strip last
// said is out of date, so it doesn't go on saying it. The next good answer redraws it.
document.addEventListener("htmx:responseError", (event) => {
  const elt = event.detail.elt;
  if (!elt?.hasAttribute?.("data-strip") || event.detail.xhr?.status !== 503) return;
  elt.removeAttribute("hx-headers");
  const strip = elt.querySelector(".strip");
  if (strip) strip.className = "strip strip-load strip-stale";
  const word = elt.querySelector("[data-strip-word]");
  if (word) word.textContent = "LocalBase is busy";
  const free = elt.querySelector("[data-strip-free]");
  if (free) free.textContent = "";
});
// The strip when the computer can't be reached: same place, same shape, and it says so.
// The next refresh that works redraws it.
document.addEventListener("htmx:sendError", (event) => {
  const elt = event.detail.elt;
  if (!elt?.hasAttribute?.("data-strip")) return;
  stripNextTry = Date.now() + stripWait;
  stripWait = Math.min(stripWait * 2, 60000);
  // It is about to say something else: the next answer has to redraw it.
  elt.removeAttribute("hx-headers");
  const strip = elt.querySelector(".strip");
  strip?.classList.add("strip-off");
  const word = elt.querySelector("[data-strip-word]");
  if (word) word.textContent = "Can’t reach this computer";
  const free = elt.querySelector("[data-strip-free]");
  if (free) free.textContent = "";
});
announceChanges();

// --- "Generating…" --------------------------------------------------------------
// A run started here keeps going while the person is elsewhere; the pill (in
// the sidebar and above the tab bar) shows it and links back to it. A page
// that shows the run itself marks <body data-run-page="href">.
// Video jobs and scripts are not runs (DESIGN §22.18, §22.19), so they are asked
// about apart. Polled pages call this often: it asks at most every 10 s while one
// runs, every 60 s when none does, and not at all while the page is hidden. Their
// own pages show them, and leaving one asks at once.
// Where a run's result is, when the page that started it recorded no place.
const RUN_PAGES = { "/v1/images/generations": "/app/studio/image", "/v1/images/edits": "/app/studio/image", "/v1/audio/transcriptions": "/app/studio/transcribe", "/v1/audio/translations": "/app/studio/transcribe" };
// The landing shows these itself, in Recent.
const ON_LANDING = new Set(["/app/studio/image", "/app/studio/video", "/app/studio/speech", "/app/studio/transcribe"]);
const OWN_PAGES = { video: "/app/studio/video", speech: "/app/studio/speech" };
const OWN_LABELS = { video: "Video", speech: "Speech" };
let ownLast = { video: 0, speech: 0 };
let ownAskedAt = -Infinity;

async function ownJobs() {
  const onOwnPage = Object.values(OWN_PAGES).includes(location.pathname);
  const running = ownLast.video + ownLast.speech > 0;
  if (!onOwnPage && !document.hidden && performance.now() - ownAskedAt >= (running ? 10_000 : 60_000)) {
    ownAskedAt = performance.now();
    try {
      const response = await fetch("/app/lab/active", { cache: "no-store", redirect: "manual" });
      ownLast = response.ok ? await response.json() : { video: 0, speech: 0 };
    } catch {
      // Unreachable: keep the last answer until the next ask.
    }
  }
  if (onOwnPage) ownAskedAt = -Infinity;
  return Object.entries(ownLast).flatMap(([tool, count]) =>
    OWN_PAGES[tool] === location.pathname ? [] : Array.from({ length: count }, () => ({ place: { href: OWN_PAGES[tool], label: OWN_LABELS[tool] } })),
  );
}

async function syncRuns() {
  let list = [];
  try {
    list = await runs.active();
  } catch {
    // Unreachable or signed out: session.js reports it; keep the pill hidden.
  }
  list.push(...(await ownJobs()));
  const here = document.body.dataset.runPage;
  // A run started in another browser has no place here: send it to its page.
  const shown = list
    .map((run) => ({
      href: run.place?.href || (run.target?.includes("chat") ? "/app/chat" : RUN_PAGES[run.target] || "/app/studio"),
      label: run.place?.label || run.label || "Generating",
    }))
    .filter((run) => run.href !== here && run.href !== location.pathname && !(location.pathname === "/app/studio" && ON_LANDING.has(run.href)));
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

// The prompt's real height, for the space kept under a page's results. It grows with what
// it holds (a template chip, a long question), and a fixed space would let it cover them.
let dockSeen = null;
const dockSize = new ResizeObserver(([entry]) => {
  document.documentElement.style.setProperty("--lb-dock-h", `${Math.ceil(entry.target.getBoundingClientRect().height)}px`);
});
function watchDock() {
  const dock = document.querySelector("[data-docked]");
  if (dock === dockSeen) return;
  if (dockSeen) dockSize.unobserve(dockSeen);
  dockSeen = dock;
  if (dock) dockSize.observe(dock);
}
watchDock();
document.addEventListener("htmx:afterSettle", watchDock);

// First run: the button says what picking this model will do.
document.addEventListener("change", (event) => {
  const said = event.target.dataset?.actionLabel;
  const label = document.querySelector("[data-action-label-text]");
  if (said && label) label.textContent = said;
});
