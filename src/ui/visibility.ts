type VisibilityTarget = Pick<
  Document,
  "hidden" | "addEventListener" | "removeEventListener"
>;

/**
 * Runs `tick` every `ms` while the page is visible. Hidden tabs skip polling
 * entirely, and becoming visible again runs `tick` at once (then resumes the
 * interval) so the page never shows stale data after a long absence.
 * Returns a function that stops polling.
 */
export function pollWhileVisible(
  tick: () => void,
  ms: number,
  target: VisibilityTarget = document,
) {
  let timer: ReturnType<typeof setInterval> | undefined;
  const schedule = () => {
    clearInterval(timer);
    timer = target.hidden ? undefined : setInterval(tick, ms);
  };
  const change = () => {
    if (!target.hidden) tick();
    schedule();
  };
  target.addEventListener("visibilitychange", change);
  schedule();
  return () => {
    clearInterval(timer);
    target.removeEventListener("visibilitychange", change);
  };
}

/**
 * Calls `resume` whenever the page becomes visible again. For pollers that
 * schedule their own next run: they skip scheduling while `target.hidden`
 * and rely on this to start again.
 */
export function onVisible(
  resume: () => void,
  target: VisibilityTarget = document,
) {
  const change = () => {
    if (!target.hidden) resume();
  };
  target.addEventListener("visibilitychange", change);
  return () => target.removeEventListener("visibilitychange", change);
}

/** How long after the page is shown again a network failure may be a resume glitch. */
export const resumeWindowMs = 10_000;
let shownAt = Number.NEGATIVE_INFINITY;

/**
 * Records when the page becomes visible again, for `resumedRecently`. iOS
 * Safari often fails the first requests after a backgrounded tab resumes.
 * Returns a function that stops tracking and forgets the last resume.
 */
export function trackResume(target: VisibilityTarget = document) {
  const stop = onVisible(() => {
    shownAt = Date.now();
  }, target);
  return () => {
    stop();
    shownAt = Number.NEGATIVE_INFINITY;
  };
}

/** Whether the page became visible again within the last `resumeWindowMs`. */
export function resumedRecently(now = Date.now()) {
  return now - shownAt < resumeWindowMs;
}
