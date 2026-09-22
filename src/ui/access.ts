import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";
import { z } from "zod";
import {
  browserAccessConfigSchema,
  loadBrowserAccessConfig,
  type BrowserAccessConfig,
} from "../domains/auth/browser-access";
import type { Permission } from "../domains/auth/authorization";
import type { BrowserIdentity } from "../domains/auth/browser-identity";
import type { MagicLinkSessionAdapter } from "../domains/auth/magic-links";
import { videoJobIdFromPath } from "../domains/runtime/route-dispatch";
import {
  createDirectSessionManager,
  githubCallbackPath,
  magicLinkCallbackPath,
  magicLinkRequestPath,
  oidcCallbackPath,
} from "./oidc-session";

export {
  browserAccessConfigSchema as uiAccessConfigSchema,
  loadBrowserAccessConfig as loadUiAccessConfig,
};

const humanClaimsSchema = z.object({
  sub: z
    .string()
    .min(1)
    .max(255)
    .regex(/^[\x00-\x7F]+$/),
  email: z
    .string()
    .email()
    .refine((value) => !value.toLowerCase().startsWith("non_identity@")),
  type: z.literal("app"),
  common_name: z.never().optional(),
  service_token_id: z.never().optional(),
  service_token_status: z.literal(false).optional(),
});

type Fetcher = (
  input: string | URL | Request,
  init?: RequestInit,
) => Promise<Response>;

export function isUiAccessPath(pathname: string): boolean {
  return (
    pathname === "/app/login" ||
    pathname === oidcCallbackPath ||
    pathname === githubCallbackPath ||
    pathname === magicLinkCallbackPath ||
    pathname === magicLinkRequestPath ||
    pathname === "/app/logout" ||
    pathname === "/app/session" ||
    pathname === "/app/api" ||
    pathname.startsWith("/app/api/")
  );
}

function allowedUiRoute(method: string, pathname: string): boolean {
  if (
    ["/_localbase/access-management", "/_localbase/api-keys"].includes(pathname)
  )
    return method === "GET" || method === "POST";
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

function redirect(location: string): Response {
  return new Response(null, {
    status: 303,
    headers: {
      location,
      "cache-control": "no-store",
      "referrer-policy": "no-referrer",
    },
  });
}

type UiAccessResult =
  | { kind: "pass" }
  | { kind: "response"; response: Response }
  | { kind: "forward"; request: Request; pathname: string };

/** Only requests verified here can receive a request-scoped gateway identity. */
export function createUiAccess({
  config,
  keyResolver,
  fetcher,
  now,
  defer,
  authorizeIdentity,
  magicLinks,
}: {
  config: BrowserAccessConfig | null;
  keyResolver?: JWTVerifyGetKey;
  fetcher?: Fetcher;
  now?: () => number;
  defer?: (task: () => void) => void;
  authorizeIdentity?: (identity: BrowserIdentity) => Readonly<{
    matchedRoles: readonly string[];
    permissions: readonly Permission[];
  }> | null;
  magicLinks?: MagicLinkSessionAdapter;
}) {
  const credentials = new WeakMap<
    Request,
    Readonly<{
      ownerId: string;
      permissions: readonly Permission[];
      matchedRoles: readonly string[];
    }>
  >();
  const cloudflareIssuer =
    config?.provider.kind === "cloudflare-access"
      ? `https://${config.provider.teamDomain}`
      : null;
  const cloudflareKeys = cloudflareIssuer
    ? (keyResolver ??
      createRemoteJWKSet(new URL(`${cloudflareIssuer}/cdn-cgi/access/certs`), {
        timeoutDuration: 5_000,
        cooldownDuration: 30_000,
        cacheMaxAge: 600_000,
      }))
    : null;
  const direct =
    config?.provider.kind === "direct"
      ? createDirectSessionManager({
          provider: config.provider,
          origin: config.origin,
          ...(keyResolver ? { keyResolver } : {}),
          ...(fetcher ? { fetcher } : {}),
          ...(now ? { now } : {}),
          ...(defer ? { defer } : {}),
          ...(magicLinks ? { magicLinks } : {}),
        })
      : null;

  const validateCommonRequest = (request: Request, url: URL): boolean => {
    if (!config) return false;
    const origin = request.headers.get("origin");
    const fetchSite = request.headers.get("sec-fetch-site");
    const expectedHost = new URL(config.origin).host;
    return !(
      request.headers.get("x-localbase-ui") !== "1" ||
      url.host !== expectedHost ||
      (request.headers.has("host") &&
        request.headers.get("host") !== expectedHost) ||
      (fetchSite !== null && fetchSite !== "same-origin") ||
      (origin !== null && origin !== config.origin) ||
      (request.method !== "GET" && origin !== config.origin)
    );
  };

  return {
    credential(request: Request) {
      return credentials.get(request);
    },
    async handle(request: Request): Promise<UiAccessResult> {
      const url = new URL(request.url);
      if (!isUiAccessPath(url.pathname)) return { kind: "pass" };
      const respond = (response: Response): UiAccessResult => ({
        kind: "response",
        response,
      });
      if (!config) return respond(failure(401));
      const expectedHost = new URL(config.origin).host;
      const exactHost =
        url.host === expectedHost &&
        (!request.headers.has("host") ||
          request.headers.get("host") === expectedHost);

      if (url.pathname === "/app/login") {
        if (request.method !== "GET") return respond(failure(405));
        if (!exactHost) return respond(failure(403));
        if (!direct) return respond(redirect(`${config.origin}/app`));
        try {
          return respond(
            await direct.startLogin(url.searchParams.get("provider")),
          );
        } catch {
          return respond(failure(503));
        }
      }

      if (
        url.pathname === oidcCallbackPath ||
        url.pathname === githubCallbackPath
      ) {
        if (request.method !== "GET" || !exactHost || !direct)
          return respond(failure(403));
        return respond(await direct.completeLogin(request, url));
      }

      if (url.pathname === magicLinkRequestPath) {
        if (request.method !== "POST" || !exactHost || !direct)
          return respond(failure(403));
        return respond(await direct.requestMagicLink(request, url));
      }

      if (url.pathname === magicLinkCallbackPath) {
        if (!exactHost || !direct) return respond(failure(403));
        if (request.method === "GET")
          return respond(direct.magicLinkRedemption());
        if (request.method !== "POST") return respond(failure(403));
        return respond(await direct.completeMagicLink(request));
      }

      if (url.pathname === "/app/logout") {
        if (request.method !== "POST" || !validateCommonRequest(request, url))
          return respond(failure(403));
        return respond(
          direct ? direct.logout(request) : new Response(null, { status: 204 }),
        );
      }

      const sessionRequest = url.pathname === "/app/session";
      if (sessionRequest && request.method !== "GET")
        return respond(failure(405));
      if (!validateCommonRequest(request, url)) return respond(failure(403));
      const pathname = url.pathname.slice("/app/api".length);
      if (!sessionRequest && !allowedUiRoute(request.method, pathname))
        return respond(failure(404));

      let ownerId: string;
      let identity: BrowserIdentity;
      if (config.provider.kind === "cloudflare-access") {
        const token = request.headers.get("cf-access-jwt-assertion");
        if (!token || !cloudflareIssuer || !cloudflareKeys)
          return respond(failure(401));
        try {
          const { payload } = await jwtVerify(token, cloudflareKeys, {
            algorithms: ["RS256"],
            issuer: cloudflareIssuer,
            audience: config.provider.audience,
            requiredClaims: ["exp", "sub", "email"],
          });
          const human = humanClaimsSchema.parse(payload);
          ownerId = `browser:${new Bun.CryptoHasher("sha256")
            .update(
              JSON.stringify([
                config.provider.kind,
                cloudflareIssuer,
                human.sub,
              ]),
            )
            .digest("hex")}`;
          identity = {
            issuer: cloudflareIssuer,
            subject: human.sub,
            verifiedEmail: human.email,
          };
        } catch {
          return respond(failure(401));
        }
      } else {
        const authenticated = direct?.authenticate(request);
        if (!authenticated) return respond(failure(401));
        ownerId = authenticated.ownerId;
        identity = authenticated.identity;
      }

      const authorization = authorizeIdentity?.(identity) ?? {
        matchedRoles: [],
        permissions: config.permissions,
      };
      const credential = Object.freeze({ ownerId, ...authorization });

      if (sessionRequest) {
        credentials.set(request, credential);
        return respond(
          Response.json(
            {
              authenticated: true,
              ...(identity.verifiedEmail
                ? { verifiedEmail: identity.verifiedEmail }
                : {}),
            },
            { headers: { "cache-control": "no-store" } },
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
      url.search = "";
      const forwarded = new Request(url, {
        method: request.method,
        headers,
        body: request.body,
        signal: request.signal,
      });
      credentials.set(forwarded, credential);
      return { kind: "forward", request: forwarded, pathname };
    },
  };
}
