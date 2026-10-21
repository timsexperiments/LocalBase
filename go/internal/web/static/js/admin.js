// Admin: copy a one-time secret or invitation link, show the fields of the
// chosen sign-in provider (hidden ones are disabled, so they're neither
// required nor sent), and close an open confirmation with Escape. Listens on
// the document, so it also works on sections htmx swaps in.
document.addEventListener("click", async (e) => {
  const button = e.target.closest?.("[data-copy-secret]");
  if (!button) return;
  const code = button.closest("section, [role=group]")?.querySelector("[data-secret]");
  if (!code) return;
  const label = button.textContent;
  try {
    await navigator.clipboard.writeText(code.textContent.trim());
    button.textContent = "Copied";
  } catch {
    button.textContent = "Couldn’t copy. Select the text instead.";
  }
  setTimeout(() => (button.textContent = label), 2000);
});

function syncProvider(form) {
  const kind = form.querySelector("[data-provider-select]")?.value;
  const publicClient = form.querySelector("[data-public-client]")?.checked;
  for (const set of form.querySelectorAll("[data-provider-fields]")) {
    const on = set.dataset.providerFields.split(" ").includes(kind);
    set.hidden = !on;
    set.disabled = !on;
  }
  const secret = form.querySelector("[data-client-secret]");
  if (secret) {
    const needed = kind === "github-oauth" || (kind === "oidc" && !publicClient);
    secret.hidden = !needed;
    secret.querySelector("input").disabled = !needed;
  }
}

document.addEventListener("change", (e) => {
  const form = e.target.closest?.("[data-provider-form]");
  if (form && e.target.matches("[data-provider-select], [data-public-client]")) syncProvider(form);
  if (e.target.matches?.("[data-password-auth]")) {
    const fields = e.target.closest("form").querySelector("[data-password-fields]");
    if (fields) fields.disabled = !e.target.checked;
  }
});

const init = () => document.querySelectorAll("[data-provider-form]").forEach(syncProvider);
init();
document.addEventListener("htmx:load", init);

document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  const open = document.activeElement?.closest?.("details.confirm[open]");
  if (!open) return;
  open.open = false;
  open.querySelector("summary").focus();
});
