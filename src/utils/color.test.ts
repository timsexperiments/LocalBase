import { expect, test } from "bun:test";
import { shouldUseColor, stripAnsiCodes } from "./color";

test("color follows TTY, NO_COLOR, and FORCE_COLOR settings", () => {
  expect(shouldUseColor({ isTTY: true }, {})).toBe(true);
  expect(shouldUseColor({ isTTY: false }, {})).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { NO_COLOR: "" })).toBe(true);
  expect(shouldUseColor({ isTTY: true }, { NO_COLOR: "1" })).toBe(false);
  expect(
    shouldUseColor({ isTTY: true }, { NO_COLOR: "1", FORCE_COLOR: "1" }),
  ).toBe(false);
  expect(shouldUseColor({ isTTY: false }, { FORCE_COLOR: "1" })).toBe(true);
  expect(shouldUseColor({ isTTY: false }, { FORCE_COLOR: "" })).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { FORCE_COLOR: "" })).toBe(true);
  expect(shouldUseColor({ isTTY: true }, { FORCE_COLOR: "0" })).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { FORCE_COLOR: "false" })).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { TERM: "dumb" })).toBe(false);
});

test("strips ANSI sequences and every C1 control without changing Unicode", () => {
  const c1 = Array.from({ length: 32 }, (_, index) =>
    String.fromCharCode(0x80 + index),
  ).join("");
  expect(stripAnsiCodes(`é € こんにちは 🧪${c1}`)).toBe("é € こんにちは 🧪");
});
