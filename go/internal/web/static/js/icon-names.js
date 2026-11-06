// --- A long press names an icon ---------------------------------------------------
// A title is no use on a phone. Holding shows the name, and letting go does nothing else:
// a long press on Remove must not remove.
const iconNames = ".result-actions .icon-btn[title]";
let holding = null, named = null, swallow = false;
function hideName() {
  clearTimeout(holding);
  holding = null;
  named?.remove();
  named = null;
}
document.addEventListener("touchstart", (event) => {
  const icon = event.target.closest?.(iconNames);
  hideName();
  swallow = false;
  if (!icon) return;
  holding = setTimeout(() => {
    swallow = true;
    named = document.createElement("span");
    named.className = "icon-name";
    named.textContent = icon.title;
    const r = icon.getBoundingClientRect();
    named.style.left = `${Math.min(Math.max(r.left + r.width / 2, 48), innerWidth - 48)}px`;
    named.style.top = `${r.top - 6}px`;
    document.body.append(named);
  }, 450);
}, { passive: true });
document.addEventListener("touchmove", hideName, { passive: true });
document.addEventListener("touchend", () => { clearTimeout(holding); holding = null; if (named) setTimeout(hideName, 900); }, { passive: true });
document.addEventListener("touchcancel", hideName, { passive: true });
document.addEventListener("contextmenu", (event) => { if (event.target.closest?.(iconNames)) event.preventDefault(); });
document.addEventListener("click", (event) => {
  if (!swallow) return;
  swallow = false;
  if (event.target.closest?.(iconNames)) { event.preventDefault(); event.stopImmediatePropagation(); }
}, true);
