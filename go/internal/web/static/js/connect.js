// Connect an app (DESIGN §30.4). Copy buttons, and keeping a new key in the snippets when
// htmx re-renders them for another model or address. The secret stays in this variable
// only, never in a URL or storage, and is dropped when the page is left.

const TOKEN = "YOUR_LOCALBASE_KEY";
let secret = "";

function applySecret() {
  if (!document.getElementById("connect-body")) {
    secret = "";
    return;
  }
  const shown = document.querySelector("[data-connect-secret]");
  if (shown) secret = shown.textContent.trim();
  if (!secret) return;
  for (const el of document.querySelectorAll("[data-snippet]")) {
    if (el.textContent.includes(TOKEN)) el.textContent = el.textContent.replaceAll(TOKEN, secret);
  }
}

// Delegated on the document, so it also works on what htmx swaps in.
document.addEventListener("click", async (e) => {
  const button = e.target.closest?.("[data-copy]");
  if (!button) return;
  const source = button.closest("[data-copy-scope]")?.querySelector("[data-copy-text]");
  if (!source) return;
  const label = button.textContent;
  const status = document.querySelector("[data-copy-status]");
  try {
    await navigator.clipboard.writeText(source.textContent);
    button.textContent = "Copied";
    if (status) status.textContent = "Copied.";
  } catch {
    button.textContent = "Couldn’t copy. Select the text instead.";
  }
  setTimeout(() => {
    button.textContent = label;
    if (status) status.textContent = "";
  }, 2000);
});

document.addEventListener("htmx:afterSwap", applySecret);
applySecret();
