/**
 * Plain-language wording for a model's details, shared by the browser UI and
 * `local-base models show`. Kept free of runtime imports.
 */

export const modeLabels = {
  llm: "Chat",
  image: "Image",
  tts: "Speech",
  stt: "Transcription",
  video: "Video",
  embedding: "Embeddings",
} as const;
export type ModeLabelKey = keyof typeof modeLabels;

export const licenseLabels = {
  open: "Commercial use OK",
  conditional: "Commercial use with conditions",
  prohibited: "Non-commercial only",
} as const;

/** How far LocalBase trusts a catalog entry. */
export const catalogTrustLabels = {
  qualified: "Qualified by LocalBase",
  community: "Community catalog, not qualified",
  user: "Added by you, not qualified",
  modified:
    "Qualified entry you changed; reference measurements no longer apply",
} as const;

/** Which catalog tier an entry came from. */
export const catalogTierLabels = {
  "built-in": "Built-in catalog",
  remote: "Remote catalog",
  user: "Your catalog",
} as const;

/** Where a memory figure came from. */
export const memoryFigureLabels = {
  estimate: "Estimate",
  measured: "Measured",
  observed: "Seen on this machine",
} as const;

export const fitLabels = {
  fits: "Fits",
  tight: "Tight fit",
  "wont-fit": "Won't fit",
} as const;

const gibibyte = 1024 ** 3;

/** Bytes as GB with one decimal, the unit the catalog uses everywhere. */
export function formatGb(bytes: number): string {
  return `${(bytes / gibibyte).toFixed(1)} GB`;
}

/** File sizes, scaled so small files are not shown as 0.0 GB. */
export function formatFileSize(bytes: number | null): string {
  if (bytes === null) return "Unknown size";
  if (bytes < 1024 ** 2) return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  if (bytes < gibibyte) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  return formatGb(bytes);
}

export function formatTokens(tokens: number): string {
  return `${tokens.toLocaleString("en-US")} tokens`;
}

/**
 * Describes a launch-shape key such as `ctx=8192;slots=1;kv=f16/f16` in
 * words. Unknown keys are returned unchanged rather than guessed at.
 */
export function describeLaunchShape(shape: string): string {
  if (shape === "default") return "standard settings";
  const parts = new Map(
    shape.split(";").map((part) => {
      const [key = "", ...value] = part.split("=");
      return [key, value.join("=")] as const;
    }),
  );
  const ctx = Number(parts.get("ctx"));
  const slots = Number(parts.get("slots"));
  const kv = parts.get("kv");
  if (!Number.isInteger(ctx) || !Number.isInteger(slots) || !kv) return shape;
  const [keyType, valueType] = kv.split("/");
  const cache = keyType === valueType ? keyType : kv;
  return `${ctx.toLocaleString("en-US")}-token context, ${slots} ${slots === 1 ? "request" : "requests"} at a time, ${cache} cache`;
}

/** This machine's fit with the memory left over, or how far short it falls. */
export function describeFit(
  fit: Readonly<{
    verdict: keyof typeof fitLabels;
    systemMemoryGb: number;
    headroomGb: number;
  }>,
): string {
  const round = (value: number) => Math.round(value * 10) / 10;
  const spare =
    fit.headroomGb >= 0
      ? `${round(fit.headroomGb)} GB to spare`
      : `${round(-fit.headroomGb)} GB short`;
  return `${fitLabels[fit.verdict]}, ${spare} of ${round(fit.systemMemoryGb)} GB`;
}
