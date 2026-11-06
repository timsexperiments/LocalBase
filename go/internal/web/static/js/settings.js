// Settings that live in this browser: chat history (TS semantics: saving
// is on when the history key exists; turning it off removes it).
import { historyKey, readHistory, writeHistory } from "./chat-core.js";

const box = document.querySelector("[data-history-settings]");
if (box) {
  const toggle = box.querySelector("[data-history-toggle]");
  const clear = box.querySelector("[data-history-clear]");
  const status = box.querySelector("[data-history-status]");
  const say = (text) => (status.textContent = text);
  try {
    toggle.checked = localStorage.getItem(historyKey) !== null;
  } catch {
    toggle.disabled = clear.disabled = true;
    say("Device storage is unavailable.");
  }
  toggle.addEventListener("change", () => {
    try {
      if (toggle.checked) {
        let saved = [];
        try {
          saved = readHistory(localStorage) ?? [];
        } catch {}
        writeHistory(localStorage, saved);
        say("History will be saved on this device.");
      } else {
        localStorage.removeItem(historyKey);
        say("Saved history removed. New conversations won’t be saved.");
      }
    } catch {
      toggle.checked = !toggle.checked;
      say("Device storage is unavailable.");
    }
  });
  clear.addEventListener("click", () => {
    try {
      localStorage.removeItem(historyKey);
      // This tab's conversations and unsent message go too (Chat keeps them
      // in sessionStorage when history is off).
      sessionStorage.removeItem(historyKey);
      sessionStorage.removeItem("localbase.chat.draft.v1");
      toggle.checked = false;
      clear.closest("details")?.removeAttribute("open");
      say("History cleared.");
    } catch {
      say("Saved device history could not be removed; clear this site’s browser data to remove it.");
    }
  });
}
