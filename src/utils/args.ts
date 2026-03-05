import { type ModelKind } from "../catalog";

export function parseFlag(args: string[], key: string): string | undefined {
  const idx = args.indexOf(key);
  if (idx === -1) return undefined;
  return args[idx + 1];
}

export function parseBool(inputValue: string | undefined, fallback: boolean): boolean {
  if (!inputValue) return fallback;
  return ["1", "true", "yes", "on"].includes(inputValue.toLowerCase());
}

export function parseList(inputValue: string | undefined): string[] | undefined {
  if (!inputValue) return undefined;
  return inputValue
    .split(",")
    .map((v) => v.trim())
    .filter(Boolean);
}

export function toInt(inputValue: string | undefined, fallback: number): number {
  if (!inputValue) return fallback;
  const parsed = Number(inputValue);
  return Number.isFinite(parsed) ? parsed : fallback;
}

export function parseKind(inputValue: string | undefined): ModelKind | undefined {
  if (!inputValue) return undefined;
  if (["llm", "stt", "tts", "image", "video", "audio"].includes(inputValue)) return inputValue as ModelKind;
  return undefined;
}

export function hasYesFlag(args: string[]): boolean {
  return args.includes("--yes") || args.includes("-y");
}
