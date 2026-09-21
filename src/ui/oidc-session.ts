import { Buffer } from "node:buffer";
import { timingSafeEqual } from "node:crypto";
import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";
import { z } from "zod";
import type { OidcAccessProvider } from "../domains/auth/browser-access";
import type { BrowserIdentity } from "../domains/auth/browser-policy";

const loginStateCookie = "__Host-localbase-oidc-state";
const sessionCookie = "__Host-localbase-session";
const loginStateTtlMs = 10 * 60 * 1_000;
const sessionTtlMs = 12 * 60 * 60 * 1_000;
const maximumLoginStates = 128;
const maximumSessions = 1_024;
const maximumResponseBytes = 64 * 1_024;
const allowedAlgorithms = [
  "RS256",
  "RS384",
  "RS512",
  "PS256",
  "PS384",
  "PS512",
  "ES256",
  "ES384",
  "ES512",
  "EdDSA",
] as const;

const metadataSchema = z.object({
  issuer: z.string(),
  authorization_endpoint: z.string().url(),
  token_endpoint: z.string().url(),
  jwks_uri: z.string().url(),
  response_types_supported: z.array(z.string()),
  code_challenge_methods_supported: z.array(z.string()).optional(),
  id_token_signing_alg_values_supported: z.array(z.string()),
  token_endpoint_auth_methods_supported: z.array(z.string()).optional(),
});

const tokenResponseSchema = z.object({
  id_token: z
    .string()
    .min(1)
    .max(48 * 1_024),
});

const identitySchema = z.object({
  sub: z
    .string()
    .min(1)
    .max(255)
    .regex(/^[\x00-\x7F]+$/),
  nonce: z.string().min(1),
  aud: z.union([z.string(), z.array(z.string()).nonempty()]),
  azp: z.string().optional(),
  exp: z.number().int().positive(),
  iat: z.number().int().nonnegative(),
  email: z.string().optional(),
  email_verified: z.boolean().optional(),
});

type Metadata = z.infer<typeof metadataSchema>;
type Fetcher = (
  input: string | URL | Request,
  init?: RequestInit,
) => Promise<Response>;
type LoginState = Readonly<{
  verifier: string;
  nonce: string;
  expiresAt: number;
}>;
type BrowserSession = Readonly<{
  ownerId: string;
  identity: BrowserIdentity;
  expiresAt: number;
}>;

function redirect(location: string, cookies: readonly string[] = []): Response {
  const headers = new Headers({
    location,
    "cache-control": "no-store",
    "referrer-policy": "no-referrer",
  });
  for (const cookie of cookies) headers.append("set-cookie", cookie);
  return new Response(null, { status: 303, headers });
}

function secureCookie(
  name: string,
  value: string,
  maxAgeSeconds: number,
): string {
  return `${name}=${value}; Path=/; Max-Age=${maxAgeSeconds}; HttpOnly; Secure; SameSite=Lax`;
}

function clearCookie(name: string): string {
  return secureCookie(name, "", 0);
}

function cookieValue(request: Request, name: string): string | null {
  const matches = (request.headers.get("cookie") ?? "")
    .split(";")
    .map((part) => part.trim())
    .filter((part) => part.startsWith(`${name}=`));
  if (matches.length !== 1) return null;
  const value = matches[0]?.slice(name.length + 1) ?? "";
  return /^[A-Za-z0-9_-]{32,128}$/.test(value) ? value : null;
}

function randomToken(bytes = 32): string {
  return Buffer.from(crypto.getRandomValues(new Uint8Array(bytes))).toString(
    "base64url",
  );
}

async function sha256Base64Url(value: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value),
  );
  return Buffer.from(digest).toString("base64url");
}

function equalTokens(left: string, right: string): boolean {
  const a = Buffer.from(left);
  const b = Buffer.from(right);
  return a.byteLength === b.byteLength && timingSafeEqual(a, b);
}

function prune<T extends { expiresAt: number }>(
  values: Map<string, T>,
  now: number,
  maximum: number,
): void {
  for (const [key, value] of values) {
    if (value.expiresAt <= now) values.delete(key);
  }
  while (values.size >= maximum) {
    const oldest = values.keys().next().value;
    if (typeof oldest !== "string") break;
    values.delete(oldest);
  }
}

function discoveryUrl(issuer: string): URL {
  const url = new URL(issuer);
  const issuerPath = url.pathname.replace(/\/$/, "");
  url.pathname = `${issuerPath}/.well-known/openid-configuration`;
  return url;
}

function isHttpsUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return (
      url.protocol === "https:" &&
      url.username === "" &&
      url.password === "" &&
      url.hash === ""
    );
  } catch {
    return false;
  }
}

async function boundedJson(response: Response): Promise<unknown> {
  const declared = Number(response.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > maximumResponseBytes)
    throw new Error("OpenID Connect response exceeded its size limit.");
  if (!response.body) return null;
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maximumResponseBytes)
        throw new Error("OpenID Connect response exceeded its size limit.");
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return JSON.parse(new TextDecoder().decode(bytes));
}

function formComponent(value: string): string {
  return new URLSearchParams({ value }).toString().slice("value=".length);
}

export function createOidcSessionManager({
  provider,
  origin,
  keyResolver,
  fetcher = fetch,
  now = Date.now,
}: {
  provider: OidcAccessProvider;
  origin: string;
  keyResolver?: JWTVerifyGetKey;
  fetcher?: Fetcher;
  now?: () => number;
}) {
  const loginStates = new Map<string, LoginState>();
  const sessions = new Map<string, BrowserSession>();
  let metadataPromise: Promise<Metadata> | null = null;
  let keys: JWTVerifyGetKey | null = keyResolver ?? null;

  const metadata = async (): Promise<Metadata> => {
    if (metadataPromise) return metadataPromise;
    metadataPromise = (async () => {
      const response = await fetcher(discoveryUrl(provider.issuer), {
        headers: { accept: "application/json" },
        redirect: "error",
        signal: AbortSignal.timeout(5_000),
      });
      if (!response.ok) throw new Error("OpenID Connect discovery failed.");
      const value = metadataSchema.parse(await boundedJson(response));
      if (
        value.issuer !== provider.issuer ||
        !value.response_types_supported.includes("code") ||
        !value.code_challenge_methods_supported?.includes("S256") ||
        !isHttpsUrl(value.authorization_endpoint) ||
        !isHttpsUrl(value.token_endpoint) ||
        !isHttpsUrl(value.jwks_uri) ||
        !value.id_token_signing_alg_values_supported.some((algorithm) =>
          allowedAlgorithms.includes(
            algorithm as (typeof allowedAlgorithms)[number],
          ),
        )
      )
        throw new Error("OpenID Connect discovery is incompatible.");
      const authentication =
        provider.clientAuthentication.kind === "client-secret-basic"
          ? "client_secret_basic"
          : "none";
      const supportedAuthentication =
        value.token_endpoint_auth_methods_supported ?? ["client_secret_basic"];
      if (!supportedAuthentication.includes(authentication))
        throw new Error(
          "OpenID Connect token endpoint authentication is incompatible.",
        );
      keys ??= createRemoteJWKSet(new URL(value.jwks_uri), {
        timeoutDuration: 5_000,
        cooldownDuration: 30_000,
        cacheMaxAge: 600_000,
      });
      return value;
    })().catch((error: unknown) => {
      metadataPromise = null;
      throw error;
    });
    return metadataPromise;
  };

  const exchange = async (
    code: string,
    state: LoginState,
    request: Request,
  ): Promise<BrowserSession> => {
    const discovered = await metadata();
    const body = new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: `${origin}/app/callback`,
      code_verifier: state.verifier,
    });
    const headers = new Headers({
      accept: "application/json",
      "content-type": "application/x-www-form-urlencoded",
    });
    if (provider.clientAuthentication.kind === "none") {
      body.set("client_id", provider.clientId);
    } else {
      const basic = Buffer.from(
        `${formComponent(provider.clientId)}:${formComponent(provider.clientAuthentication.clientSecret)}`,
      ).toString("base64");
      headers.set("authorization", `Basic ${basic}`);
    }
    const response = await fetcher(discovered.token_endpoint, {
      method: "POST",
      headers,
      body,
      redirect: "error",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(5_000)]),
    });
    if (!response.ok) throw new Error("OpenID Connect token exchange failed.");
    const token = tokenResponseSchema.parse(await boundedJson(response));
    if (!keys) throw new Error("OpenID Connect keys are unavailable.");
    const algorithms = discovered.id_token_signing_alg_values_supported.filter(
      (algorithm): algorithm is (typeof allowedAlgorithms)[number] =>
        allowedAlgorithms.includes(
          algorithm as (typeof allowedAlgorithms)[number],
        ),
    );
    const { payload } = await jwtVerify(token.id_token, keys, {
      algorithms,
      issuer: provider.issuer,
      audience: provider.clientId,
      requiredClaims: ["exp", "iat", "sub", "nonce"],
    });
    const identity = identitySchema.parse(payload);
    if (!equalTokens(identity.nonce, state.nonce))
      throw new Error("OpenID Connect nonce did not match.");
    if (
      Array.isArray(identity.aud) && identity.aud.length > 1
        ? identity.azp !== provider.clientId
        : identity.azp !== undefined && identity.azp !== provider.clientId
    )
      throw new Error("OpenID Connect authorized party did not match.");
    const verifiedEmail =
      identity.email_verified === true
        ? z.string().email().safeParse(identity.email).data
        : undefined;
    return {
      ownerId: `browser:${new Bun.CryptoHasher("sha256")
        .update(JSON.stringify([provider.kind, provider.issuer, identity.sub]))
        .digest("hex")}`,
      identity: {
        issuer: provider.issuer,
        subject: identity.sub,
        ...(verifiedEmail ? { verifiedEmail } : {}),
      },
      expiresAt: Math.min(identity.exp * 1_000, now() + sessionTtlMs),
    };
  };

  return {
    async startLogin(): Promise<Response> {
      const discovered = await metadata();
      const state = randomToken();
      const verifier = randomToken(64);
      const nonce = randomToken();
      prune(loginStates, now(), maximumLoginStates);
      loginStates.set(state, {
        verifier,
        nonce,
        expiresAt: now() + loginStateTtlMs,
      });
      const authorization = new URL(discovered.authorization_endpoint);
      for (const [key, value] of Object.entries({
        response_type: "code",
        client_id: provider.clientId,
        redirect_uri: `${origin}/app/callback`,
        scope: "openid profile email",
        state,
        nonce,
        code_challenge: await sha256Base64Url(verifier),
        code_challenge_method: "S256",
      }))
        authorization.searchParams.set(key, value);
      return redirect(authorization.href, [
        secureCookie(
          loginStateCookie,
          state,
          Math.floor(loginStateTtlMs / 1_000),
        ),
      ]);
    },

    async completeLogin(request: Request, url: URL): Promise<Response> {
      const stateValue = url.searchParams.get("state") ?? "";
      const cookieState = cookieValue(request, loginStateCookie) ?? "";
      const state = loginStates.get(stateValue);
      loginStates.delete(stateValue);
      const failed = () =>
        redirect(`${origin}/app?signin=failed`, [
          clearCookie(loginStateCookie),
        ]);
      if (
        !state ||
        state.expiresAt <= now() ||
        !equalTokens(stateValue, cookieState) ||
        url.searchParams.has("error")
      )
        return failed();
      const code = url.searchParams.get("code");
      if (!code || code.length > 8_192) return failed();
      try {
        const identity = await exchange(code, state, request);
        if (identity.expiresAt <= now()) return failed();
        prune(sessions, now(), maximumSessions);
        const session = randomToken();
        sessions.set(session, identity);
        return redirect(`${origin}/app`, [
          clearCookie(loginStateCookie),
          secureCookie(
            sessionCookie,
            session,
            Math.max(1, Math.floor((identity.expiresAt - now()) / 1_000)),
          ),
        ]);
      } catch {
        return failed();
      }
    },

    authenticate(request: Request): BrowserSession | null {
      const token = cookieValue(request, sessionCookie);
      const session = token ? sessions.get(token) : null;
      if (!token || !session || session.expiresAt <= now()) {
        if (token) sessions.delete(token);
        return null;
      }
      return session;
    },

    logout(request: Request): Response {
      const token = cookieValue(request, sessionCookie);
      if (token) sessions.delete(token);
      return new Response(null, {
        status: 204,
        headers: { "set-cookie": clearCookie(sessionCookie) },
      });
    },
  };
}
