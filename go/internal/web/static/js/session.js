// Tells three failures apart when an htmx request does not get a normal
// response (#363, #364):
// - LocalBase's own session expired: the server answers 401 with HX-Redirect,
//   which htmx follows to the sign-in page (no code needed here).
// - The edge sign-in in front of LocalBase (such as Cloudflare Access)
//   expired: the edge redirects the request cross-origin, so fetch fails.
//   A same-origin probe with redirect: "manual" comes back opaque.
// - The gateway is unreachable: the probe itself fails.
import { forgetOthers, forgetTab } from "./person.js";
import { background } from "./requests.js";

forgetOthers();

const notice = () => document.getElementById("session-notice");

// The notice showing: connection, action, sign-in or sign-out. Its kind decides what clears it (DESIGN §22.3a).
let shownKind = "";

function show(message, action, kind = "action") {
  const box = notice();
  if (!box) return;
  box.replaceChildren();
  shownKind = kind;
  const card = document.createElement("div");
  card.className = "card mt-3 flex flex-wrap items-center justify-between gap-3 border-danger-line bg-danger-soft p-4";
  const text = document.createElement("p");
  text.className = "font-semibold text-danger";
  text.textContent = message;
  card.append(text);
  if (action) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "btn btn-quiet";
    button.textContent = action.label;
    button.addEventListener("click", action.run);
    card.append(button);
  }
  box.append(card);
}

function clear() {
  notice()?.replaceChildren();
  shownKind = "";
  signOutFailed = false;
}

// A failed sign-out says the person is still signed in. A poll failing for the same reason doesn't
// replace that with a message that doesn't say so.
let signOutFailed = false;

async function diagnose(event) {
  if (signOutFailed) return;
  // A form that says its own send failure ("[data-own-send-error]") keeps the sign-in check
  // but not the two lines that would repeat it.
  const own = event?.detail?.elt?.closest?.("[data-own-send-error]");
  let response;
  try {
    response = await fetch("/app/session", {
      redirect: "manual",
      credentials: "same-origin",
      cache: "no-store",
      headers: { "X-Localbase-UI": "1" },
    });
  } catch {
    if (!own) show("Can't reach LocalBase right now. Check the connection and try again.", null, "connection");
    return;
  }
  if (response.type === "opaqueredirect") {
    show("Your sign-in expired. Reload to sign in again.", { label: "Reload", run: () => location.reload() }, "sign-in");
  } else if (response.status === 401) {
    location.assign("/app/login");
  } else if (!own) {
    show("That didn't load. Try again.");
  }
}

document.addEventListener("htmx:sendError", diagnose);
// Told apart as each request starts: by the time it ends, a swap may have replaced its element.
const pages = new WeakSet();
document.addEventListener("htmx:beforeRequest", (event) => {
  if (background(event.detail.elt)) pages.add(event.detail.xhr);
});
document.addEventListener("htmx:afterRequest", (event) => {
  if (!event.detail.successful) return;
  if (shownKind === "connection" || (shownKind === "action" && !pages.has(event.detail.xhr))) clear();
});
// A page change starts with no notice.
document.addEventListener("htmx:afterSwap", (event) => {
  if (event.detail.boosted) clear();
});

// Polling regions (data-poll) skip requests while the page is hidden, so a
// phone in a pocket does not keep the gateway busy. Done here rather than in
// an hx-trigger filter because the CSP forbids htmx's eval.
document.addEventListener("htmx:beforeRequest", (event) => {
  if (document.hidden && event.detail.elt?.hasAttribute?.("data-poll")) event.preventDefault();
});

// Sign out: the gateway clears the LocalBase session; a Cloudflare Access
// session then signs out at the edge.
document.addEventListener("click", async (event) => {
  const button = event.target.closest?.("[data-logout]");
  if (!button) return;
  button.disabled = true;
  let failure = "";
  try {
    const answer = await fetch("/app/logout", { method: "POST", credentials: "same-origin", headers: { "X-Localbase-UI": "1" } });
    // Only a real answer ends it. A 503 means the session is still good, so saying "signed out" would be false.
    if (!answer.ok) failure = "We couldn’t sign you out. Try again in a moment.";
  } catch {
    failure = "Can’t reach LocalBase, so we couldn’t sign you out. You’re still signed in.";
  }
  if (failure) {
    button.disabled = false;
    signOutFailed = true;
    show(failure, null, "sign-out");
    return;
  }
  // Only now: a sign-out that failed leaves the person signed in, with their words still here.
  forgetTab();
  // Not to /app/login: with one provider that goes straight back to it, and a provider that
  // remembers you signs you right back in.
  location.assign(button.dataset.logoutUrl || "/app?signedout=1");
});
