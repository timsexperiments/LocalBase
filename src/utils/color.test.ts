import { expect, test } from "bun:test";
import { shouldUseColor } from "./color";

test("color follows TTY, NO_COLOR, and FORCE_COLOR settings", () => {
  expect(shouldUseColor({ isTTY: true }, {})).toBe(true);
  expect(shouldUseColor({ isTTY: false }, {})).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { NO_COLOR: "" })).toBe(true);
  expect(shouldUseColor({ isTTY: true }, { NO_COLOR: "1" })).toBe(false);
  expect(
    shouldUseColor({ isTTY: true }, { NO_COLOR: "1", FORCE_COLOR: "1" }),
  ).toBe(false);
  expect(shouldUseColor({ isTTY: false }, { FORCE_COLOR: "1" })).toBe(true);
  expect(shouldUseColor({ isTTY: true }, { FORCE_COLOR: "0" })).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { FORCE_COLOR: "false" })).toBe(false);
  expect(shouldUseColor({ isTTY: true }, { TERM: "dumb" })).toBe(false);
});
