import { brotliCompressSync, constants, gzipSync } from "node:zlib";

export type ContentEncoding = "br" | "gzip";

/** Bodies smaller than this gain too little from compression to bother. */
export const MIN_COMPRESSIBLE_BYTES = 1024;

/**
 * Picks the best encoding the client accepts, preferring brotli over gzip.
 * Honors `q=0` exclusions and the `*` wildcard; returns undefined for
 * identity.
 */
export function negotiateContentEncoding(
  acceptEncoding: string | null,
): ContentEncoding | undefined {
  if (!acceptEncoding) return undefined;
  const weights = new Map<string, number>();
  for (const part of acceptEncoding.split(",")) {
    const [rawName, ...params] = part.trim().split(";");
    const name = rawName?.trim().toLowerCase();
    if (!name) continue;
    let weight = 1;
    for (const param of params) {
      const [key, value] = param.trim().split("=");
      if (key?.trim().toLowerCase() !== "q") continue;
      const parsed = Number(value);
      weight = Number.isFinite(parsed) ? parsed : 0;
    }
    weights.set(name, weight);
  }
  const accepts = (name: ContentEncoding) =>
    (weights.get(name) ?? weights.get("*") ?? 0) > 0;
  if (accepts("br")) return "br";
  if (accepts("gzip")) return "gzip";
  return undefined;
}

/**
 * Compresses a body. `static` favors ratio for bodies compressed once and
 * served many times; `dynamic` favors latency for per-request bodies.
 */
export function compressBody(
  body: Uint8Array,
  encoding: ContentEncoding,
  mode: "static" | "dynamic",
): Uint8Array<ArrayBuffer> {
  if (encoding === "gzip")
    return new Uint8Array(gzipSync(body, { level: mode === "static" ? 9 : 6 }));
  return new Uint8Array(
    brotliCompressSync(body, {
      params: {
        [constants.BROTLI_PARAM_QUALITY]: mode === "static" ? 9 : 4,
        [constants.BROTLI_PARAM_SIZE_HINT]: body.byteLength,
      },
    }),
  );
}

export function appendVary(headers: Headers, value: string): void {
  const current = headers.get("vary");
  if (!current) {
    headers.set("vary", value);
    return;
  }
  const names = current.split(",").map((name) => name.trim().toLowerCase());
  if (!names.includes(value.toLowerCase()) && !names.includes("*"))
    headers.set("vary", `${current}, ${value}`);
}

/**
 * Buffers a complete JSON response and compresses it when the client accepts
 * an encoding and the body is large enough. Only call this for handlers that
 * build their JSON in memory; streamed upstream bodies must not be buffered.
 */
export async function compressJsonResponse(
  request: Request,
  response: Response,
  headers: Headers,
): Promise<BodyInit | null> {
  if (
    request.method !== "GET" ||
    !response.body ||
    headers.has("content-encoding") ||
    !headers.get("content-type")?.toLowerCase().startsWith("application/json")
  )
    return response.body;
  appendVary(headers, "Accept-Encoding");
  const encoding = negotiateContentEncoding(
    request.headers.get("accept-encoding"),
  );
  if (!encoding) return response.body;
  const body = new Uint8Array(await response.arrayBuffer());
  if (body.byteLength < MIN_COMPRESSIBLE_BYTES) return body;
  const compressed = compressBody(body, encoding, "dynamic");
  headers.set("content-encoding", encoding);
  headers.set("content-length", String(compressed.byteLength));
  return compressed;
}
