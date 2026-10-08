export type TranscriptionResponseFormat =
  "json" | "text" | "srt" | "verbose_json" | "vtt";

/**
 * Collapses whisper.cpp segment artifacts (newlines between segments, leading
 * and trailing spaces, whitespace runs) into the single clean string OpenAI
 * returns.
 */
export function normalizeTranscriptText(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

type TranscriptionBody = {
  text: string;
  segments?: Array<{ text: string } & Record<string, unknown>>;
} & Record<string, unknown>;

/** Normalizes a validated whisper JSON body for the requested response format. */
export function normalizeTranscriptionJson(
  data: TranscriptionBody,
  format: TranscriptionResponseFormat,
): Record<string, unknown> {
  const text = normalizeTranscriptText(data.text);
  if (format !== "verbose_json") return { text };
  return {
    ...data,
    text,
    ...(data.segments
      ? {
          segments: data.segments.map((segment) => ({
            ...segment,
            text: normalizeTranscriptText(segment.text),
          })),
        }
      : {}),
  };
}

/** Whether whisper returns this format as a plain-text body instead of JSON. */
export function isPlainTextTranscriptionFormat(
  format: TranscriptionResponseFormat,
): format is "text" | "srt" | "vtt" {
  return format === "text" || format === "srt" || format === "vtt";
}

/** Normalizes a plain-text whisper body; srt/vtt keep their structural newlines. */
export function normalizePlainTranscription(
  body: string,
  format: "text" | "srt" | "vtt",
): string {
  return format === "text" ? normalizeTranscriptText(body) : body;
}
