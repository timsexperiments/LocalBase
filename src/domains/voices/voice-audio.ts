import {
  VOICE_MAX_SECONDS,
  VOICE_MAX_UPLOAD_BYTES,
  VOICE_MIN_SECONDS,
  VOICE_SAMPLE_RATE_HZ,
  VoiceError,
} from "./voice-contract";

/**
 * Reference clips are decoded here, in bounded TypeScript, instead of handing
 * an untrusted file to a native decoder. Only RIFF/WAVE PCM and IEEE float
 * are accepted; the UI converts microphone recordings and other formats to
 * WAV in the browser before upload.
 */

const MIN_INPUT_RATE_HZ = 8_000;
const MAX_INPUT_RATE_HZ = 192_000;
const MAX_CHANNELS = 8;
/** Below roughly -60 dBFS peak the clip carries no usable speech. */
const SILENCE_PEAK = 0.001;
/** Windowed-sinc half width, in zero crossings of the low-pass kernel. */
const RESAMPLE_ZERO_CROSSINGS = 8;

const WAVE_FORMAT_PCM = 0x0001;
const WAVE_FORMAT_IEEE_FLOAT = 0x0003;
const WAVE_FORMAT_EXTENSIBLE = 0xfffe;

export type NormalizedVoiceAudio = Readonly<{
  /** 24 kHz mono PCM16 RIFF/WAVE with only `fmt ` and `data` chunks. */
  wav: Uint8Array;
  sampleCount: number;
  durationMs: number;
}>;

type DecodedWav = {
  sampleRate: number;
  channels: number;
  frames: number;
  read: (frame: number, channel: number) => number;
};

function unsupported(message: string): never {
  throw new VoiceError("unsupported_audio_format", message);
}

function fourCc(view: DataView, offset: number): string {
  return String.fromCharCode(
    view.getUint8(offset),
    view.getUint8(offset + 1),
    view.getUint8(offset + 2),
    view.getUint8(offset + 3),
  );
}

function decodeWav(bytes: Uint8Array): DecodedWav {
  if (bytes.byteLength > VOICE_MAX_UPLOAD_BYTES) {
    throw new VoiceError(
      "audio_too_large",
      `Audio exceeds the ${VOICE_MAX_UPLOAD_BYTES / (1024 * 1024)} MiB limit.`,
    );
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (
    bytes.byteLength < 12 ||
    fourCc(view, 0) !== "RIFF" ||
    fourCc(view, 8) !== "WAVE"
  ) {
    unsupported("Upload a WAV file (RIFF/WAVE).");
  }

  let format:
    | { tag: number; channels: number; sampleRate: number; bits: number }
    | undefined;
  let data: { offset: number; length: number } | undefined;
  let offset = 12;
  while (offset + 8 <= bytes.byteLength) {
    const id = fourCc(view, offset);
    const size = view.getUint32(offset + 4, true);
    const body = offset + 8;
    if (size > bytes.byteLength - body) {
      unsupported("The WAV file is truncated or has an invalid chunk size.");
    }
    if (id === "fmt ") {
      if (format) unsupported("The WAV file has more than one format chunk.");
      if (size < 16) unsupported("The WAV format chunk is too short.");
      let tag = view.getUint16(body, true);
      if (tag === WAVE_FORMAT_EXTENSIBLE) {
        if (size < 40) unsupported("The WAV format chunk is too short.");
        // The sub-format GUID starts with the plain format tag.
        tag = view.getUint16(body + 24, true);
      }
      const channels = view.getUint16(body + 2, true);
      const sampleRate = view.getUint32(body + 4, true);
      const blockAlign = view.getUint16(body + 12, true);
      const bits = view.getUint16(body + 14, true);
      if (tag !== WAVE_FORMAT_PCM && tag !== WAVE_FORMAT_IEEE_FLOAT) {
        unsupported("Only uncompressed PCM or float WAV audio is supported.");
      }
      const validBits =
        tag === WAVE_FORMAT_PCM
          ? [8, 16, 24, 32].includes(bits)
          : bits === 32 || bits === 64;
      if (!validBits) unsupported(`${bits}-bit WAV audio is not supported.`);
      if (channels < 1 || channels > MAX_CHANNELS) {
        unsupported(`WAV audio must have 1 to ${MAX_CHANNELS} channels.`);
      }
      if (sampleRate < MIN_INPUT_RATE_HZ || sampleRate > MAX_INPUT_RATE_HZ) {
        unsupported(
          `WAV sample rate must be between ${MIN_INPUT_RATE_HZ} and ${MAX_INPUT_RATE_HZ} Hz.`,
        );
      }
      if (blockAlign !== channels * (bits / 8)) {
        unsupported("The WAV block alignment does not match its format.");
      }
      format = { tag, channels, sampleRate, bits };
    } else if (id === "data") {
      if (data) unsupported("The WAV file has more than one data chunk.");
      data = { offset: body, length: size };
    }
    // Chunks are word aligned; a final odd chunk may omit its pad byte.
    offset = body + size + (size % 2);
  }
  if (!format) unsupported("The WAV file has no format chunk.");
  if (!data) unsupported("The WAV file has no audio data.");

  const { tag, channels, sampleRate, bits } = format;
  const bytesPerSample = bits / 8;
  const blockBytes = channels * bytesPerSample;
  const frames = Math.floor(data.length / blockBytes);
  const start = data.offset;
  const read = (frame: number, channel: number): number => {
    const at = start + frame * blockBytes + channel * bytesPerSample;
    if (tag === WAVE_FORMAT_IEEE_FLOAT) {
      const value =
        bits === 32 ? view.getFloat32(at, true) : view.getFloat64(at, true);
      if (!Number.isFinite(value)) {
        unsupported("The WAV file contains invalid float samples.");
      }
      return Math.max(-1, Math.min(1, value));
    }
    switch (bits) {
      case 8:
        return (view.getUint8(at) - 128) / 128;
      case 16:
        return view.getInt16(at, true) / 32_768;
      case 24: {
        const value =
          view.getUint8(at) |
          (view.getUint8(at + 1) << 8) |
          (view.getInt8(at + 2) << 16);
        return value / 8_388_608;
      }
      default:
        return view.getInt32(at, true) / 2_147_483_648;
    }
  };
  return { sampleRate, channels, frames, read };
}

function greatestCommonDivisor(a: number, b: number): number {
  while (b !== 0) [a, b] = [b, a % b];
  return a;
}

/**
 * Polyphase windowed-sinc resampling. Rates are integers, so the output grid
 * repeats every `upsample` samples and each phase has one fixed kernel.
 */
export function resampleMono(
  input: Float32Array,
  inputRate: number,
  outputRate: number,
): Float32Array {
  if (inputRate === outputRate) return input.slice();
  const divisor = greatestCommonDivisor(inputRate, outputRate);
  const upsample = outputRate / divisor;
  const downsample = inputRate / divisor;
  const cutoff = Math.min(1, outputRate / inputRate);
  const halfWidth = Math.ceil(RESAMPLE_ZERO_CROSSINGS / cutoff);
  const taps = 2 * halfWidth;
  const kernels = new Float32Array(upsample * taps);
  for (let phase = 0; phase < upsample; phase++) {
    const fraction = phase / upsample;
    let sum = 0;
    for (let tap = 0; tap < taps; tap++) {
      const distance = tap - halfWidth + 1 - fraction;
      const x = cutoff * distance;
      const sinc = x === 0 ? 1 : Math.sin(Math.PI * x) / (Math.PI * x);
      // Blackman window over the kernel span.
      const position = (distance + halfWidth) / (2 * halfWidth);
      const window =
        0.42 -
        0.5 * Math.cos(2 * Math.PI * position) +
        0.08 * Math.cos(4 * Math.PI * position);
      const value = sinc * Math.max(0, window);
      kernels[phase * taps + tap] = value;
      sum += value;
    }
    // Unity DC gain for every phase.
    for (let tap = 0; tap < taps; tap++) kernels[phase * taps + tap] /= sum;
  }
  const length = Math.floor((input.length * upsample) / downsample);
  const output = new Float32Array(length);
  for (let index = 0; index < length; index++) {
    const position = index * downsample;
    const base = Math.floor(position / upsample);
    const phase = position % upsample;
    const kernel = phase * taps;
    let value = 0;
    for (let tap = 0; tap < taps; tap++) {
      const sample = base + tap - halfWidth + 1;
      if (sample >= 0 && sample < input.length) {
        value += input[sample]! * kernels[kernel + tap]!;
      }
    }
    output[index] = value;
  }
  return output;
}

export function encodePcm16Wav(
  samples: Float32Array,
  sampleRate: number,
): Uint8Array {
  const bytes = new Uint8Array(44 + samples.length * 2);
  const view = new DataView(bytes.buffer);
  const ascii = (offset: number, text: string) => {
    for (let index = 0; index < text.length; index++) {
      view.setUint8(offset + index, text.charCodeAt(index));
    }
  };
  ascii(0, "RIFF");
  view.setUint32(4, bytes.byteLength - 8, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, WAVE_FORMAT_PCM, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, "data");
  view.setUint32(40, samples.length * 2, true);
  for (let index = 0; index < samples.length; index++) {
    const value = Math.max(-1, Math.min(1, samples[index]!));
    view.setInt16(
      44 + index * 2,
      Math.round(value < 0 ? value * 32_768 : value * 32_767),
      true,
    );
  }
  return bytes;
}

/**
 * Validates an uploaded reference clip and rewrites it as 24 kHz mono PCM16.
 * Rewriting also drops every metadata chunk the upload carried.
 */
export function normalizeVoiceAudio(bytes: Uint8Array): NormalizedVoiceAudio {
  const decoded = decodeWav(bytes);
  const seconds = decoded.frames / decoded.sampleRate;
  if (seconds < VOICE_MIN_SECONDS) {
    throw new VoiceError(
      "audio_too_short",
      `The recording is ${seconds.toFixed(1)} s; use at least ${VOICE_MIN_SECONDS} s of speech.`,
    );
  }
  if (seconds > VOICE_MAX_SECONDS) {
    throw new VoiceError(
      "audio_too_long",
      `The recording is ${seconds.toFixed(1)} s; keep it to ${VOICE_MAX_SECONDS} s or less.`,
    );
  }
  const mono = new Float32Array(decoded.frames);
  for (let frame = 0; frame < decoded.frames; frame++) {
    let sum = 0;
    for (let channel = 0; channel < decoded.channels; channel++) {
      sum += decoded.read(frame, channel);
    }
    mono[frame] = sum / decoded.channels;
  }
  const resampled = resampleMono(
    mono,
    decoded.sampleRate,
    VOICE_SAMPLE_RATE_HZ,
  );
  let peak = 0;
  for (const sample of resampled) peak = Math.max(peak, Math.abs(sample));
  if (peak < SILENCE_PEAK) {
    throw new VoiceError(
      "audio_silent",
      "The recording is silent. Check the microphone and record again.",
    );
  }
  return Object.freeze({
    wav: encodePcm16Wav(resampled, VOICE_SAMPLE_RATE_HZ),
    sampleCount: resampled.length,
    durationMs: Math.round((resampled.length / VOICE_SAMPLE_RATE_HZ) * 1000),
  });
}
