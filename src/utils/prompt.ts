import { checkbox, confirm, input, number, select } from "@inquirer/prompts";
import { shouldUseColor } from "./color";

export const plainTheme = {
  prefix: { idle: "?", done: "✓" },
  spinner: { frames: ["*"], interval: 80 },
  style: {
    answer: (text: string) => text,
    message: (text: string) => text,
    error: (text: string) => `> ${text}`,
    defaultAnswer: (text: string) => `(${text})`,
    help: (text: string) => text,
    highlight: (text: string) => text,
    key: (text: string) => `<${text}>`,
    disabled: (text: string) => text,
    disabledChoice: (text: string) => text,
    description: (text: string) => text,
    renderSelectedChoices: (choices: ReadonlyArray<{ name: string }>) =>
      choices.map(({ name }) => name).join(", "),
    keysHelpTip: (keys: [string, string][]) =>
      keys.map(([key, action]) => `<${key}> ${action}`).join(" · "),
  },
  icon: { checked: "[x]", unchecked: "[ ]", cursor: ">" },
};

function promptTheme() {
  return shouldUseColor(process.stdout) ? undefined : plainTheme;
}

export async function textPrompt(
  message: string,
  defaultValue: string,
): Promise<string> {
  const value = await input({
    message,
    default: defaultValue,
    theme: promptTheme(),
  });
  return value.trim() || defaultValue;
}

export async function numberPrompt(
  message: string,
  defaultValue: number,
): Promise<number> {
  const value = await number({
    message,
    default: defaultValue,
    theme: promptTheme(),
    validate: (candidate) =>
      typeof candidate === "number" && Number.isFinite(candidate)
        ? true
        : "Please enter a valid number",
  });
  return value ?? defaultValue;
}

export async function confirmPrompt(
  message: string,
  defaultValue: boolean,
): Promise<boolean> {
  return confirm({ message, default: defaultValue, theme: promptTheme() });
}

export async function singleSelectPrompt<T extends string>(
  message: string,
  options: Array<{ name: string; value: T; disabled?: string | boolean }>,
  defaultValue: T,
): Promise<T> {
  return select({
    message,
    choices: options,
    default: defaultValue,
    theme: promptTheme(),
  });
}

export async function multiSelectPrompt<T extends string>(
  message: string,
  options: Array<{
    name: string;
    value: T;
    checked?: boolean;
    disabled?: string | boolean;
  }>,
  requireSelection = true,
): Promise<T[]> {
  return checkbox({
    message,
    choices: options,
    theme: promptTheme(),
    validate: (values) =>
      !requireSelection || values.length > 0
        ? true
        : "Select at least one option",
  });
}
