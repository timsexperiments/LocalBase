import { join } from "node:path";
import { createHmac, timingSafeEqual } from "node:crypto";
import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";
import { z } from "zod";
import { videoJobIdFromPath } from "../domains/runtime/route-dispatch";

const browserSessionCookie = "localbase_ui_session";
const browserSessionDurationSeconds = 30 * 24 * 60 * 60;
const clearedBrowserSessionCookie = `${browserSessionCookie}=; Path=/app; Max-Age=0; HttpOnly; Secure; SameSite=Strict`;
const browserSessionPayloadSchema = z
  .object({
    version: z.literal(1),
    ownerId: z.string().min(1).max(256),
    expiresAt: z.number().int().positive(),
  })
  .strict();

type BrowserSessionCredential = Readonly<{
  ownerId: string;
  signingSecret: string;
}>;

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

function failure(status: number, clearBrowserSession = false): Response {
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
      headers: {
        "cache-control": "no-store",
        ...(clearBrowserSession
          ? { "set-cookie": clearedBrowserSessionCookie }
          : {}),
      },
    },
  );
}

function apiKeyMode(clearBrowserSession = false): Response {
  return Response.json(
    { authenticated: false, mode: "api-key" },
    {
      headers: {
        "cache-control": "no-store",
        ...(clearBrowserSession
          ? { "set-cookie": clearedBrowserSessionCookie }
          : {}),
      },
    },
  );
}

function manualBrowserBoundary(request: Request, url: URL): boolean {
  const origin = request.headers.get("origin");
  const fetchSite = request.headers.get("sec-fetch-site");
  let originMatches = origin === null;
  if (origin !== null) {
    try {
      const parsed = new URL(origin);
      originMatches =
        parsed.origin === origin &&
        parsed.host === url.host &&
        (parsed.protocol === "http:" || parsed.protocol === "https:");
    } catch {
      originMatches = false;
    }
  }
  return (
    request.headers.get("x-localbase-ui") === "1" &&
    (!request.headers.has("host") ||
      request.headers.get("host") === url.host) &&
    (fetchSite === null || fetchSite === "same-origin") &&
    originMatches &&
    (request.method === "GET" || origin !== null)
  );
}

function bearerToken(request: Request): string | null {
  const match = /^Bearer ([\x21-\x7e]+)$/.exec(
    request.headers.get("authorization") ?? "",
  );
  return match?.[1] ?? null;
}

function sessionCookieValue(request: Request): string | null {
  const values = (request.headers.get("cookie") ?? "")
    .split(";")
    .map((part) => part.trim())
    .filter((part) => part.startsWith(`${browserSessionCookie}=`))
    .map((part) => part.slice(browserSessionCookie.length + 1));
  return values.length === 1 && values[0] ? values[0] : null;
}

function sessionSignature(payload: string, signingSecret: string): string {
  return createHmac("sha256", signingSecret)
    .update(payload)
    .digest("base64url");
}

function issueBrowserSession(
  credential: BrowserSessionCredential,
  now: number,
): string {
  const expiresAt = now + browserSessionDurationSeconds * 1000;
  const payload = Buffer.from(
    JSON.stringify({ version: 1, ownerId: credential.ownerId, expiresAt }),
  ).toString("base64url");
  const signature = sessionSignature(payload, credential.signingSecret);
  return `${browserSessionCookie}=${payload}.${signature}; Path=/app; Max-Age=${browserSessionDurationSeconds}; HttpOnly; Secure; SameSite=Strict`;
}

function readBrowserSession(
  request: Request,
  resolveSessionOwner: (
    ownerId: string,
  ) => BrowserSessionCredential | undefined,
  now: number,
): BrowserSessionCredential | undefined {
  const token = sessionCookieValue(request);
  if (!token) return undefined;
  const [encoded, providedSignature, extra] = token.split(".");
  if (!encoded || !providedSignature || extra !== undefined) return undefined;
  let value: unknown;
  try {
    value = JSON.parse(Buffer.from(encoded, "base64url").toString("utf8"));
  } catch {
    return undefined;
  }
  const payload = browserSessionPayloadSchema.safeParse(value);
  if (!payload.success || payload.data.expiresAt <= now) return undefined;
  const credential = resolveSessionOwner(payload.data.ownerId);
  if (!credential || credential.ownerId !== payload.data.ownerId)
    return undefined;
  const expectedSignature = sessionSignature(encoded, credential.signingSecret);
  const provided = Buffer.from(providedSignature);
  const expected = Buffer.from(expectedSignature);
  if (
    provided.length !== expected.length ||
    !timingSafeEqual(provided, expected)
  )
    return undefined;
  return credential;
}

type UiAccessResult =
  | { kind: "pass" }
  | { kind: "response"; response: Response }
  | { kind: "forward"; request: Request; pathname: string };

/** Only requests verified here can receive a request-scoped gateway identity. */
export function createUiAccess({
  config,
  keyResolver,
  authenticateApiKey,
  resolveSessionOwner,
  now = Date.now,
}: {
  config: Awaited<ReturnType<typeof loadUiAccessConfig>>;
  keyResolver?: JWTVerifyGetKey;
  authenticateApiKey?: (key: string) => BrowserSessionCredential | undefined;
  resolveSessionOwner?: (
    ownerId: string,
  ) => BrowserSessionCredential | undefined;
  now?: () => number;
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
      if (session && request.method !== "GET" && request.method !== "POST")
        return respond(failure(405));
      const expectedHost = config ? new URL(config.origin).host : null;
      const manual = expectedHost === null || url.host !== expectedHost;
      if (manual) {
        const cookie = sessionCookieValue(request);
        if (!config && session && request.method === "GET" && !cookie)
          return respond(apiKeyMode());
        if (!config && !session && !cookie) return respond(failure(401));
        if (!manualBrowserBoundary(request, url)) return respond(failure(403));
        if (session && request.method === "POST") {
          if (!authenticateApiKey || !resolveSessionOwner)
            return respond(failure(405));
          const origin = request.headers.get("origin");
          if (!origin || new URL(origin).protocol !== "https:")
            return respond(apiKeyMode());
          const token = bearerToken(request);
          const credential = token ? authenticateApiKey(token) : undefined;
          if (!credential) return respond(failure(401));
          const cookie = issueBrowserSession(credential, now());
          credentials.set(
            request,
            Object.freeze({ ownerId: credential.ownerId }),
          );
          return respond(
            Response.json(
              { authenticated: true },
              {
                headers: {
                  "cache-control": "no-store",
                  "set-cookie": cookie,
                },
              },
            ),
          );
        }
        const credential =
          cookie && resolveSessionOwner
            ? readBrowserSession(request, resolveSessionOwner, now())
            : undefined;
        if (cookie && !credential)
          return respond(session ? apiKeyMode(true) : failure(401, true));
        if (session) {
          if (credential)
            credentials.set(
              request,
              Object.freeze({ ownerId: credential.ownerId }),
            );
          return respond(
            credential
              ? Response.json(
                  { authenticated: true },
                  { headers: { "cache-control": "no-store" } },
                )
              : apiKeyMode(),
          );
        }
        if (!credential) return respond(failure(403));
        const pathname = url.pathname.slice("/app/api".length);
        if (!allowedUiRoute(request.method, pathname))
          return respond(failure(404));
        const forwarded = forwardUiRequest(request, url, pathname);
        credentials.set(
          forwarded,
          Object.freeze({ ownerId: credential.ownerId }),
        );
        return { kind: "forward", request: forwarded, pathname };
      }
      if (session && request.method !== "GET" && request.method !== "POST")
        return respond(failure(405));
      if (!config || !issuer || !keys) {
        return respond(session ? apiKeyMode() : failure(401));
      }
      const origin = request.headers.get("origin");
      const fetchSite = request.headers.get("sec-fetch-site");
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
      const forwarded = forwardUiRequest(request, url, pathname);
      credentials.set(forwarded, Object.freeze({ ownerId }));
      return { kind: "forward", request: forwarded, pathname };
    },
  };
}

function forwardUiRequest(
  request: Request,
  url: URL,
  pathname: string,
): Request {
  const headers = new Headers(request.headers);
  for (const name of [...headers.keys()]) {
    if (
      name.startsWith("cf-access-") ||
      ["cookie", "authorization", "x-api-key", "x-localbase-ui"].includes(name)
    )
      headers.delete(name);
  }
  url.pathname = pathname;
  return new Request(url, {
    method: request.method,
    headers,
    body: request.body,
    signal: request.signal,
  });
}
