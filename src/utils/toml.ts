import { existsSync, readFileSync } from "node:fs";
import { type LocalBaseConfig } from "../manager";
import { parseOptionalParallelSlots } from "../domains/config/parallel";

export type ConfigOverrides = Partial<LocalBaseConfig>;

function parseTomlValue(value: string): string | number | boolean | string[] {
  const trimmed = value.trim();
  if (trimmed.startsWith("[") && trimmed.endsWith("]")) {
    const inner = trimmed.slice(1, -1).trim();
    if (!inner) return [];
    return inner
      .split(",")
      .map((entry) => entry.trim().replace(/^"|"$/g, ""))
      .filter(Boolean);
  }
  if (trimmed === "true") return true;
  if (trimmed === "false") return false;
  if (/^-?\d+$/.test(trimmed)) return Number(trimmed);
  return trimmed.replace(/^"|"$/g, "");
}

export function loadTomlOverrides(path: string): ConfigOverrides {
  if (!existsSync(path)) throw new Error(`Config file not found: ${path}`);

  const raw = readFileSync(path, "utf8");
  const values: Record<string, string | number | boolean | string[]> = {};

  for (const line of raw.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#") || trimmed.startsWith("["))
      continue;
    const idx = trimmed.indexOf("=");
    if (idx === -1) continue;
    const key = trimmed.slice(0, idx).trim();
    const value = trimmed.slice(idx + 1).trim();
    values[key] = parseTomlValue(value);
  }

  const selectedLlmModels = values.selectedLlmModels ?? values.llmModels;
  const selectedSttModels = values.selectedSttModels ?? values.sttModels;

  return {
    root: typeof values.root === "string" ? values.root : undefined,
    host: typeof values.host === "string" ? values.host : undefined,
    port: typeof values.port === "number" ? values.port : undefined,
    ctxSize: typeof values.ctxSize === "number" ? values.ctxSize : undefined,
    parallel: parseOptionalParallelSlots(values.parallel),
    sttHost: typeof values.sttHost === "string" ? values.sttHost : undefined,
    sttPort: typeof values.sttPort === "number" ? values.sttPort : undefined,
    startupOnBoot:
      typeof values.startupOnBoot === "boolean"
        ? values.startupOnBoot
        : undefined,
    selectedLlmModels: Array.isArray(selectedLlmModels)
      ? selectedLlmModels
      : undefined,
    selectedSttModels: Array.isArray(selectedSttModels)
      ? selectedSttModels
      : undefined,
    activeLlmModel:
      typeof values.activeLlmModel === "string"
        ? values.activeLlmModel
        : undefined,
    activeSttModel:
      typeof values.activeSttModel === "string"
        ? values.activeSttModel
        : undefined,
  };
}
