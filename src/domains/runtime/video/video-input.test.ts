import { expect, test } from "bun:test";
import {
  MAX_VIDEO_PNG_BYTES,
  MAX_VIDEO_WAV_BYTES,
  videoGenerationInputSchema,
} from "./video-input";
import { testPcm16Wav, testPng } from "./video-input.fixtures";

function speechInput(options: {
  portrait?: string;
  audio?: string;
  frames?: number;
}) {
  return {
    kind: "speech",
    prompt: "A speaker reads one sentence.",
    width: 2,
    height: 1,
    videoFrames: options.frames ?? 16,
    fps: 16,
    portrait: {
      format: "png",
      data: options.portrait ?? testPng({ width: 2, height: 1 }),
    },
    audio: {
      format: "wav",
      data: options.audio ?? testPcm16Wav({ sampleRate: 8_000, frames: 8_000 }),
    },
  };
}

test("accepts bounded exact-canvas PNG and WAV fitting the video duration", () => {
  expect(videoGenerationInputSchema.safeParse(speechInput({})).success).toBe(
    true,
  );
});

test("rejects noncanonical or oversized inline media", () => {
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({ portrait: `${testPng({ width: 2, height: 1 })}\n` }),
    ).success,
  ).toBe(false);
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({
        audio: "A".repeat(Math.ceil(MAX_VIDEO_WAV_BYTES / 3) * 4 + 4),
      }),
    ).success,
  ).toBe(false);
  expect(MAX_VIDEO_PNG_BYTES).toBe(4 * 1024 * 1024);
});

test("rejects mismatched canvas and compressed pixel expansion", () => {
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({ portrait: testPng({ width: 1, height: 1 }) }),
    ).success,
  ).toBe(false);
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({
        portrait: testPng({
          width: 2,
          height: 1,
          inflated: new Uint8Array(100),
        }),
      }),
    ).success,
  ).toBe(false);
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({
        portrait: testPng({
          width: 2_048,
          height: 1_024,
          inflated: new Uint8Array(1),
        }),
      }),
    ).success,
  ).toBe(false);
});

test("rejects an expansion bomb through a preallocated pixel buffer", () => {
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({
        portrait: testPng({
          width: 2,
          height: 1,
          inflated: new Uint8Array(MAX_VIDEO_PNG_BYTES * 2),
        }),
      }),
    ).success,
  ).toBe(false);
});

test("accepts only 8-bit RGB or RGBA non-interlaced PNG", () => {
  for (const portrait of [
    testPng({ width: 2, height: 1, bitDepth: 16 }),
    testPng({ width: 2, height: 1, interlace: 1 }),
  ]) {
    expect(
      videoGenerationInputSchema.safeParse(speechInput({ portrait })).success,
    ).toBe(false);
  }
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({ portrait: testPng({ width: 2, height: 1, colorType: 6 }) }),
    ).success,
  ).toBe(true);
});

test("rejects audio longer than the qualified frame window", () => {
  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({
        frames: 15,
        audio: testPcm16Wav({ sampleRate: 8_000, frames: 8_000 }),
      }),
    ).success,
  ).toBe(false);
});

test("rejects a WAV chunk missing its required odd-byte pad", () => {
  const valid = Uint8Array.fromBase64(
    testPcm16Wav({ sampleRate: 8_000, frames: 8_000 }),
  );
  const malformed = new Uint8Array(valid.byteLength + 9);
  malformed.set(valid);
  malformed.set(new TextEncoder().encode("JUNK"), valid.byteLength);
  const view = new DataView(malformed.buffer);
  view.setUint32(4, malformed.byteLength - 8, true);
  view.setUint32(valid.byteLength + 4, 1, true);
  malformed[malformed.byteLength - 1] = 0;

  expect(
    videoGenerationInputSchema.safeParse(
      speechInput({ audio: malformed.toBase64() }),
    ).success,
  ).toBe(false);
});
