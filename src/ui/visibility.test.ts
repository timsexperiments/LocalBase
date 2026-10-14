import { describe, expect, test } from "bun:test";
import { onVisible, pollWhileVisible } from "./visibility";

class FakeDocument extends EventTarget {
  hidden = false;
  set(hidden: boolean) {
    this.hidden = hidden;
    this.dispatchEvent(new Event("visibilitychange"));
  }
}
const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe("pollWhileVisible", () => {
  test("polls while visible, pauses while hidden, and refreshes on return", async () => {
    const page = new FakeDocument();
    let ticks = 0;
    const stop = pollWhileVisible(() => ticks++, 10, page);
    await wait(35);
    expect(ticks).toBeGreaterThanOrEqual(2);

    page.set(true);
    const hiddenAt = ticks;
    await wait(40);
    expect(ticks).toBe(hiddenAt);

    page.set(false);
    expect(ticks).toBe(hiddenAt + 1);
    await wait(35);
    expect(ticks).toBeGreaterThan(hiddenAt + 1);

    stop();
    const stoppedAt = ticks;
    page.set(true);
    page.set(false);
    await wait(30);
    expect(ticks).toBe(stoppedAt);
  });

  test("does not poll when started in a hidden tab until it is shown", async () => {
    const page = new FakeDocument();
    page.hidden = true;
    let ticks = 0;
    const stop = pollWhileVisible(() => ticks++, 10, page);
    await wait(30);
    expect(ticks).toBe(0);
    page.set(false);
    expect(ticks).toBe(1);
    stop();
  });
});

describe("onVisible", () => {
  test("fires only when the page becomes visible, until stopped", () => {
    const page = new FakeDocument();
    let resumes = 0;
    const stop = onVisible(() => resumes++, page);
    page.set(true);
    expect(resumes).toBe(0);
    page.set(false);
    expect(resumes).toBe(1);
    stop();
    page.set(true);
    page.set(false);
    expect(resumes).toBe(1);
  });
});
