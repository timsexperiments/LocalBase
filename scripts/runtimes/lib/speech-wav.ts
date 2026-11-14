// The TypeScript gateway's speech WAV checks, copied verbatim from
// src/domains/runtime/speech-supervisor.ts when the runtime release scripts stopped importing src/
// (retirement R4b). The Kokoro release test holds the runtime's output to them. The Go gateway's
// own check is wavSamples in go/internal/gateway/infer/speech.go.
export const SPEECH_MAX_FRAMES = 256;

export const SPEECH_SAMPLES_PER_FRAME = 1_920;

export const SPEECH_SAMPLE_RATE_HZ = 24_000;

export const SPEECH_MAX_OUTPUT_BYTES = 1024 * 1024;

export class SpeechOutputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "SpeechOutputError";
  }
}

export type ValidatedWav = Readonly<{
  bytes: Uint8Array;
  sampleCount: number;
  dataOffset: number;
}>;

function fourCc(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(...bytes.subarray(offset, offset + 4));
}

export function validateSpeechWav(bytes: Uint8Array): ValidatedWav {
  if (bytes.byteLength > SPEECH_MAX_OUTPUT_BYTES) {
    throw new SpeechOutputError("Speech output exceeds the 1 MiB limit.");
  }
  if (
    bytes.byteLength < 44 ||
    fourCc(bytes, 0) !== "RIFF" ||
    fourCc(bytes, 8) !== "WAVE"
  ) {
    throw new SpeechOutputError("Speech output is not a RIFF/WAVE file.");
  }

  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (view.getUint32(4, true) + 8 !== bytes.byteLength) {
    throw new SpeechOutputError(
      "Speech WAV length does not match its RIFF header.",
    );
  }

  let foundFormat = false;
  let dataLength: number | undefined;
  let dataOffset = 0;
  let offset = 12;
  for (; offset + 8 <= bytes.byteLength;) {
    const chunkType = fourCc(bytes, offset);
    const chunkLength = view.getUint32(offset + 4, true);
    const payloadOffset = offset + 8;
    const paddedEnd = payloadOffset + chunkLength + (chunkLength % 2);
    if (paddedEnd > bytes.byteLength) {
      throw new SpeechOutputError(
        `Speech WAV chunk ${chunkType} extends beyond the file.`,
      );
    }
    if (chunkType === "fmt ") {
      if (foundFormat || chunkLength !== 16) {
        throw new SpeechOutputError("Speech WAV has an invalid fmt chunk.");
      }
      foundFormat = true;
      const audioFormat = view.getUint16(payloadOffset, true);
      const channels = view.getUint16(payloadOffset + 2, true);
      const sampleRate = view.getUint32(payloadOffset + 4, true);
      const byteRate = view.getUint32(payloadOffset + 8, true);
      const blockAlign = view.getUint16(payloadOffset + 12, true);
      const bitsPerSample = view.getUint16(payloadOffset + 14, true);
      if (
        audioFormat !== 1 ||
        channels !== 1 ||
        sampleRate !== SPEECH_SAMPLE_RATE_HZ ||
        byteRate !== SPEECH_SAMPLE_RATE_HZ * 2 ||
        blockAlign !== 2 ||
        bitsPerSample !== 16
      ) {
        throw new SpeechOutputError(
          "Speech output must be PCM16 mono WAV at 24 kHz.",
        );
      }
    } else if (chunkType === "data") {
      if (dataLength !== undefined || chunkLength === 0 || chunkLength % 2) {
        throw new SpeechOutputError("Speech WAV has an invalid data chunk.");
      }
      dataLength = chunkLength;
      dataOffset = payloadOffset;
    }
    offset = paddedEnd;
  }

  if (offset !== bytes.byteLength) {
    throw new SpeechOutputError("Speech WAV has an incomplete trailing chunk.");
  }

  if (!foundFormat || dataLength === undefined) {
    throw new SpeechOutputError("Speech WAV is missing fmt or data audio.");
  }
  const sampleCount = dataLength / 2;
  if (sampleCount > SPEECH_MAX_FRAMES * SPEECH_SAMPLES_PER_FRAME) {
    throw new SpeechOutputError("Speech WAV exceeds the native frame limit.");
  }
  return Object.freeze({ bytes, sampleCount, dataOffset });
}
