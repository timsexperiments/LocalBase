// The Go UI's dictation helpers against the TS dictation.tsx and dictation-audio.ts they were ported
// from, frozen as fixtures (parity/frozen.ts; testdata/dictation.json).
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/dictation.js";
import { appends, bytes, refused, wavs } from "./parity/dictation.cases.ts";
import { fixture, snap } from "./parity/frozen.ts";

const want = fixture("dictation");

describe("appendDictation", () => {
  for (const [value, text] of appends)
    test(JSON.stringify([value, text]), () => expect(go.appendDictation(value, text)).toBe(want.appendDictation[JSON.stringify([value, text])]));
});

describe("encodePcm16Wav", () => {
  for (const [name, samples, capture] of wavs)
    test(name, async () => expect(snap(await bytes(go.encodePcm16Wav(samples(), capture)))).toEqual(want.encodePcm16Wav[name]));
  for (const [name, samples] of refused)
    test(name, () => {
      expect(want.refused[name]).toBeString();
      expect(() => go.encodePcm16Wav(samples())).toThrow(want.refused[name]);
    });
});

describe("dictationForm", () => {
  test("asks the model to detect the language", () => {
    const form = go.dictationForm(new Blob(["x"], { type: "audio/wav" }), "m");
    expect(form.get("language")).toBe("auto");
    expect(form.get("model")).toBe("m");
    expect(form.get("response_format")).toBe("json");
  });
});
