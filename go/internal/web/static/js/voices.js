// My voices: one Play button per voice, one recording playing at a time.
const player = new Audio();
let current = null;

function show(button, playing) {
  button.setAttribute("aria-pressed", String(playing));
  button.querySelector("[data-play-icon]")?.classList.toggle("hidden", playing);
  button.querySelector("[data-pause-icon]")?.classList.toggle("hidden", !playing);
}

document.addEventListener("click", (event) => {
  const button = event.target.closest?.("[data-play]");
  if (!button) return;
  if (current === button && !player.paused) {
    player.pause();
    return;
  }
  if (current) show(current, false);
  current = button;
  player.src = button.dataset.play;
  player.play().then(() => show(button, true), () => show(button, false));
});
player.addEventListener("pause", () => current && show(current, false));
player.addEventListener("ended", () => current && show(current, false));
// A redrawn list (after add, rename or delete) stops playback.
document.addEventListener("htmx:afterSwap", () => {
  player.pause();
  current = null;
});
