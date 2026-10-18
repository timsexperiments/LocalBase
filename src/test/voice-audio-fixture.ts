/** Test WAV builder for voice upload tests. */
export type WavOptions = {
  sampleRate?: number;
  channels?: number;
  bits?: number;
  format?: number;
  extensible?: boolean;
  seconds?: number;
  amplitude?: number;
  extraChunks?: Array<{ id: string; body: Uint8Array }>;
};

/** Builds a WAV with a 220 Hz tone in every channel. */
export function toneWav({
  sampleRate = 48_000,
  channels = 2,
  bits = 16,
  format = 1,
  extensible = false,
  seconds = 4,
  amplitude = 0.5,
  extraChunks = [],
}: WavOptions = {}): Uint8Array {
  const frames = Math.round(sampleRate * seconds);
  const bytesPerSample = bits / 8;
  const data = new Uint8Array(frames * channels * bytesPerSample);
  const view = new DataView(data.buffer);
  for (let frame = 0; frame < frames; frame++) {
    const value =
      amplitude * Math.sin((2 * Math.PI * 220 * frame) / sampleRate);
    for (let channel = 0; channel < channels; channel++) {
      const at = (frame * channels + channel) * bytesPerSample;
      if (format === 3) {
        if (bits === 32) view.setFloat32(at, value, true);
        else view.setFloat64(at, value, true);
      } else if (bits === 8) view.setUint8(at, Math.round(value * 127) + 128);
      else if (bits === 16) view.setInt16(at, Math.round(value * 32_767), true);
      else if (bits === 24) {
        const sample = Math.round(value * 8_388_607);
        view.setUint8(at, sample & 0xff);
        view.setUint8(at + 1, (sample >> 8) & 0xff);
        view.setInt8(at + 2, sample >> 16);
      } else view.setInt32(at, Math.round(value * 2_147_483_647), true);
    }
  }
  const fmt = new Uint8Array(extensible ? 40 : 16);
  const fmtView = new DataView(fmt.buffer);
  fmtView.setUint16(0, extensible ? 0xfffe : format, true);
  fmtView.setUint16(2, channels, true);
  fmtView.setUint32(4, sampleRate, true);
  fmtView.setUint32(8, sampleRate * channels * bytesPerSample, true);
  fmtView.setUint16(12, channels * bytesPerSample, true);
  fmtView.setUint16(14, bits, true);
  if (extensible) {
    fmtView.setUint16(16, 22, true);
    fmtView.setUint16(18, bits, true);
    fmtView.setUint16(24, format, true);
  }
  const chunks = [
    { id: "fmt ", body: fmt },
    ...extraChunks,
    { id: "data", body: data },
  ];
  const length =
    12 +
    chunks.reduce(
      (sum, { body }) => sum + 8 + body.byteLength + (body.byteLength % 2),
      0,
    );
  const bytes = new Uint8Array(length);
  const out = new DataView(bytes.buffer);
  const ascii = (offset: number, text: string) => {
    for (let index = 0; index < 4; index++) {
      out.setUint8(offset + index, text.charCodeAt(index));
    }
  };
  ascii(0, "RIFF");
  out.setUint32(4, length - 8, true);
  ascii(8, "WAVE");
  let offset = 12;
  for (const { id, body } of chunks) {
    ascii(offset, id);
    out.setUint32(offset + 4, body.byteLength, true);
    bytes.set(body, offset + 8);
    offset += 8 + body.byteLength + (body.byteLength % 2);
  }
  return bytes;
}
