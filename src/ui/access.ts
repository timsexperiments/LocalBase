import { join } from "node:path";
import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";
import { z } from "zod";
import { videoJobIdFromPath } from "../domains/runtime/route-dispatch";

export const uiAccessConfigSchema = z
  .object({
    teamDomain: z
      .string()
      .regex(/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$/),
    audience: z
      .string()
      .min(1)
      .refine((value) => value.trim() === value),
    origin: z
      .string()
      .url()
      .refine((value) => {
        try {
          const url = new URL(value);
          return url.protocol === "https:" && url.origin === value;
        } catch {
          return false;
        }
      }, "Expected an exact HTTPS origin without path, credentials, or trailing slash."),
  })
  .strict();

export async function loadUiAccessConfig(root: string) {
  let contents: string;
  try {
    contents = await Bun.file(join(root, "ui-access.json")).text();
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return null;
    throw error;
  }
  try {
    return uiAccessConfigSchema.parse(JSON.parse(contents));
  } catch {
    throw new Error(
      "Invalid ui-access.json. Expected strict teamDomain, audience, and HTTPS origin configuration.",
    );
  }
}

const humanClaimsSchema = z.object({
  sub: z
    .string()
    .min(1)
    .refine((value) => value.trim().length > 0),
  email: z
    .string()
    .email()
    .refine((value) => !value.toLowerCase().startsWith("non_identity@")),
  type: z.literal("app"),
  common_name: z.never().optional(),
  service_token_id: z.never().optional(),
  service_token_status: z.literal(false).optional(),
});

export function isUiAccessPath(pathname: string): boolean {
  return (
    pathname === "/app/session" ||
    pathname === "/app/api" ||
    pathname.startsWith("/app/api/")
  );
}

function allowedUiRoute(method: string, pathname: string): boolean {
  if (pathname === "/_localbase/model-management")
    return method === "GET" || method === "POST";
  if (method === "GET" && pathname === "/_localbase/models") return true;
  if (
    method === "POST" &&
    [
      "/v1/chat/completions",
      "/v1/embeddings",
      "/v1/images/generations",
      "/v1/audio/speech",
      "/v1/audio/transcriptions",
      "/v1/videos",
    ].includes(pathname)
  )
    return true;
  if (!videoJobIdFromPath(pathname)) return false;
  if (pathname.endsWith("/content")) return method === "GET";
  if (pathname.endsWith("/cancel")) return method === "POST";
  return method === "GET" || method === "DELETE";
}

function failure(status: number): Response {
  return Response.json(
    {
      error: {
        message: "UI access denied.",
        type: "authentication_error",
        code: "ui_access_denied",
      },
    },
    {
      status,
      headers: { "cache-control": "no-store" },
    },
  );
}

type UiAccessResult =
  | { kind: "pass" }
  | { kind: "response"; response: Response }
  | { kind: "forward"; request: Request; pathname: string };

/** Only requests verified here can receive a request-scoped gateway identity. */
export function createUiAccess({
  config,
  keyResolver,
}: {
  config: Awaited<ReturnType<typeof loadUiAccessConfig>>;
  keyResolver?: JWTVerifyGetKey;
}) {
  const credentials = new WeakMap<Request, Readonly<{ ownerId: string }>>();
  const issuer = config ? `https://${config.teamDomain}` : null;
  const keys = issuer
    ? (keyResolver ??
      createRemoteJWKSet(new URL(`${issuer}/cdn-cgi/access/certs`), {
        timeoutDuration: 5_000,
        cooldownDuration: 30_000,
        cacheMaxAge: 600_000,
      }))
    : null;

  return {
    credential(request: Request) {
      return credentials.get(request);
    },
    async handle(request: Request): Promise<UiAccessResult> {
      const url = new URL(request.url);
      if (!isUiAccessPath(url.pathname)) return { kind: "pass" };
      const session = url.pathname === "/app/session";
      const respond = (response: Response): UiAccessResult => ({
        kind: "response",
        response,
      });
      if (session && request.method !== "GET") return respond(failure(405));
      if (!config || !issuer || !keys) {
        return respond(failure(401));
      }
      const origin = request.headers.get("origin");
      const fetchSite = request.headers.get("sec-fetch-site");
      const expectedHost = new URL(config.origin).host;
      if (
        request.headers.get("x-localbase-ui") !== "1" ||
        url.host !== expectedHost ||
        (request.headers.has("host") &&
          request.headers.get("host") !== expectedHost) ||
        (fetchSite !== null && fetchSite !== "same-origin") ||
        (origin !== null && origin !== config.origin) ||
        (request.method !== "GET" && origin !== config.origin)
      ) {
        return respond(failure(403));
      }
      const pathname = url.pathname.slice("/app/api".length);
      if (!session && !allowedUiRoute(request.method, pathname))
        return respond(failure(404));
      const token = request.headers.get("cf-access-jwt-assertion");
      if (!token) return respond(failure(401));
      let ownerId: string;
      try {
        const { payload } = await jwtVerify(token, keys, {
          algorithms: ["RS256"],
          issuer,
          audience: config.audience,
          requiredClaims: ["exp", "sub", "email"],
        });
        const human = humanClaimsSchema.parse(payload);
        ownerId = `ui-access:${new Bun.CryptoHasher("sha256")
          .update(JSON.stringify([issuer, human.sub]))
          .digest("hex")}`;
      } catch {
        return respond(failure(401));
      }
      if (session) {
        credentials.set(request, Object.freeze({ ownerId }));
        return respond(
          Response.json(
            { authenticated: true },
            {
              headers: { "cache-control": "no-store" },
            },
          ),
        );
      }
      const headers = new Headers(request.headers);
      for (const name of [...headers.keys()]) {
        if (
          name.startsWith("cf-access-") ||
          ["cookie", "authorization", "x-api-key", "x-localbase-ui"].includes(
            name,
          )
        )
          headers.delete(name);
      }
      url.pathname = pathname;
      const forwarded = new Request(url, {
        method: request.method,
        headers,
        body: request.body,
        signal: request.signal,
      });
      credentials.set(forwarded, Object.freeze({ ownerId }));
      return { kind: "forward", request: forwarded, pathname };
    },
  };
}
