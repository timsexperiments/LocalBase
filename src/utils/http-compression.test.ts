import { expect, test } from "bun:test";
import { brotliDecompressSync, gunzipSync } from "node:zlib";
import {
  appendVary,
  compressBody,
  compressJsonResponse,
  negotiateContentEncoding,
} from "./http-compression";

test("prefers brotli, honors q=0, and falls back to identity", () => {
  expect(negotiateContentEncoding("gzip, deflate, br")).toBe("br");
  expect(negotiateContentEncoding("gzip, br;q=0")).toBe("gzip");
  expect(negotiateContentEncoding("BR;Q=0.5")).toBe("br");
  expect(negotiateContentEncoding("*")).toBe("br");
  expect(negotiateContentEncoding("*;q=0, gzip")).toBe("gzip");
  expect(negotiateContentEncoding("identity")).toBeUndefined();
  expect(negotiateContentEncoding("gzip;q=0, br;q=0")).toBeUndefined();
  expect(negotiateContentEncoding("")).toBeUndefined();
  expect(negotiateContentEncoding(null)).toBeUndefined();
});

test("round-trips both encodings", () => {
  const body = new TextEncoder().encode("localbase ".repeat(500));
  expect(
    brotliDecompressSync(compressBody(body, "br", "static")).toString(),
  ).toBe("localbase ".repeat(500));
  expect(gunzipSync(compressBody(body, "gzip", "dynamic")).toString()).toBe(
    "localbase ".repeat(500),
  );
});

test("merges Vary without duplicating it", () => {
  const headers = new Headers({ vary: "Origin" });
  appendVary(headers, "Accept-Encoding");
  appendVary(headers, "accept-encoding");
  expect(headers.get("vary")).toBe("Origin, Accept-Encoding");
});

test("compresses large JSON and leaves small or non-JSON bodies alone", async () => {
  const json = JSON.stringify({ data: "x".repeat(4096) });
  const request = new Request("http://local/v1/models", {
    headers: { "accept-encoding": "gzip" },
  });
  const large = new Response(json, {
    headers: { "content-type": "application/json" },
  });
  const largeHeaders = new Headers(large.headers);
  const body = await compressJsonResponse(request, large, largeHeaders);
  expect(largeHeaders.get("content-encoding")).toBe("gzip");
  expect(largeHeaders.get("vary")).toBe("Accept-Encoding");
  expect(gunzipSync(body as Uint8Array).toString()).toBe(json);

  const small = new Response("{}", {
    headers: { "content-type": "application/json" },
  });
  const smallHeaders = new Headers(small.headers);
  await compressJsonResponse(request, small, smallHeaders);
  expect(smallHeaders.get("content-encoding")).toBeNull();
  expect(smallHeaders.get("vary")).toBe("Accept-Encoding");

  const text = new Response("x".repeat(4096), {
    headers: { "content-type": "text/plain" },
  });
  const textHeaders = new Headers(text.headers);
  await compressJsonResponse(request, text, textHeaders);
  expect(textHeaders.get("content-encoding")).toBeNull();
  expect(textHeaders.get("vary")).toBeNull();
});
