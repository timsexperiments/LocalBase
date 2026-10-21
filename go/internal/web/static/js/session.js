// Tells three failures apart when an htmx request does not get a normal
// response (#363, #364):
// - LocalBase's own session expired: the server answers 401 with HX-Redirect,
//   which htmx follows to the sign-in page (no code needed here).
// - The edge sign-in in front of LocalBase (such as Cloudflare Access)
//   expired: the edge redirects the request cross-origin, so fetch fails.
//   A same-origin probe with redirect: "manual" comes back opaque.
// - The gateway is unreachable: the probe itself fails.
const notice = () => document.getElementById("session-notice");

function show(message, action) {
  const box = notice();
  if (!box) return;
  box.replaceChildren();
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

async function diagnose() {
  let response;
  try {
    response = await fetch("/app/session", {
      redirect: "manual",
      credentials: "same-origin",
      cache: "no-store",
      headers: { "X-Localbase-UI": "1" },
    });
  } catch {
    show("Can't reach LocalBase right now. Check the connection and try again.");
    return;
  }
  if (response.type === "opaqueredirect") {
    show("Your sign-in expired. Reload to sign in again.", { label: "Reload", run: () => location.reload() });
  } else if (response.status === 401) {
    location.assign("/app/login");
  } else {
    show("That didn't load. Try again.");
  }
}

document.addEventListener("htmx:sendError", diagnose);
document.addEventListener("htmx:afterRequest", (event) => {
  if (event.detail.successful) notice()?.replaceChildren();
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
  try {
    await fetch("/app/logout", { method: "POST", credentials: "same-origin", headers: { "X-Localbase-UI": "1" } });
  } finally {
    location.assign(button.dataset.logoutUrl || "/app/login");
  }
});
