import { assets, shell } from "./assets.generated";

/** Only these public resources bypass gateway authentication. */
export function playgroundResponse(
  request: Request,
  pathname: string,
): Response | undefined {
  if (request.method !== "GET" && request.method !== "HEAD") return;
  const asset =
    pathname === "/" || pathname === "/app" || pathname === "/app/"
      ? { body: shell, type: "text/html; charset=utf-8" }
      : Object.hasOwn(assets, pathname)
        ? assets[pathname]
        : undefined;
  if (!asset) return;
  return new Response(request.method === "HEAD" ? null : asset.body, {
    headers: {
      "content-type": asset.type,
      "content-length": String(new TextEncoder().encode(asset.body).byteLength),
      "cache-control": "no-store",
      "content-security-policy":
        "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src data: blob:; media-src blob:; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
      "x-content-type-options": "nosniff",
      "referrer-policy": "no-referrer",
      "permissions-policy": "camera=(), microphone=(), geolocation=()",
      "x-frame-options": "DENY",
    },
  });
}
