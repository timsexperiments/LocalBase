// Get models: choosing another download file shows its details, actions and
// fit without a request (the server rendered every choice). Works on cards
// htmx adds later, since it listens on the document.
document.addEventListener("change", (e) => {
  const radio = e.target;
  if (!(radio instanceof HTMLInputElement) || !radio.matches("input[data-choice]")) return;
  const form = radio.closest("[data-discover-form]");
  if (!form) return;
  for (const panel of form.querySelectorAll("[data-choice-panel]")) panel.hidden = panel.dataset.choicePanel !== radio.dataset.choice;
  const none = form.querySelector("[data-no-choice]");
  if (none) none.hidden = true;
  const badge = form.closest("article")?.querySelector("[data-fit-badge]");
  if (badge) {
    badge.textContent = radio.dataset.fit;
    badge.title = radio.dataset.fitHint;
    badge.hidden = !radio.dataset.fit;
  }
});
