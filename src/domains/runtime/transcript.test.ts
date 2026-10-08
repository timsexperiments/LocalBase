import { describe, expect, test } from "bun:test";
import {
  isPlainTextTranscriptionFormat,
  normalizePlainTranscription,
  normalizeTranscriptText,
  normalizeTranscriptionJson,
} from "./transcript";

describe("transcript normalization", () => {
  test("collapses newlines and whitespace runs and trims", () => {
    expect(
      normalizeTranscriptText(" Hello there.\n And then...\r\n\t  done. \n"),
    ).toBe("Hello there. And then... done.");
    expect(normalizeTranscriptText("  \n ")).toBe("");
    expect(normalizeTranscriptText("clean")).toBe("clean");
  });

  test("json keeps only text", () => {
    expect(
      normalizeTranscriptionJson({ text: " a\n b", extra: 1 }, "json"),
    ).toEqual({ text: "a b" });
  });

  test("verbose_json normalizes top-level and segment text", () => {
    const result = normalizeTranscriptionJson(
      {
        task: "transcribe",
        text: " a.\n b.",
        segments: [
          { id: 0, text: " a.\n" },
          { id: 1, text: " b." },
        ],
      },
      "verbose_json",
    );
    expect(result).toEqual({
      task: "transcribe",
      text: "a. b.",
      segments: [
        { id: 0, text: "a." },
        { id: 1, text: "b." },
      ],
    });
  });

  test("plain formats only normalize text, never srt or vtt", () => {
    const srt = "1\n00:00:00,000 --> 00:00:01,000\n Hi\n\n";
    expect(normalizePlainTranscription(srt, "srt")).toBe(srt);
    expect(normalizePlainTranscription(srt, "vtt")).toBe(srt);
    expect(normalizePlainTranscription(" a\n b\n", "text")).toBe("a b");
    expect(isPlainTextTranscriptionFormat("verbose_json")).toBe(false);
    expect(isPlainTextTranscriptionFormat("srt")).toBe(true);
  });
});
