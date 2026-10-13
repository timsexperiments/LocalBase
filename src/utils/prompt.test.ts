import { expect, test } from "bun:test";
import { plainTheme } from "./prompt";

test("plain prompt theme keeps all select and checkbox output uncolored", () => {
  const styles = plainTheme.style;
  const text = "prompt text";
  expect(styles.message(text)).not.toMatch(/\x1b\[/);
  for (const style of [
    styles.answer,
    styles.error,
    styles.defaultAnswer,
    styles.help,
    styles.highlight,
    styles.key,
    styles.disabled,
    styles.disabledChoice,
    styles.description,
  ]) {
    expect(style(text)).not.toMatch(/\x1b\[/);
  }
  expect(styles.keysHelpTip([["↑↓", "navigate"]])).not.toMatch(/\x1b\[/);
  expect(plainTheme.icon).toEqual({
    checked: "[x]",
    unchecked: "[ ]",
    cursor: ">",
  });
});
