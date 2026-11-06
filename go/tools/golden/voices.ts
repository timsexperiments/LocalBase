// Golden custom-voice normalization and name rules from the TypeScript code.
//
//   bun run go/tools/golden/voices.ts > go/internal/voices/testdata/voices.json
//
// Audio inputs are described by specs and built from a 32-bit LCG with exact
// arithmetic only; voices_test.go builds the same bytes from the same specs
// (the recorded input sha256 proves it) and must reproduce every output.
import { createHash } from "node:crypto";
import { normalizeVoiceAudio } from "../../../src/domains/voices/voice-audio";
import {
  customVoiceNameSchema,
  VoiceError,
  voiceNameKey,
} from "../../../src/domains/voices/voice-contract";

type Spec = {
  case: string;
  tag?: number; // 1 PCM, 3 float, other values are rejected
  channels?: number;
  rate?: number;
  bits?: number;
  frames?: number;
  seed?: number;
  amp?: number; // float amplitude
  extensible?: boolean;
  blockAlign?: number;
  silent?: boolean;
  nanAt?: number;
  noFmt?: boolean;
  noData?: boolean;
  dupFmt?: boolean;
  dupData?: boolean;
  preChunks?: { id: string; size: number }[];
  truncate?: number; // drop this many trailing bytes
  raw?: string; // base64; replaces everything
};

export function build(spec: Spec): Uint8Array {
  if (spec.raw !== undefined) return Uint8Array.from(Buffer.from(spec.raw, "base64"));
  let x = spec.seed ?? 1;
  const next = () => (x = (Math.imul(x, 1664525) + 1013904223) >>> 0);
  const tag = spec.tag ?? 1;
  const channels = spec.channels ?? 1;
  const rate = spec.rate ?? 16000;
  const bits = spec.bits ?? 16;
  const frames = spec.frames ?? rate * 4;
  const bytesPerSample = bits / 8;
  const blockAlign = spec.blockAlign ?? channels * bytesPerSample;
  const parts: number[] = [];
  const u8 = (v: number) => parts.push(v & 0xff);
  const u16 = (v: number) => (u8(v), u8(v >>> 8));
  const u32 = (v: number) => (u16(v & 0xffff), u16(v >>> 16));
  const ascii = (s: string) => [...s].forEach((c) => u8(c.charCodeAt(0)));
  const fmt = () => {
    ascii("fmt ");
    u32(spec.extensible ? 40 : 16);
    u16(spec.extensible ? 0xfffe : tag);
    u16(channels);
    u32(rate);
    u32(rate * blockAlign);
    u16(blockAlign);
    u16(bits);
    if (spec.extensible) {
      u16(22);
      u16(bits);
      u32(0);
      u16(tag);
      for (let i = 0; i < 14; i++) u8(0);
    }
  };
  const data = () => {
    ascii("data");
    const size = frames * channels * bytesPerSample;
    u32(size);
    const view = new DataView(new ArrayBuffer(8));
    for (let i = 0; i < frames * channels; i++) {
      if (tag === 3) {
        let v = spec.silent ? 0 : ((next() >>> 8) / 16777216 * 2 - 1) * (spec.amp ?? 0.5);
        if (spec.nanAt === i) v = NaN;
        if (bits === 32) view.setFloat32(0, v, true);
        else view.setFloat64(0, v, true);
        for (let k = 0; k < bytesPerSample; k++) u8(view.getUint8(k));
      } else {
        for (let k = 0; k < bytesPerSample; k++) {
          u8(spec.silent ? (bits === 8 ? 128 : 0) : next() >>> 24);
        }
      }
    }
    if (size % 2) u8(0);
  };
  ascii("RIFF");
  u32(0);
  ascii("WAVE");
  if (!spec.noFmt) fmt();
  for (const c of spec.preChunks ?? []) {
    ascii(c.id);
    u32(c.size);
    for (let i = 0; i < c.size + (c.size % 2); i++) u8(0);
  }
  if (spec.dupFmt) fmt();
  if (!spec.noData) data();
  if (spec.dupData) data();
  const bytes = Uint8Array.from(parts);
  new DataView(bytes.buffer).setUint32(4, bytes.length - 8, true);
  return spec.truncate ? bytes.slice(0, bytes.length - spec.truncate) : bytes;
}

const specs: Spec[] = [
  { case: "pcm16 mono 16k", rate: 16000, frames: 64000 },
  { case: "pcm16 stereo 44.1k", channels: 2, rate: 44100, frames: 198450, seed: 7 },
  { case: "pcm24 mono 48k", bits: 24, rate: 48000, frames: 153600, seed: 3 },
  { case: "pcm32 mono 22.05k", bits: 32, rate: 22050, frames: 77175, seed: 11 },
  { case: "pcm8 mono 8k", bits: 8, rate: 8000, frames: 40000, seed: 5 },
  { case: "float32 stereo 48k", tag: 3, bits: 32, channels: 2, rate: 48000, frames: 168000, seed: 13 },
  { case: "float64 mono 96k", tag: 3, bits: 64, rate: 96000, frames: 300000, seed: 17, amp: 1.5 },
  { case: "pcm16 mono 24k (no resample)", rate: 24000, frames: 96000, seed: 19 },
  { case: "extensible pcm16 32k", extensible: true, rate: 32000, frames: 112000, seed: 23 },
  { case: "pcm16 mono 192k", rate: 192000, frames: 576000, seed: 29 },
  { case: "pcm16 mono 11.025k", rate: 11025, frames: 44100, seed: 31 },
  { case: "pcm16 3 channels 12k with odd LIST chunk", channels: 3, rate: 12000, frames: 48000, seed: 37, preChunks: [{ id: "LIST", size: 7 }] },
  { case: "pcm8 odd data size", bits: 8, rate: 8000, frames: 24001, seed: 41 },
  { case: "exactly 3 s", rate: 16000, frames: 48000, seed: 43 },
  { case: "exactly 30 s", rate: 8000, frames: 240000, seed: 47 },
  { case: "too short (tie 2.25 s)", rate: 16000, frames: 36000 },
  { case: "too short 0.1 s", rate: 8000, frames: 800 },
  { case: "too long", rate: 8000, frames: 240400 },
  { case: "silent", rate: 16000, frames: 64000, silent: true },
  { case: "silent pcm8", bits: 8, rate: 16000, frames: 64000, silent: true },
  { case: "float NaN", tag: 3, bits: 32, rate: 16000, frames: 64000, nanAt: 1000 },
  { case: "adpcm", tag: 2, rate: 16000, frames: 64000 },
  { case: "12-bit", bits: 12, rate: 16000, frames: 100 },
  { case: "float16", tag: 3, bits: 16, rate: 16000, frames: 100 },
  { case: "9 channels", channels: 9, rate: 16000, frames: 100 },
  { case: "rate 7999", rate: 7999, frames: 100 },
  { case: "rate 192001", rate: 192001, frames: 100 },
  { case: "bad block align", blockAlign: 3, rate: 16000, frames: 100 },
  { case: "no fmt", noFmt: true, rate: 16000, frames: 100 },
  { case: "no data", noData: true },
  { case: "two fmt", dupFmt: true, rate: 16000, frames: 100 },
  { case: "two data", dupData: true, rate: 16000, frames: 100 },
  { case: "truncated", rate: 16000, frames: 64000, truncate: 10 },
  { case: "not wav", raw: Buffer.from("ID3\u0004\u0000\u0000\u0000\u0000\u0000\u0000hello").toString("base64") },
  { case: "empty", raw: "" },
  { case: "riff only", raw: Buffer.from("RIFF\u0004\u0000\u0000\u0000WAVE").toString("base64") },
];

const sha = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");
const audio = specs.map((spec) => {
  const input = build(spec);
  try {
    const out = normalizeVoiceAudio(input);
    return { spec, inputSha256: sha(input), sha256: sha(out.wav), sizeBytes: out.wav.byteLength, sampleCount: out.sampleCount, durationMs: out.durationMs };
  } catch (error) {
    if (!(error instanceof VoiceError)) throw error;
    return { spec, inputSha256: sha(input), error: { code: error.code, message: error.message } };
  }
});

const nameInputs = [
  "Narrator", "  Narrator  ", "Two\t\n words", "a".repeat(48), "a".repeat(49), "😀".repeat(24), "😀".repeat(25),
  "", "   ", "  ﻿", "Default", "DEFAULT", "ＤＥＦＡＵＬＴ", "af_heart", "AF_Bella", "voice-0123456789ab",
  "voice-0123456789AB", "bad\u0007bell", "zero​width", "éclair", "éclair", "Ｆｕｌｌ", "ﬁne", "İstanbul",
  "ΟΔΟΣ", "ΟΔΟΣ.", "Σ", "ΑΣ'Β", "Straße", "ǅ", "Ⅻ", "x y", "tab\u000bvt", "\u0085next",
];
const names = nameInputs.map((input) => {
  const parsed = customVoiceNameSchema.safeParse(input);
  return parsed.success
    ? { input, name: parsed.data, key: voiceNameKey(parsed.data) }
    : { input, error: parsed.error.issues[0]?.message, key: voiceNameKey(input) };
});

console.log(JSON.stringify({ audio, names }, null, 1));
