// What every Studio tool's prompt does, shared by lab.js and image.js: chips that show their
// sheet's choice, a prompt that grows with its text, and putting words into it. No listeners here,
// so importing it twice does nothing twice.

// A chip ([data-chip-label=name]) shows the label of the checked radio of
// the same name in its form.
export function syncChips(root = document) {
  for (const label of root.querySelectorAll("[data-chip-label]")) {
    const form = label.closest("form");
    const checked = form?.querySelector(`input[type=radio][name="${CSS.escape(label.dataset.chipLabel)}"]:checked`);
    label.textContent = checked?.dataset.label ?? checked?.value ?? "None";
  }
}

export function grow(area) {
  area.style.height = "auto";
  area.style.height = `${Math.min(area.scrollHeight, window.innerHeight * 0.3)}px`;
}

export function fill(text) {
  const prompt = document.getElementById("lab-prompt");
  if (!prompt) return;
  prompt.value = text;
  grow(prompt);
  prompt.focus({ preventScroll: true }); // the prompt is always on screen; the page stays where it was
}
