/**
 * Catalog content rule: entries carry source-derived facts and capabilities
 * only. No anecdotal quality claims, rankings, scores or machine-specific
 * speed figures; unknown facts stay unset.
 */
import { describe, expect, test } from "bun:test";
import { CATALOG_DATA } from "./models";
import { FORMER_CATALOG_DATA } from "./former";
import {
  MEASUREMENT_HARDWARE,
  MEMORY_MEASUREMENTS,
} from "../domains/runtime/memory-measurements";

type Entry = Record<string, unknown> & { modelId: string };

const builtIn = CATALOG_DATA as readonly Entry[];
const former = FORMER_CATALOG_DATA as readonly Entry[];
const everyEntry = [...builtIn, ...former];

/** Free-text fields shown to users (`catch` is the `summary` in the API). */
const TEXT_FIELDS = ["displayName", "catch", "notes"] as const;

/** Subjective or comparative wording that is not a source-derived fact. */
const SUBJECTIVE =
  /\b(best|better|great|good|strong(?:er|est)?|top[- ]?(?:tier|end)|ideal(?:ly)?|fast(?:er|est)?|quick(?:er|est)?|slow(?:er|est)?|high[- ]quality|quality|excellent|exceptional|superb|state[- ]of[- ]the[- ]art|gold standard|premier|perfect|highly|capable|popular|recommended|lightweight|optimi[sz]ed|powerful|impressive|leading|accurate|accuracy|real[- ]time|frontier)\b/i;

/**
 * Reviewed phrases that use a flagged word to qualify a claim rather than
 * make one. Each is removed before matching; keep this list short.
 */
const ALLOWED_PHRASES = [
  /human voice quality has not been assessed/gi,
  /image quality relative to [\w. -]+? has not been assessed locally/gi,
  /lip-sync quality and longer clips are not qualified/gi,
];

/** Throughput and latency figures depend on the machine that measured them. */
const SPEED_FIGURE =
  /\b\d+(?:\.\d+)?\s*(?:tok(?:ens)?\/s|it\/s|docs\/s|ms per|s per (?:request|image|step))\b|\b\d+(?:\.\d+)?[x×] (?:faster|slower)\b/i;

function flagged(text: string): string | null {
  const stripped = ALLOWED_PHRASES.reduce(
    (rest, phrase) => rest.replace(phrase, ""),
    text,
  );
  return (
    SUBJECTIVE.exec(stripped)?.[0] ?? SPEED_FIGURE.exec(stripped)?.[0] ?? null
  );
}

describe("catalog text is factual", () => {
  test.each(everyEntry.map((entry) => [entry.modelId, entry] as const))(
    "%s has no subjective, ranking or speed wording",
    (_id, entry) => {
      const hits = TEXT_FIELDS.flatMap((field) => {
        const value = entry[field];
        if (typeof value !== "string") return [];
        const hit = flagged(value);
        return hit ? [`${field}: "${hit}"`] : [];
      });
      expect(hits).toEqual([]);
    },
  );

  test("the lint catches the wording it exists to block", () => {
    for (const sample of [
      "Good default STT latency/quality tradeoff.",
      "Best quality Whisper family model for local deployment.",
      "Stronger than Clef Flash on intent; Clef Flash is the faster default.",
      "Great coding quality per watt; ideal default for a 12GB GPU.",
      "Top-tier 27B model.",
      "High reasoning quality.",
      "Strong FIM capability.",
      "Answers in 0.5 s per request at 48 tok/s.",
    ])
      expect(flagged(sample)).not.toBeNull();
    expect(flagged("Human voice quality has not been assessed.")).toBeNull();
  });

  test("no entry carries a coding score", () => {
    expect(
      everyEntry
        .filter((entry) => "codingScore" in entry)
        .map((entry) => entry.modelId),
    ).toEqual([]);
  });
});

describe("built-in facts come from the pinned files", () => {
  test.each(builtIn.map((entry) => [entry.modelId, entry] as const))(
    "%s lists its storage as the sum of its pinned artifacts",
    (_id, entry) => {
      const artifacts = entry.artifacts as { expectedSizeBytes: number }[];
      const bytes = artifacts.reduce(
        (sum, artifact) => sum + artifact.expectedSizeBytes,
        0,
      );
      expect(entry.storageGb as number).toBeCloseTo(bytes / 1e9, 2);
    },
  );

  test("every built-in LLM states the context its GGUF declares", () => {
    expect(
      builtIn
        .filter(
          (entry) =>
            entry.kind === "llm" &&
            typeof entry.contextWindowTokens !== "number",
        )
        .map((entry) => entry.modelId),
    ).toEqual([]);
  });

  test("checked-in measurements share the machine the labels name", () => {
    // Model metadata labels every measured figure with MEASUREMENT_HARDWARE.
    expect(
      MEMORY_MEASUREMENTS.filter(
        (record) => record.hardware !== MEASUREMENT_HARDWARE,
      ).map((record) => record.modelId),
    ).toEqual([]);
  });
});
