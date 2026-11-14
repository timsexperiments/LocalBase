// The inputs of dictation.test.ts, shared by the test (the Go side) and the fixture generator
// (the frozen TS side). See frozen.ts.

export const appends: [string, string][] = [["", "hello"], ["Draft", " more words "], ["Draft ", "x"], ["Line\n", "next"], ["keep", "   "]];

export const wavs: [string, () => Float32Array, any?][] = [
  ["ramp", () => Float32Array.from({ length: 1600 }, (_, i) => Math.sin(i / 10) * 1.2)],
  ["edges", () => Float32Array.from([-1, 1, 0, NaN, Infinity, -Infinity, 0.5, -0.5])],
  ["other rate", () => Float32Array.from({ length: 480 }, (_, i) => i / 480), { sampleRate: 48_000, maxSeconds: 1 }],
];

export const refused: [string, () => Float32Array][] = [["empty", () => new Float32Array(0)], ["too long", () => new Float32Array(16_000 * 60 + 1)]];

export const bytes = async (b: Blob) => new Uint8Array(await b.arrayBuffer());
