import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";
import { z } from "zod";
import {
  browserAccessConfigSchema,
  loadBrowserAccessConfig,
  type BrowserAccessConfig,
} from "../domains/auth/browser-access";
import type { Permission } from "../domains/auth/authorization";
import { videoJobIdFromPath } from "../domains/runtime/route-dispatch";

export {
  browserAccessConfigSchema as uiAccessConfigSchema,
  loadBrowserAccessConfig as loadUiAccessConfig,
};

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
  config: BrowserAccessConfig | null;
  keyResolver?: JWTVerifyGetKey;
}) {
  const credentials = new WeakMap<
    Request,
    Readonly<{ ownerId: string; permissions: readonly Permission[] }>
  >();
  const issuer = config ? `https://${config.provider.teamDomain}` : null;
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
          audience: config.provider.audience,
          requiredClaims: ["exp", "sub", "email"],
        });
        const human = humanClaimsSchema.parse(payload);
        ownerId = `browser:${new Bun.CryptoHasher("sha256")
          .update(JSON.stringify([config.provider.kind, issuer, human.sub]))
          .digest("hex")}`;
      } catch {
        return respond(failure(401));
      }
      if (session) {
        credentials.set(
          request,
          Object.freeze({ ownerId, permissions: config.permissions }),
        );
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
      credentials.set(
        forwarded,
        Object.freeze({ ownerId, permissions: config.permissions }),
      );
      return { kind: "forward", request: forwarded, pathname };
    },
  };
}
