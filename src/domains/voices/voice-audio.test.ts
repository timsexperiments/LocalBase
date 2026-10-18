import { describe, expect, test } from "bun:test";
import {
  encodePcm16Wav,
  normalizeVoiceAudio,
  resampleMono,
} from "./voice-audio";
import { VOICE_MAX_UPLOAD_BYTES, VoiceError } from "./voice-contract";
import { toneWav } from "../../test/voice-audio-fixture";

function header(wav: Uint8Array) {
  const view = new DataView(wav.buffer, wav.byteOffset, wav.byteLength);
  return {
    format: view.getUint16(20, true),
    channels: view.getUint16(22, true),
    sampleRate: view.getUint32(24, true),
    bits: view.getUint16(34, true),
    dataLength: view.getUint32(40, true),
  };
}

function rejection(bytes: Uint8Array): VoiceError {
  try {
    normalizeVoiceAudio(bytes);
  } catch (error) {
    if (error instanceof VoiceError) return error;
    throw error;
  }
  throw new Error("Expected the audio to be rejected.");
}

describe("normalizeVoiceAudio", () => {
  test("rewrites 48 kHz stereo PCM16 as 24 kHz mono PCM16", () => {
    const result = normalizeVoiceAudio(toneWav());
    expect(header(result.wav)).toEqual({
      format: 1,
      channels: 1,
      sampleRate: 24_000,
      bits: 16,
      dataLength: 96_000 * 2,
    });
    expect(result.wav.byteLength).toBe(44 + 96_000 * 2);
    expect(result.sampleCount).toBe(96_000);
    expect(result.durationMs).toBe(4_000);
  });

  test.each([
    ["8-bit PCM", { bits: 8, sampleRate: 16_000, channels: 1 }],
    ["24-bit PCM", { bits: 24, sampleRate: 44_100 }],
    ["32-bit PCM", { bits: 32, sampleRate: 22_050, channels: 1 }],
    ["32-bit float", { bits: 32, format: 3, sampleRate: 24_000 }],
    ["64-bit float", { bits: 64, format: 3, sampleRate: 8_000 }],
    ["extensible PCM", { extensible: true, sampleRate: 96_000 }],
  ] as const)("accepts %s", (_label, options) => {
    const result = normalizeVoiceAudio(toneWav(options));
    expect(header(result.wav).sampleRate).toBe(24_000);
    expect(header(result.wav).channels).toBe(1);
    expect(result.durationMs).toBe(4_000);
  });

  test("keeps the tone level and frequency through resampling", () => {
    const result = normalizeVoiceAudio(toneWav({ sampleRate: 44_100 }));
    const view = new DataView(result.wav.buffer, 44);
    const samples = Array.from(
      { length: result.sampleCount },
      (_, index) => view.getInt16(index * 2, true) / 32_768,
    );
    // Skip the filter edges, then measure RMS and zero crossings.
    const middle = samples.slice(2_400, -2_400);
    const rms = Math.sqrt(
      middle.reduce((sum, value) => sum + value * value, 0) / middle.length,
    );
    expect(rms).toBeGreaterThan(0.5 / Math.SQRT2 - 0.01);
    expect(rms).toBeLessThan(0.5 / Math.SQRT2 + 0.01);
    let crossings = 0;
    for (let index = 1; index < middle.length; index++) {
      if (middle[index - 1]! < 0 !== middle[index]! < 0) crossings++;
    }
    const hz = crossings / 2 / (middle.length / 24_000);
    expect(Math.abs(hz - 220)).toBeLessThan(2);
  });

  test("drops metadata chunks such as LIST/INFO", () => {
    const info = new TextEncoder().encode("INFOIART\u0006\0\0\0Person");
    const result = normalizeVoiceAudio(
      toneWav({ extraChunks: [{ id: "LIST", body: info }] }),
    );
    expect(new TextDecoder().decode(result.wav)).not.toContain("Person");
    expect(new TextDecoder().decode(result.wav.subarray(0, 44))).toContain(
      "data",
    );
  });

  test.each([
    ["an MP3", new Uint8Array([0x49, 0x44, 0x33, 4, 0, 0, 0, 0, 0, 0, 0, 0])],
    ["random bytes", crypto.getRandomValues(new Uint8Array(4096))],
    ["an empty file", new Uint8Array()],
  ])("rejects %s as unsupported", (_label, bytes) => {
    expect(rejection(bytes).code).toBe("unsupported_audio_format");
  });

  test("rejects compressed, malformed and out-of-range formats", () => {
    expect(rejection(toneWav({ format: 2 })).message).toContain("uncompressed");
    const twelveBit = toneWav();
    new DataView(twelveBit.buffer).setUint16(34, 12, true);
    expect(rejection(twelveBit).message).toContain("12-bit");
    expect(rejection(toneWav({ sampleRate: 4_000 })).message).toContain(
      "sample rate",
    );
    expect(rejection(toneWav({ channels: 9, seconds: 3.1 })).message).toContain(
      "channels",
    );
    const truncated = toneWav().subarray(0, 1000);
    expect(rejection(truncated).message).toContain("truncated");
    const nan = toneWav({ format: 3, bits: 32, channels: 1 });
    new DataView(nan.buffer).setFloat32(44 + 400, Number.NaN, true);
    expect(rejection(nan).message).toContain("invalid float");
  });

  test("enforces length, size and silence bounds", () => {
    expect(rejection(toneWav({ seconds: 2.9 })).code).toBe("audio_too_short");
    expect(
      rejection(toneWav({ seconds: 30.5, channels: 1, sampleRate: 8_000 }))
        .code,
    ).toBe("audio_too_long");
    expect(rejection(toneWav({ amplitude: 0 })).code).toBe("audio_silent");
    const oversized = new Uint8Array(VOICE_MAX_UPLOAD_BYTES + 1);
    oversized.set(toneWav({ seconds: 3 }));
    expect(rejection(oversized).code).toBe("audio_too_large");
  });
});

test("resampleMono keeps unity DC gain", () => {
  const output = resampleMono(
    new Float32Array(4_800).fill(0.25),
    48_000,
    24_000,
  );
  expect(output.length).toBe(2_400);
  expect(output[1_200]).toBeCloseTo(0.25, 4);
});

test("encodePcm16Wav clamps out-of-range samples", () => {
  const wav = encodePcm16Wav(new Float32Array([2, -2]), 24_000);
  const view = new DataView(wav.buffer);
  expect(view.getInt16(44, true)).toBe(32_767);
  expect(view.getInt16(46, true)).toBe(-32_768);
});
