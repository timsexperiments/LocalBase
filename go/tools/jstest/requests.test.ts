// A request the page makes by itself must never count as the person's: a successful poll would
// otherwise clear "That didn't load" for something the person did (session.js, DESIGN §22.3a).
// This scans every template's triggers. A new polling or loading element is recognized, and a
// trigger word nobody has classified fails until someone decides which kind it is.
import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "fs";
import { background } from "../../internal/web/static/js/requests.js";

const dir = new URL("../../internal/web/templates/", import.meta.url).pathname;
const BACKGROUND_WORDS = ["every", "load", "revealed", "intersect"];
const PERSON_WORDS = ["click", "submit", "change", "input", "keyup", "keydown", "search"];
// Modifiers and their values: they say when or from where, not who.
const MODIFIERS = /^(changed|once|consume|queue:\S+|delay:\S+|throttle:\S+|from:\S+|target:\S+|\d+(ms|s))$/;

type Found = { file: string; tag: string; trigger: string | null; poll: boolean };
const found: Found[] = [];
for (const file of readdirSync(dir).filter((f) => f.endsWith(".html"))) {
  const html = readFileSync(dir + file, "utf8");
  for (const m of html.matchAll(/<([a-z][a-z0-9-]*)\b([^>]*?)>/g)) {
    const attrs = m[2];
    const trig = attrs.match(/\bhx-trigger="([^"]*)"/)?.[1] ?? null;
    const poll = /\bdata-poll\b/.test(attrs);
    if (trig !== null || poll) found.push({ file, tag: m[1], trigger: trig, poll });
  }
}
const elt = (f: Found) => ({ getAttribute: (n: string) => (n === "hx-trigger" ? f.trigger : null), hasAttribute: (n: string) => n === "data-poll" && f.poll });
const words = (t: string) => t.replace(/\{\{[^}]*\}\}/g, " ").split(/[\s,\[\]]+/).filter(Boolean).filter((w) => !w.includes("="));

describe("which requests the page makes by itself", () => {
  test("the templates have polling elements to check", () => {
    expect(found.filter((f) => f.poll || /\bevery\b/.test(f.trigger ?? "")).length).toBeGreaterThan(3);
  });
  test("every trigger word is one we have classified", () => {
    const unknown = found.flatMap((f) => words(f.trigger ?? "").filter((w) => !BACKGROUND_WORDS.includes(w) && !PERSON_WORDS.includes(w) && !MODIFIERS.test(w)).map((w) => `${f.file}: "${w}" in "${f.trigger}"`));
    expect(unknown).toEqual([]);
  });
  test("each polling, loading or revealed element counts as the page's own", () => {
    const missed = found.filter((f) => f.poll || words(f.trigger ?? "").some((w) => BACKGROUND_WORDS.includes(w))).filter((f) => !background(elt(f))).map((f) => `${f.file} <${f.tag}> "${f.trigger}"`);
    expect(missed).toEqual([]);
  });
  test("a person's trigger never counts as the page's own", () => {
    const wrong = found.filter((f) => !f.poll && f.trigger && !words(f.trigger).some((w) => BACKGROUND_WORDS.includes(w))).filter((f) => background(elt(f))).map((f) => `${f.file} <${f.tag}> "${f.trigger}"`);
    expect(wrong).toEqual([]);
  });
  test("combined and delayed triggers", () => {
    const at = (t: string | null, poll = false) => background({ getAttribute: () => t, hasAttribute: (n: string) => n === "data-poll" && poll });
    expect(at("every 2s")).toBe(true);
    expect(at("load delay:1s")).toBe(true);
    expect(at("change, every 5s")).toBe(true);
    expect(at("revealed")).toBe(true);
    expect(at("input changed delay:250ms from:#model-q, submit")).toBe(false);
    expect(at("click")).toBe(false);
    expect(at(null)).toBe(false);
    expect(at(null, true)).toBe(true);
    expect(background(null)).toBe(false);
  });
});
