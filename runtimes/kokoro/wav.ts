export const MAX_SAMPLES = 256 * 1920;

/** The speech API accepts bounded PCM16 mono at 24 kHz. */
export function pcm16Wav(samples: Float32Array): Uint8Array {
  if (samples.length < 1 || samples.length > MAX_SAMPLES) {
    throw new Error("Speech waveform is empty or exceeds the sample limit.");
  }
  const bytes = new Uint8Array(44 + samples.length * 2);
  const view = new DataView(bytes.buffer);
  const text = (offset: number, value: string) => {
    bytes.set(new TextEncoder().encode(value), offset);
  };
  text(0, "RIFF");
  view.setUint32(4, bytes.length - 8, true);
  text(8, "WAVEfmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, 24000, true);
  view.setUint32(28, 48000, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  text(36, "data");
  view.setUint32(40, samples.length * 2, true);
  for (let index = 0; index < samples.length; index++) {
    const sample = samples[index];
    if (!Number.isFinite(sample) || Math.abs(sample) > 1) {
      throw new Error("Speech waveform contains invalid or clipped samples.");
    }
    view.setInt16(
      44 + index * 2,
      Math.round(sample * (sample < 0 ? 32768 : 32767)),
      true,
    );
  }
  return bytes;
}
