import { type LocalBaseConfig } from "../manager";
import { z } from "zod";
import { parallelSlotsSchema } from "../domains/config/parallel";

export type ConfigOverrides = Partial<LocalBaseConfig>;

const configOverridesSchema = z
  .object({
    root: z.string().min(1).optional(),
    host: z.string().min(1).optional(),
    port: z.number().int().positive().optional(),
    ctxSize: z.number().int().positive().optional(),
    parallel: parallelSlotsSchema.optional(),
    sttHost: z.string().min(1).optional(),
    sttPort: z.number().int().positive().optional(),
    startupOnBoot: z.boolean().optional(),
    selectedLlmModels: z.array(z.string().min(1)).optional(),
    selectedSttModels: z.array(z.string().min(1)).optional(),
    selectedImageModels: z.array(z.string().min(1)).optional(),
    activeLlmModel: z.string().min(1).optional(),
    activeSttModel: z.string().optional(),
    activeImageModel: z.string().optional(),
    hfToken: z.string().optional(),
  })
  .strict();

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

export async function loadTomlOverrides(
  path: string,
): Promise<ConfigOverrides> {
  const file = Bun.file(path);
  if (!(await file.exists())) throw new Error(`Config file not found: ${path}`);

  const raw = await file.text();
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

  return configOverridesSchema.parse(values);
}
