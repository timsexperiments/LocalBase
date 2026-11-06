import { expect, test } from "bun:test";
import { axeLabels, compare, messagesIn, missingLabels, parseAllow, phoneLabels, toPattern } from "../check-manifest/manifest.ts";

const source = [
  'const check = (ok, msg) => { console.log((ok ? "ok   " : "FAIL ") + msg); };',
  'check(a === 1, "a plain check");',
  "for (const width of [320, 390]) check(b > 2, `at ${width}, it fits: ${JSON.stringify({ b, c })}`);",
  "if (false) {",
  '  check(c.label === "not the message" && !/, 0\\)$/.test(c.ground), `a guarded check: ${c}`);',
  "}",
].join("\n");

test("a check in a section guarded off is reported missing, and the run can't pass", () => {
  const log = "ok   a plain check\nok   at 320, it fits: {}\nok   at 390, it fits: {}\n";
  const r = compare(source, log);
  expect(r.checks).toBe(3);
  expect(r.missing).toEqual(["a guarded check: ${c}"]);
});

test("every check with a result line, even a failing one, counts as having run", () => {
  const log = "ok   a plain check\nFAIL at 320, it fits: {}\nok   a guarded check: x\n";
  expect(compare(source, log).missing).toEqual([]);
});

test("a check absent for a stated reason is allowed only in its own mode", () => {
  const log = "ok   a plain check\nok   at 320, it fits: {}\n";
  const allow = parseAllow("# a comment\nedit\ta guarded check\tthis gateway has one\n");
  const inMode = compare(source, log, allow, "edit");
  expect(inMode.missing).toEqual([]);
  expect(inMode.allowed).toEqual(["a guarded check: ${c} (this gateway has one)"]);
  expect(compare(source, log, allow, "other").missing).toEqual(["a guarded check: ${c}"]);
});

test("strings and brackets in a condition aren't taken for the message", () => {
  expect(messagesIn(source).messages).toContain("a guarded check: ${c}");
  expect(messagesIn(source).messages).not.toContain("not the message");
});

test("a message with no words of its own can't be held to account, and says so", () => {
  const r = compare("check(ok, `${label}`);", "ok   anything\n");
  expect(r.unverifiable).toEqual(["${label}"]);
  expect(r.missing).toEqual([]);
});

test("a call it can't read is reported rather than dropped", () => {
  expect(compare("check(ok, message);", "").unparsed.length).toBe(1);
});

test("the words of a message are literal, and each ${...} matches anything", () => {
  const rx = new RegExp(`^${toPattern("at ${width} (wide), a.b: ${JSON.stringify({ a })}")}$`);
  expect(rx.test("at 320 (wide), a.b: {}")).toBe(true);
  expect(rx.test("at 320 (wide), axb: {}")).toBe(false);
});

test("a page list has to appear at every width", () => {
  const labels = phoneLabels('const pages = ["/app/a", "/app/b"];');
  expect(labels.length).toBe(8);
  const log = labels.filter((l) => !l.endsWith("/app/b")).map((l) => `ok   ${l}`).join("\n");
  expect(missingLabels(labels, log)).toEqual(labels.filter((l) => l.endsWith("/app/b")));
  expect(missingLabels(["1280 /app/a"], "ok   1280 /app/ab")).toEqual(["1280 /app/a"]);
});

test("an axe state has to appear at every width and scheme", () => {
  const labels = axeLabels('const states = [\n  ["one", "/app/a", ""],\n];');
  expect(labels).toEqual(["1280 light one", "1280 dark  one", " 390 light one", " 390 dark  one"]);
});

test("every check in the real scripts can be read", async () => {
  const text = await Bun.file(new URL("../chat-runs-check/run.ts", import.meta.url)).text();
  const { messages, unparsed } = messagesIn(text);
  expect(unparsed).toEqual([]);
  expect(messages.length).toBeGreaterThan(100);
});
