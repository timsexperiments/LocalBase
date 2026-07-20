import { parseArgs } from "node:util";
import { type ModelKind } from "../catalog";

/**
 * Parses a flag value robustly from the argument array using node:util's parseArgs.
 * Automatically supports both space-separated (--port 8787) and equals-separated (--port=8787) formats.
 */
export function parseFlag(args: string[], key: string): string | undefined {
  const optionName = key.replace(/^--?/, "");
  try {
    const { values } = parseArgs({
      args,
      options: {
        [optionName]: { type: "string" },
      },
      strict: false,
    });
    return values[optionName] as string | undefined;
  } catch {
    // Safe fallback to manual lookup in case of abnormal formatting
    const idx = args.indexOf(key);
    if (idx === -1) return undefined;
    return args[idx + 1];
  }
}

export function parseBool(
  inputValue: string | undefined,
  fallback: boolean,
): boolean {
  if (!inputValue) return fallback;
  return ["1", "true", "yes", "on"].includes(inputValue.toLowerCase());
}

export function parseList(
  inputValue: string | undefined,
): string[] | undefined {
  if (!inputValue) return undefined;
  return inputValue
    .split(",")
    .map((v) => v.trim())
    .filter(Boolean);
}

export function toInt(
  inputValue: string | undefined,
  fallback: number,
): number {
  if (!inputValue) return fallback;
  const parsed = Number(inputValue);
  return Number.isFinite(parsed) ? parsed : fallback;
}

export function parseKind(
  inputValue: string | undefined,
): ModelKind | undefined {
  if (!inputValue) return undefined;
  if (["llm", "stt", "image"].includes(inputValue))
    return inputValue as ModelKind;
  return undefined;
}

/**
 * Checks if the yes flag (-y or --yes) is specified.
 */
export function hasYesFlag(args: string[]): boolean {
  try {
    const { values } = parseArgs({
      args,
      options: {
        yes: { type: "boolean", short: "y" },
      },
      strict: false,
    });
    return !!values.yes;
  } catch {
    return args.includes("--yes") || args.includes("-y");
  }
}
