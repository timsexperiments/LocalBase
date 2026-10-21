// The Go UI's dictation helpers against TS dictation.tsx and dictation-audio.ts.
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/dictation.js";
import { appendDictation } from "../../../src/ui/dictation";
import { encodePcm16Wav } from "../../../src/ui/dictation-audio";

describe("appendDictation", () => {
  for (const [value, text] of [["", "hello"], ["Draft", " more words "], ["Draft ", "x"], ["Line\n", "next"], ["keep", "   "]])
    test(JSON.stringify([value, text]), () => expect(go.appendDictation(value, text)).toBe(appendDictation(value, text)));
});

describe("encodePcm16Wav", () => {
  const bytes = async (b: Blob) => new Uint8Array(await b.arrayBuffer());
  const inputs: [string, Float32Array, any?][] = [
    ["ramp", Float32Array.from({ length: 1600 }, (_, i) => Math.sin(i / 10) * 1.2)],
    ["edges", Float32Array.from([-1, 1, 0, NaN, Infinity, -Infinity, 0.5, -0.5])],
    ["other rate", Float32Array.from({ length: 480 }, (_, i) => i / 480), { sampleRate: 48_000, maxSeconds: 1 }],
  ];
  for (const [name, samples, capture] of inputs)
    test(name, async () => expect(await bytes(go.encodePcm16Wav(samples, capture))).toEqual(await bytes(encodePcm16Wav(samples, capture))));
  for (const [name, samples] of [["empty", new Float32Array(0)], ["too long", new Float32Array(16_000 * 60 + 1)]] as const)
    test(name, () => {
      expect(() => go.encodePcm16Wav(samples)).toThrow();
      let message = "";
      try { encodePcm16Wav(samples); } catch (e) { message = (e as Error).message; }
      expect(() => go.encodePcm16Wav(samples)).toThrow(message);
    });
});
