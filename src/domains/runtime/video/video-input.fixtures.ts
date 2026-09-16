import { zlibSync } from "fflate";

function uint32(value: number): Uint8Array {
  const bytes = new Uint8Array(4);
  new DataView(bytes.buffer).setUint32(0, value);
  return bytes;
}

function crc32(data: Uint8Array): number {
  let crc = 0xffffffff;
  for (const byte of data) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
    }
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Uint8Array): Uint8Array {
  const typeBytes = new TextEncoder().encode(type);
  const crcInput = new Uint8Array(typeBytes.byteLength + data.byteLength);
  crcInput.set(typeBytes);
  crcInput.set(data, typeBytes.byteLength);
  const chunk = new Uint8Array(12 + data.byteLength);
  chunk.set(uint32(data.byteLength));
  chunk.set(crcInput, 4);
  chunk.set(uint32(crc32(crcInput)), 8 + data.byteLength);
  return chunk;
}

export function testPng(options: {
  width: number;
  height: number;
  colorType?: 2 | 6;
  bitDepth?: number;
  interlace?: number;
  inflated?: Uint8Array;
}): string {
  const colorType = options.colorType ?? 2;
  const channels = colorType === 2 ? 3 : 4;
  const header = new Uint8Array(13);
  const view = new DataView(header.buffer);
  view.setUint32(0, options.width);
  view.setUint32(4, options.height);
  header[8] = options.bitDepth ?? 8;
  header[9] = colorType;
  header[12] = options.interlace ?? 0;
  const inflated =
    options.inflated ??
    new Uint8Array((options.width * channels + 1) * options.height);
  const chunks = [
    Uint8Array.from([137, 80, 78, 71, 13, 10, 26, 10]),
    pngChunk("IHDR", header),
    pngChunk("IDAT", zlibSync(inflated)),
    pngChunk("IEND", new Uint8Array()),
  ];
  const bytes = new Uint8Array(
    chunks.reduce((total, chunk) => total + chunk.byteLength, 0),
  );
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes.toBase64();
}

export function testPcm16Wav(options: {
  sampleRate: number;
  frames: number;
}): string {
  const dataBytes = options.frames * 2;
  const bytes = new Uint8Array(44 + dataBytes);
  const view = new DataView(bytes.buffer);
  const text = (offset: number, value: string) => {
    bytes.set(new TextEncoder().encode(value), offset);
  };
  text(0, "RIFF");
  view.setUint32(4, bytes.byteLength - 8, true);
  text(8, "WAVE");
  text(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, options.sampleRate, true);
  view.setUint32(28, options.sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  text(36, "data");
  view.setUint32(40, dataBytes, true);
  return bytes.toBase64();
}
