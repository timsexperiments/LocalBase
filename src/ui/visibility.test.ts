import { afterEach, beforeEach, describe, expect, jest, test } from "bun:test";
import { onVisible, pollWhileVisible } from "./visibility";

class FakeDocument extends EventTarget {
  hidden = false;
  set(hidden: boolean) {
    this.hidden = hidden;
    this.dispatchEvent(new Event("visibilitychange"));
  }
}

describe("pollWhileVisible", () => {
  beforeEach(() => jest.useFakeTimers());
  afterEach(() => jest.useRealTimers());

  test("polls while visible, pauses while hidden, and refreshes on return", () => {
    const page = new FakeDocument();
    let ticks = 0;
    const stop = pollWhileVisible(() => ticks++, 10, page);
    jest.advanceTimersByTime(35);
    expect(ticks).toBe(3);

    page.set(true);
    jest.advanceTimersByTime(40);
    expect(ticks).toBe(3);

    page.set(false);
    expect(ticks).toBe(4);
    jest.advanceTimersByTime(35);
    expect(ticks).toBe(7);

    stop();
    page.set(true);
    page.set(false);
    jest.advanceTimersByTime(30);
    expect(ticks).toBe(7);
  });

  test("does not poll when started in a hidden tab until it is shown", () => {
    const page = new FakeDocument();
    page.hidden = true;
    let ticks = 0;
    const stop = pollWhileVisible(() => ticks++, 10, page);
    jest.advanceTimersByTime(30);
    expect(ticks).toBe(0);
    page.set(false);
    expect(ticks).toBe(1);
    jest.advanceTimersByTime(10);
    expect(ticks).toBe(2);
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
