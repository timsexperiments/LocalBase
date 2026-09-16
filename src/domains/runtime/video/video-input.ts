import { z } from "zod";
import { unzlibSync } from "fflate";

export const MAX_VIDEO_WAV_BYTES = 8 * 1024 * 1024;
export const MAX_VIDEO_PNG_BYTES = 4 * 1024 * 1024;

const MAX_BASE64_WAV_LENGTH = Math.ceil(MAX_VIDEO_WAV_BYTES / 3) * 4;
const MAX_BASE64_PNG_LENGTH = Math.ceil(MAX_VIDEO_PNG_BYTES / 3) * 4;
const PNG_SIGNATURE = Uint8Array.from([137, 80, 78, 71, 13, 10, 26, 10]);

const generationSchema = z
  .object({
    sampler: z.literal("euler"),
    scheduler: z.enum(["discrete", "lcm"]),
    steps: z.number().int().positive().max(1_000),
    cfgScale: z.number().positive().max(100),
    flowShift: z.number().positive().max(100),
  })
  .strict();

const commonInputShape = {
  prompt: z.string().min(1).max(16_384),
  negativePrompt: z.string().max(16_384).optional(),
  width: z.number().int().positive().max(16_384).optional(),
  height: z.number().int().positive().max(16_384).optional(),
  videoFrames: z.number().int().positive().max(4_097).optional(),
  fps: z.number().int().positive().max(240).optional(),
  seed: z.number().int().optional(),
  outputFormat: z.enum(["webm", "webp", "avi"]).optional(),
  generation: generationSchema.optional(),
};

const inlinePngSchema = z
  .object({
    format: z.literal("png"),
    data: z.string().min(1).max(MAX_BASE64_PNG_LENGTH),
  })
  .strict();

const inlineWavSchema = z
  .object({
    format: z.literal("wav"),
    data: z.string().min(1).max(MAX_BASE64_WAV_LENGTH),
  })
  .strict();

export const videoConditioningInputSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("text") }).strict(),
  z
    .object({
      kind: z.literal("speech"),
      portrait: inlinePngSchema,
      audio: inlineWavSchema,
    })
    .strict(),
]);

const textVideoInputSchema = z
  .object({ kind: z.literal("text"), ...commonInputShape })
  .strict();

const speechVideoInputSchema = z
  .object({
    kind: z.literal("speech"),
    ...commonInputShape,
    width: commonInputShape.width.unwrap(),
    height: commonInputShape.height.unwrap(),
    videoFrames: commonInputShape.videoFrames.unwrap(),
    fps: commonInputShape.fps.unwrap(),
    portrait: inlinePngSchema,
    audio: inlineWavSchema,
  })
  .strict()
  .superRefine((input, ctx) => {
    validateSpeechMedia(input, (message, path) =>
      ctx.addIssue({ code: "custom", message, path }),
    );
  });

/**
 * Native video input after catalog qualification. Speech conditioning accepts
 * only canonical inline PCM16/float32 WAV and non-interlaced 8-bit RGB/RGBA PNG.
 */
export const videoGenerationInputSchema = z.discriminatedUnion("kind", [
  textVideoInputSchema,
  speechVideoInputSchema,
]);

export type VideoGenerationInput = z.infer<typeof videoGenerationInputSchema>;
export type SpeechVideoInput = z.infer<typeof speechVideoInputSchema>;

function validateSpeechMedia(
  input: z.infer<typeof speechVideoInputSchema>,
  issue: (message: string, path: PropertyKey[]) => void,
): void {
  const png = decodeCanonicalBase64(input.portrait.data);
  if (!png || png.byteLength > MAX_VIDEO_PNG_BYTES) {
    issue("Portrait must be canonical base64 within the 4 MiB limit.", [
      "portrait",
      "data",
    ]);
  } else {
    const dimensions = validatePng(png);
    if (
      !dimensions ||
      dimensions.width !== input.width ||
      dimensions.height !== input.height
    ) {
      issue("Portrait PNG dimensions must match the qualified video canvas.", [
        "portrait",
      ]);
    }
  }

  const wav = decodeCanonicalBase64(input.audio.data);
  if (!wav || wav.byteLength > MAX_VIDEO_WAV_BYTES) {
    issue("Audio must be canonical base64 within the 8 MiB limit.", [
      "audio",
      "data",
    ]);
    return;
  }
  const metadata = parseWav(wav);
  if (!metadata) {
    issue("Audio must be a supported PCM16 or normalized float32 WAV.", [
      "audio",
    ]);
    return;
  }
  if (metadata.frames * input.fps > metadata.sampleRate * input.videoFrames) {
    issue("Audio duration must not exceed the qualified video duration.", [
      "audio",
    ]);
  }
}

function decodeCanonicalBase64(value: string): Uint8Array | undefined {
  if (
    value.length === 0 ||
    value.length % 4 !== 0 ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
      value,
    )
  ) {
    return undefined;
  }
  try {
    const bytes = Uint8Array.fromBase64(value);
    return bytes.toBase64() === value ? bytes : undefined;
  } catch {
    return undefined;
  }
}

function validatePng(
  bytes: Uint8Array,
): Readonly<{ width: number; height: number }> | undefined {
  if (bytes.byteLength < 33) return undefined;
  if (!PNG_SIGNATURE.every((byte, index) => bytes[index] === byte)) {
    return undefined;
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (view.getUint32(8) !== 13 || fourCc(bytes, 12) !== "IHDR") {
    return undefined;
  }
  const width = view.getUint32(16);
  const height = view.getUint32(20);
  const bitDepth = bytes[24];
  const colorType = bytes[25];
  if (
    width === 0 ||
    height === 0 ||
    bitDepth !== 8 ||
    (colorType !== 2 && colorType !== 6) ||
    bytes[26] !== 0 ||
    bytes[27] !== 0 ||
    bytes[28] !== 0
  ) {
    return undefined;
  }
  const channels = colorType === 2 ? 3 : 4;
  const rowBytes = width * channels;
  const pixelBytes = rowBytes * height;
  const inflatedBytes = (rowBytes + 1) * height;
  if (
    !Number.isSafeInteger(pixelBytes) ||
    !Number.isSafeInteger(inflatedBytes) ||
    pixelBytes > MAX_VIDEO_PNG_BYTES
  ) {
    return undefined;
  }

  let offset = 8;
  let sawEnd = false;
  let sawHeader = false;
  let imageDataEnded = false;
  const imageData: Uint8Array[] = [];
  let imageDataBytes = 0;
  while (offset + 12 <= bytes.byteLength) {
    const length = view.getUint32(offset);
    const next = offset + 12 + length;
    if (!Number.isSafeInteger(next) || next > bytes.byteLength)
      return undefined;
    const type = fourCc(bytes, offset + 4);
    if (
      !validPngCrc(
        bytes.subarray(offset + 4, offset + 8 + length),
        view.getUint32(offset + 8 + length),
      )
    ) {
      return undefined;
    }
    if (type === "IHDR") {
      if (sawHeader || offset !== 8 || length !== 13) return undefined;
      sawHeader = true;
    } else if (type === "IDAT") {
      if (!sawHeader || sawEnd || imageDataEnded) return undefined;
      imageDataBytes += length;
      if (imageDataBytes > MAX_VIDEO_PNG_BYTES) return undefined;
      imageData.push(bytes.subarray(offset + 8, offset + 8 + length));
    } else if (type === "IEND") {
      if (length !== 0 || next !== bytes.byteLength) return undefined;
      sawEnd = true;
      break;
    } else {
      if (imageData.length > 0) imageDataEnded = true;
      // Reject unknown critical chunks. Ancillary metadata is allowed only
      // before the contiguous image-data run in this deliberately narrow form.
      if ((bytes[offset + 4] ?? 0) >= 65 && (bytes[offset + 4] ?? 0) <= 90) {
        return undefined;
      }
    }
    offset = next;
  }
  if (!sawEnd || !sawHeader || imageData.length === 0) return undefined;

  const compressed = new Uint8Array(imageDataBytes);
  let compressedOffset = 0;
  for (const chunk of imageData) {
    compressed.set(chunk, compressedOffset);
    compressedOffset += chunk.byteLength;
  }
  if (!validateInflatedScanlines(compressed, inflatedBytes, rowBytes + 1)) {
    return undefined;
  }
  return Object.freeze({ width, height });
}

function validateInflatedScanlines(
  compressed: Uint8Array,
  expectedBytes: number,
  rowStride: number,
): boolean {
  try {
    // fflate uses this caller-owned output directly and disables its internal
    // growth path for synchronous inflation. The extra sentinel byte makes a
    // truncated overrun distinguishable from exact output while bounding the
    // only pixel buffer allocation before decompression begins.
    const output = unzlibSync(compressed, {
      out: new Uint8Array(expectedBytes + 1),
    });
    if (output.byteLength !== expectedBytes) return false;
    for (let offset = 0; offset < output.byteLength; offset += rowStride) {
      if ((output[offset] ?? 5) > 4) return false;
    }
    return true;
  } catch {
    return false;
  }
}

function validPngCrc(data: Uint8Array, expected: number): boolean {
  let crc = 0xffffffff;
  for (const byte of data) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
    }
  }
  return (crc ^ 0xffffffff) >>> 0 === expected;
}

function parseWav(
  bytes: Uint8Array,
): Readonly<{ sampleRate: number; frames: number }> | undefined {
  if (
    bytes.byteLength < 44 ||
    fourCc(bytes, 0) !== "RIFF" ||
    fourCc(bytes, 8) !== "WAVE"
  ) {
    return undefined;
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (view.getUint32(4, true) + 8 !== bytes.byteLength) return undefined;

  let format:
    | Readonly<{
        encoding: number;
        channels: number;
        sampleRate: number;
        byteRate: number;
        blockAlign: number;
        bitsPerSample: number;
      }>
    | undefined;
  let sampleData: Uint8Array | undefined;
  let offset = 12;
  while (offset + 8 <= bytes.byteLength) {
    const id = fourCc(bytes, offset);
    const size = view.getUint32(offset + 4, true);
    const start = offset + 8;
    const end = start + size;
    if (!Number.isSafeInteger(end) || end > bytes.byteLength) return undefined;
    if (id === "fmt " && !format) {
      if (size < 16) return undefined;
      format = Object.freeze({
        encoding: view.getUint16(start, true),
        channels: view.getUint16(start + 2, true),
        sampleRate: view.getUint32(start + 4, true),
        byteRate: view.getUint32(start + 8, true),
        blockAlign: view.getUint16(start + 12, true),
        bitsPerSample: view.getUint16(start + 14, true),
      });
    } else if (id === "data" && !sampleData) {
      sampleData = bytes.subarray(start, end);
    }
    offset = end + (size % 2);
  }
  if (offset !== bytes.byteLength || !format || !sampleData) return undefined;
  const bytesPerSample = format.bitsPerSample / 8;
  if (
    (format.encoding !== 1 && format.encoding !== 3) ||
    (format.encoding === 1 && format.bitsPerSample !== 16) ||
    (format.encoding === 3 && format.bitsPerSample !== 32) ||
    (format.channels !== 1 && format.channels !== 2) ||
    format.sampleRate < 8_000 ||
    format.sampleRate > 192_000 ||
    format.blockAlign !== format.channels * bytesPerSample ||
    format.byteRate !== format.sampleRate * format.blockAlign ||
    sampleData.byteLength === 0 ||
    sampleData.byteLength % format.blockAlign !== 0
  ) {
    return undefined;
  }
  if (format.encoding === 3) {
    const samples = new DataView(
      sampleData.buffer,
      sampleData.byteOffset,
      sampleData.byteLength,
    );
    for (let offset = 0; offset < sampleData.byteLength; offset += 4) {
      const sample = samples.getFloat32(offset, true);
      if (!Number.isFinite(sample) || sample < -1 || sample > 1) {
        return undefined;
      }
    }
  }
  return Object.freeze({
    sampleRate: format.sampleRate,
    frames: sampleData.byteLength / format.blockAlign,
  });
}

function fourCc(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(
    bytes[offset] ?? 0,
    bytes[offset + 1] ?? 0,
    bytes[offset + 2] ?? 0,
    bytes[offset + 3] ?? 0,
  );
}
