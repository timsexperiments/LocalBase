import { expect, test } from "bun:test";
import { deflateSync } from "node:zlib";
import { validateJpegImage, validatePngImage } from "./image-validation";
import { testPng } from "./video/video-input.fixtures";

function chunks(bytes: Uint8Array) {
  const result: { type: string; data: Uint8Array }[] = [];
  for (let offset = 8; offset < bytes.length;) {
    const length = new DataView(
      bytes.buffer,
      bytes.byteOffset,
      bytes.byteLength,
    ).getUint32(offset);
    result.push({
      type: new TextDecoder().decode(bytes.subarray(offset + 4, offset + 8)),
      data: bytes.subarray(offset + 8, offset + 8 + length),
    });
    offset += length + 12;
  }
  return result;
}

function encodePng(parts: { type: string; data: Uint8Array }[]): Uint8Array {
  const encoded = parts.map(({ type, data }) => {
    const chunk = new Uint8Array(data.length + 12);
    new DataView(chunk.buffer).setUint32(0, data.length);
    chunk.set(new TextEncoder().encode(type), 4);
    chunk.set(data, 8);
    return chunk;
  });
  const signature = Uint8Array.from([137, 80, 78, 71, 13, 10, 26, 10]);
  const result = new Uint8Array(
    signature.length + encoded.reduce((n, c) => n + c.length, 0),
  );
  result.set(signature);
  let offset = signature.length;
  for (const chunk of encoded) {
    result.set(chunk, offset);
    offset += chunk.length;
  }
  return result;
}

function withInflated(png: Uint8Array, raw: Uint8Array) {
  return encodePng(
    chunks(png).map((part) =>
      part.type === "IDAT"
        ? { ...part, data: new Uint8Array(deflateSync(raw)) }
        : part,
    ),
  );
}

function jpeg(frameMarker: number, precision = 8) {
  const frame = Uint8Array.from([
    0xff,
    frameMarker,
    0,
    11,
    precision,
    0,
    1,
    0,
    1,
    1,
    1,
    0x11,
    0,
  ]);
  const scan = Uint8Array.from([
    0xff, 0xda, 0, 8, 1, 1, 0, 0, 63, 0, 0xff, 0xd9,
  ]);
  return Uint8Array.from([0xff, 0xd8, ...frame, ...scan]);
}

test("stops PNG expansion at the declared scanline limit", () => {
  const base = Uint8Array.fromBase64(testPng({ width: 1, height: 1 }));
  const bomb = withInflated(base, new Uint8Array(64 * 1024 * 1024));
  const started = performance.now();
  expect(validatePngImage(bomb)).toBeUndefined();
  expect(performance.now() - started).toBeLessThan(2_000);
});

test("rejects invalid PNG scanline filters and missing indexed palettes", () => {
  const base = Uint8Array.fromBase64(testPng({ width: 1, height: 1 }));
  expect(
    validatePngImage(withInflated(base, Uint8Array.from([5, 0, 0, 0]))),
  ).toBeUndefined();
  const indexed = chunks(base);
  indexed[0]!.data[8] = 1;
  indexed[0]!.data[9] = 3;
  indexed.find((part) => part.type === "IDAT")!.data = new Uint8Array(
    deflateSync(Uint8Array.from([0, 0])),
  );
  expect(validatePngImage(encodePng(indexed))).toBeUndefined();
});

test("keeps accepting trailing zlib data and rejects incomplete streams", () => {
  const base = Uint8Array.fromBase64(testPng({ width: 1, height: 1 }));
  const parts = chunks(base);
  const idat = parts.find((part) => part.type === "IDAT")!;
  const compressed = new Uint8Array(idat.data.length + 4);
  compressed.set(idat.data);
  compressed.set([1, 2, 3, 4], idat.data.length);
  expect(
    validatePngImage(
      encodePng(
        parts.map((part) =>
          part.type === "IDAT" ? { ...part, data: compressed } : part,
        ),
      ),
    ),
  ).toBeDefined();
  expect(
    validatePngImage(
      encodePng(
        parts.map((part) =>
          part.type === "IDAT"
            ? { ...part, data: part.data.subarray(0, part.data.length - 2) }
            : part,
        ),
      ),
    ),
  ).toBeUndefined();
});

test("rejects unknown critical PNG chunks and invalid depth/color combinations", () => {
  const base = Uint8Array.fromBase64(testPng({ width: 1, height: 1 }));
  const parts = chunks(base);
  expect(
    validatePngImage(
      encodePng([
        parts[0]!,
        { type: "ABCD", data: new Uint8Array() },
        ...parts.slice(1),
      ]),
    ),
  ).toBeUndefined();
  const badDepth = parts.map((part) => ({
    ...part,
    data:
      part.type === "IHDR"
        ? Uint8Array.from(part.data, (byte, i) => (i === 8 ? 1 : byte))
        : part.data,
  }));
  expect(validatePngImage(encodePng(badDepth))).toBeUndefined();
});

test("accepts supported JPEG frame types and rejects unsupported precision and frame types", () => {
  for (const marker of [0xc0, 0xc1, 0xc2])
    expect(validateJpegImage(jpeg(marker))).toBeDefined();
  for (const marker of [
    0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf,
  ])
    expect(validateJpegImage(jpeg(marker))).toBeUndefined();
  expect(validateJpegImage(jpeg(0xc0, 12))).toBeUndefined();
  expect(validateJpegImage(jpeg(0xc0).slice(0, 0))).toBeUndefined();
});

test("accepts interlaced PNGs whose Adam7 passes have zero width", () => {
  const base = Uint8Array.fromBase64(testPng({ width: 1, height: 1 }));
  const interlaced = chunks(base);
  interlaced[0]!.data[12] = 1;
  interlaced.find((part) => part.type === "IDAT")!.data = new Uint8Array(
    deflateSync(Uint8Array.from([0, 0, 0, 0])),
  );
  expect(validatePngImage(encodePng(interlaced))).toEqual({
    width: 1,
    height: 1,
  });
});

test("rejects PNG transparency chunks the backend cannot decode", () => {
  const base = Uint8Array.fromBase64(testPng({ width: 1, height: 1 }));
  const withTrns = (data: Uint8Array, afterIdat: boolean) => {
    const parts = chunks(base);
    const index = parts.findIndex((part) => part.type === "IDAT");
    parts.splice(afterIdat ? index + 1 : index, 0, { type: "tRNS", data });
    return encodePng(parts);
  };
  expect(validatePngImage(withTrns(new Uint8Array(6), false))).toBeDefined();
  expect(validatePngImage(withTrns(new Uint8Array(1), false))).toBeUndefined();
  expect(validatePngImage(withTrns(new Uint8Array(6), true))).toBeUndefined();
});
