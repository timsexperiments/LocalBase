import { inflateSync } from "node:zlib";

export type ImageDimensions = Readonly<{ width: number; height: number }>;

const PNG_SIGNATURE = Uint8Array.from([137, 80, 78, 71, 13, 10, 26, 10]);

/** Validates PNG structure and bounds decompression to the exact scanline size. */
export function validatePngImage(
  bytes: Uint8Array,
): ImageDimensions | undefined {
  if (
    bytes.byteLength < 8 ||
    !PNG_SIGNATURE.every((byte, index) => bytes[index] === byte)
  )
    return undefined;

  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let offset = 8;
  let dimensions: ImageDimensions | undefined;
  let bitDepth = 0;
  let colorType = 0;
  let interlace = 0;
  let sawIdat = false;
  let sawPalette = false;
  let paletteEntries = 0;
  let idatEnded = false;
  let sawIend = false;
  const chunks: Uint8Array[] = [];
  let compressedLength = 0;
  while (offset + 12 <= bytes.byteLength) {
    const length = view.getUint32(offset);
    const dataStart = offset + 8;
    const next = dataStart + length + 4;
    if (!Number.isSafeInteger(next) || next > bytes.byteLength)
      return undefined;
    const type = fourCc(bytes, offset + 4);
    if (
      type !== "IHDR" &&
      type !== "PLTE" &&
      type !== "IDAT" &&
      type !== "IEND" &&
      (bytes[offset + 4]! & 0x20) === 0
    )
      return undefined;
    if (!dimensions) {
      if (type !== "IHDR" || offset !== 8 || length !== 13) return undefined;
      const width = view.getUint32(dataStart);
      const height = view.getUint32(dataStart + 4);
      bitDepth = bytes[dataStart + 8] ?? 0;
      colorType = bytes[dataStart + 9] ?? 0;
      if (
        width === 0 ||
        height === 0 ||
        width > 2048 ||
        height > 2048 ||
        width * height > 4_194_304 ||
        !validPngDepth(colorType, bitDepth) ||
        bytes[dataStart + 10] !== 0 ||
        bytes[dataStart + 11] !== 0 ||
        (bytes[dataStart + 12] !== 0 && bytes[dataStart + 12] !== 1)
      )
        return undefined;
      interlace = bytes[dataStart + 12] ?? 0;
      dimensions = Object.freeze({ width, height });
    } else if (type === "IHDR") {
      return undefined;
    }

    if (type === "PLTE") {
      if (
        sawIdat ||
        sawPalette ||
        length === 0 ||
        length > 768 ||
        length % 3 !== 0 ||
        colorType === 0 ||
        colorType === 4
      )
        return undefined;
      if (colorType === 3 && length / 3 > 2 ** bitDepth) return undefined;
      sawPalette = true;
      paletteEntries = length / 3;
    }

    // The backend's stb_image decoder rejects these tRNS layouts.
    if (type === "tRNS") {
      if (sawIdat) return undefined;
      if (colorType === 3) {
        if (!sawPalette || length > paletteEntries) return undefined;
      } else if (
        colorType === 4 ||
        colorType === 6 ||
        length !== channelCount(colorType) * 2
      ) {
        return undefined;
      }
    }

    if (type === "IDAT") {
      if (idatEnded || sawIend || (colorType === 3 && !sawPalette))
        return undefined;
      sawIdat = true;
      compressedLength += length;
      if (
        !Number.isSafeInteger(compressedLength) ||
        compressedLength > bytes.byteLength
      )
        return undefined;
      chunks.push(bytes.subarray(dataStart, dataStart + length));
    } else {
      if (sawIdat && type !== "IEND") idatEnded = true;
      if (type === "IEND") {
        if (length !== 0 || next !== bytes.byteLength || sawIend)
          return undefined;
        sawIend = true;
        offset = next;
        break;
      }
    }
    offset = next;
  }
  if (!dimensions || !sawIdat || !sawIend || offset !== bytes.byteLength)
    return undefined;

  const channels = channelCount(colorType);
  const expectedLength = pngRawLength(
    dimensions.width,
    dimensions.height,
    channels,
    bitDepth,
    interlace,
  );
  if (!expectedLength || expectedLength >= 0x7fffffff) return undefined;
  const compressed = new Uint8Array(compressedLength);
  let compressedOffset = 0;
  for (const chunk of chunks) {
    compressed.set(chunk, compressedOffset);
    compressedOffset += chunk.byteLength;
  }
  try {
    const raw = inflateSync(compressed, {
      maxOutputLength: expectedLength + 1,
    });
    if (raw.byteLength !== expectedLength) return undefined;
    return validPngFilters(
      raw,
      dimensions.width,
      dimensions.height,
      channels,
      bitDepth,
      interlace,
    )
      ? dimensions
      : undefined;
  } catch {
    return undefined;
  }
}

/** Checks JPEG segment bounds, SOF/SOS presence, entropy markers, and EOI. */
export function validateJpegImage(
  bytes: Uint8Array,
): ImageDimensions | undefined {
  if (bytes.length < 4 || bytes[0] !== 0xff || bytes[1] !== 0xd8)
    return undefined;
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let offset = 2;
  let dimensions: ImageDimensions | undefined;
  let sawSos = false;
  while (offset < bytes.length) {
    if (sawSos) {
      while (offset < bytes.length && bytes[offset] !== 0xff) offset++;
      if (offset >= bytes.length) return undefined;
    }
    if (bytes[offset] !== 0xff) return undefined;
    while (offset < bytes.length && bytes[offset] === 0xff) offset++;
    if (offset >= bytes.length) return undefined;
    const marker = bytes[offset++]!;
    if (marker === 0x00) {
      if (!sawSos) return undefined;
      continue;
    }
    if (marker === 0xd9)
      return sawSos && dimensions && offset === bytes.length
        ? dimensions
        : undefined;
    if (marker >= 0xd0 && marker <= 0xd7) {
      if (!sawSos) return undefined;
      continue;
    }
    if (marker === 0xd8) return undefined;
    if (marker === 0x01) continue;
    if (offset + 2 > bytes.length) return undefined;
    const length = view.getUint16(offset);
    if (length < 2 || offset + length > bytes.length) return undefined;
    const sof =
      marker >= 0xc0 && marker <= 0xcf && ![0xc4, 0xc8, 0xcc].includes(marker);
    if (sof) {
      if (
        ![0xc0, 0xc1, 0xc2].includes(marker) ||
        bytes[offset + 2] !== 8 ||
        length < 8 ||
        dimensions ||
        length !== 8 + 3 * bytes[offset + 7]!
      )
        return undefined;
      dimensions = Object.freeze({
        height: view.getUint16(offset + 3),
        width: view.getUint16(offset + 5),
      });
      if (!dimensions.width || !dimensions.height) return undefined;
    }
    if (marker === 0xcc) return undefined;
    if (marker === 0xda) {
      if (!dimensions) return undefined;
      const scanComponents = bytes[offset + 2] ?? 0;
      if (scanComponents < 1 || length !== 6 + 2 * scanComponents)
        return undefined;
      sawSos = true;
    }
    offset += length;
  }
  return undefined;
}

function validPngFilters(
  raw: Uint8Array,
  width: number,
  height: number,
  channels: number,
  depth: number,
  interlace: number,
): boolean {
  const bitsPerPixel = channels * depth;
  const passes =
    interlace === 0
      ? [[0, 0, 1, 1]]
      : [
          [0, 0, 8, 8],
          [4, 0, 8, 8],
          [0, 4, 4, 8],
          [2, 0, 4, 4],
          [0, 2, 2, 4],
          [1, 0, 2, 2],
          [0, 1, 1, 2],
        ];
  let offset = 0;
  for (const [x, y, dx, dy] of passes) {
    const passWidth = width <= x! ? 0 : Math.ceil((width - x!) / dx!);
    const passHeight = height <= y! ? 0 : Math.ceil((height - y!) / dy!);
    if (passWidth === 0 || passHeight === 0) continue;
    const rowLength = Math.ceil((passWidth * bitsPerPixel) / 8);
    for (let row = 0; row < passHeight; row++) {
      if (raw[offset]! > 4) return false;
      offset += rowLength + 1;
    }
  }
  return offset === raw.byteLength;
}

function validPngDepth(color: number, depth: number): boolean {
  return color === 0
    ? [1, 2, 4, 8, 16].includes(depth)
    : color === 2
      ? [8, 16].includes(depth)
      : color === 3
        ? [1, 2, 4, 8].includes(depth)
        : color === 4 || color === 6
          ? [8, 16].includes(depth)
          : false;
}

function channelCount(color: number): number {
  return color === 0 || color === 3 ? 1 : color === 2 ? 3 : color === 4 ? 2 : 4;
}

function pngRawLength(
  width: number,
  height: number,
  channels: number,
  depth: number,
  interlace: number,
): number | undefined {
  const bitsPerPixel = channels * depth;
  if (interlace === 0) return safePassLength(width, height, bitsPerPixel);
  const starts = [
    [0, 0],
    [4, 0],
    [0, 4],
    [2, 0],
    [0, 2],
    [1, 0],
    [0, 1],
  ] as const;
  const steps = [
    [8, 8],
    [8, 8],
    [4, 8],
    [4, 4],
    [2, 4],
    [2, 2],
    [1, 2],
  ] as const;
  let total = 0;
  for (let pass = 0; pass < 7; pass++) {
    const [x, y] = starts[pass]!;
    const [dx, dy] = steps[pass]!;
    const passWidth = width <= x ? 0 : Math.ceil((width - x) / dx);
    const passHeight = height <= y ? 0 : Math.ceil((height - y) / dy);
    if (passWidth && passHeight)
      total +=
        safePassLength(passWidth, passHeight, bitsPerPixel) ??
        Number.POSITIVE_INFINITY;
  }
  return Number.isSafeInteger(total) ? total : undefined;
}

function safePassLength(
  width: number,
  height: number,
  bitsPerPixel: number,
): number | undefined {
  const row = Math.ceil((width * bitsPerPixel) / 8);
  const length = (row + 1) * height;
  return Number.isSafeInteger(length) ? length : undefined;
}

function fourCc(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(
    bytes[offset] ?? 0,
    bytes[offset + 1] ?? 0,
    bytes[offset + 2] ?? 0,
    bytes[offset + 3] ?? 0,
  );
}
