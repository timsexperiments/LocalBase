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
