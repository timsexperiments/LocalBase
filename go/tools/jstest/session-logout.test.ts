import { afterEach, describe, expect, test } from "bun:test";

type Listener = (event: unknown) => unknown;

// session.js registers on the document as it loads: a small stand-in is enough to click its button.
async function load(answer: () => Promise<{ ok: boolean }>) {
  const listeners: Record<string, Listener[]> = {};
  const element = () => ({
    children: [] as unknown[],
    textContent: "",
    className: "",
    replaceChildren() {
      this.children = [];
    },
    append(...more: unknown[]) {
      this.children.push(...more);
    },
    addEventListener() {},
  });
  const noticeEl = element();
  const assigned: string[] = [];
  g.document = {
    addEventListener(name: string, fn: Listener) {
      (listeners[name] ??= []).push(fn);
    },
    getElementById: (id: string) => (id === "session-notice" ? noticeEl : null),
    createElement: element,
    hidden: false,
  };
  g.location = { assign: (to: string) => assigned.push(to), reload() {} };
  g.fetch = answer;
  await import(`../../internal/web/static/js/session.js?${Math.random()}`);
  const button = { disabled: false, dataset: {} as Record<string, string> };
  for (const fn of listeners.click ?? []) {
    await fn({ target: { closest: (s: string) => (s === "[data-logout]" ? button : null) } });
  }
  return { button, assigned, noticeEl };
}

// `bun test` runs every file in one process, so the stubs must not outlive each test.
const g = globalThis as Record<string, unknown>;
const original = { fetch: g.fetch, document: g.document, location: g.location };
afterEach(() => {
  g.fetch = original.fetch;
  g.document = original.document;
  g.location = original.location;
});

describe("signing out", () => {
  test("a real answer goes to the signed-out page", async () => {
    const { assigned } = await load(async () => ({ ok: true }));
    expect(assigned).toEqual(["/app?signedout=1"]);
  });

  test("a refusal doesn't say signed out, and the button works again", async () => {
    const { assigned, button, noticeEl } = await load(async () => ({ ok: false }));
    expect(assigned).toEqual([]);
    expect(button.disabled).toBe(false);
    expect(JSON.stringify(noticeEl)).toContain("We couldn’t sign you out");
  });

  test("no answer at all doesn't either", async () => {
    const { assigned, button } = await load(async () => {
      throw new Error("offline");
    });
    expect(assigned).toEqual([]);
    expect(button.disabled).toBe(false);
  });
});
