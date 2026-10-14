import {
  compressBody,
  type ContentEncoding,
  MIN_COMPRESSIBLE_BYTES,
  negotiateContentEncoding,
} from "../utils/http-compression";
import { assets, shell } from "./assets.generated";

const securityHeaders = {
  "content-security-policy":
    "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src data: blob:; media-src blob:; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
  "x-content-type-options": "nosniff",
  "referrer-policy": "no-referrer",
  "permissions-policy": "camera=(), microphone=(self), geolocation=()",
  "x-frame-options": "DENY",
} as const;

type StaticResource = {
  identity: Uint8Array<ArrayBuffer>;
  type: string;
  cacheControl: string;
  etag?: string;
  encoded: Map<ContentEncoding, Uint8Array<ArrayBuffer>>;
};

const resources = new Map<string, StaticResource>();

function resource(pathname: string): StaticResource | undefined {
  const shellPath =
    pathname === "/" || pathname === "/app" || pathname === "/app/";
  const key = shellPath ? "/app" : pathname;
  const cached = resources.get(key);
  if (cached) return cached;
  const source = shellPath
    ? { body: shell, type: "text/html; charset=utf-8" }
    : Object.hasOwn(assets, pathname)
      ? assets[pathname]
      : undefined;
  if (!source) return;
  const created: StaticResource = {
    identity: new TextEncoder().encode(source.body),
    type: source.type,
    // Asset names carry a content hash, so a given URL never changes; the
    // shell names the current hashes and must be revalidated on every load.
    cacheControl: shellPath
      ? "no-cache"
      : "public, max-age=31536000, immutable",
    etag: shellPath ? `"${Bun.hash(source.body).toString(36)}"` : undefined,
    encoded: new Map(),
  };
  resources.set(key, created);
  return created;
}

/** Compresses each resource at most once per encoding, on first request. */
function encodedBody(
  resource: StaticResource,
  encoding: ContentEncoding,
): Uint8Array<ArrayBuffer> {
  let body = resource.encoded.get(encoding);
  if (!body) {
    body = compressBody(resource.identity, encoding, "static");
    resource.encoded.set(encoding, body);
  }
  return body;
}

function etagMatches(header: string | null, etag: string): boolean {
  if (!header) return false;
  return header
    .split(",")
    .map((tag) => tag.trim().replace(/^W\//, ""))
    .some((tag) => tag === etag || tag === "*");
}

/** Only these public resources bypass gateway authentication. */
export function playgroundResponse(
  request: Request,
  pathname: string,
): Response | undefined {
  if (request.method !== "GET" && request.method !== "HEAD") return;
  const asset = resource(pathname);
  if (!asset) return;
  const compressible = asset.identity.byteLength >= MIN_COMPRESSIBLE_BYTES;
  const headers: Record<string, string> = {
    "content-type": asset.type,
    "cache-control": asset.cacheControl,
    ...securityHeaders,
  };
  if (compressible) headers.vary = "Accept-Encoding";
  if (asset.etag) {
    headers.etag = asset.etag;
    if (etagMatches(request.headers.get("if-none-match"), asset.etag))
      return new Response(null, { status: 304, headers });
  }
  const encoding = compressible
    ? negotiateContentEncoding(request.headers.get("accept-encoding"))
    : undefined;
  const body = encoding ? encodedBody(asset, encoding) : asset.identity;
  if (encoding) headers["content-encoding"] = encoding;
  headers["content-length"] = String(body.byteLength);
  return new Response(request.method === "HEAD" ? null : body, { headers });
}
