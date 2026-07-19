import { checkbox, confirm, input, number, select } from "@inquirer/prompts";

export async function textPrompt(
  message: string,
  defaultValue: string,
): Promise<string> {
  const value = await input({ message, default: defaultValue });
  return value.trim() || defaultValue;
}

export async function numberPrompt(
  message: string,
  defaultValue: number,
): Promise<number> {
  const value = await number({
    message,
    default: defaultValue,
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
  return confirm({ message, default: defaultValue });
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
): Promise<T[]> {
  return checkbox({
    message,
    choices: options,
    validate: (values) =>
      values.length > 0 ? true : "Select at least one option",
  });
}
