import { Buffer } from "node:buffer";
import { timingSafeEqual } from "node:crypto";
import { createRemoteJWKSet, jwtVerify, type JWTVerifyGetKey } from "jose";
import { z } from "zod";
import type {
  DirectAccessProvider,
  GithubAccessRegistration,
  OidcAccessRegistration,
} from "../domains/auth/browser-access";
import type { BrowserIdentity } from "../domains/auth/browser-policy";

const loginStateCookie = "__Host-localbase-login-state";
const sessionCookie = "__Host-localbase-session";
const loginStateTtlMs = 10 * 60 * 1_000;
const sessionTtlMs = 12 * 60 * 60 * 1_000;
const maximumLoginStates = 128;
const maximumSessions = 1_024;
const maximumResponseBytes = 64 * 1_024;
export const oidcCallbackPath = "/oidc/callback";
export const githubCallbackPath = "/github/callback";
const githubIssuer = "https://github.com";
const githubAuthorizationEndpoint = "https://github.com/login/oauth/authorize";
const githubTokenEndpoint = "https://github.com/login/oauth/access_token";
const githubUserEndpoint = "https://api.github.com/user";
const githubEmailsEndpoint = "https://api.github.com/user/emails";
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

const githubTokenResponseSchema = z.object({
  access_token: z.string().min(1).max(8_192),
  token_type: z.string().toLowerCase().pipe(z.literal("bearer")),
});

const githubUserSchema = z.object({
  id: z.number().int().positive(),
  login: z.string().min(1).max(255),
});

const githubEmailSchema = z.object({
  email: z.string().email(),
  verified: z.boolean(),
  primary: z.boolean(),
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
type LoginState =
  | Readonly<{
      kind: "oidc";
      registrationId: string;
      verifier: string;
      nonce: string;
      expiresAt: number;
    }>
  | Readonly<{
      kind: "github-oauth";
      registrationId: string;
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

function escapeHtml(value: string): string {
  return value.replace(
    /[&<>"']/g,
    (character) =>
      ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      })[character] ?? character,
  );
}

function providerChoice(provider: DirectAccessProvider): Response {
  const links = provider.registrations
    .map(
      (registration) =>
        `<a href="/app/login?provider=${encodeURIComponent(registration.id)}">${escapeHtml(registration.name)}</a>`,
    )
    .join("");
  return new Response(
    `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to LocalBase</title><style>body{font:16px system-ui;margin:0;background:#201e25;color:#eee9f1}main{max-width:28rem;margin:12vh auto;padding:1.5rem}h1{font-size:1.6rem}nav{display:grid;gap:.75rem}a{color:inherit;text-decoration:none;border:1px solid #453b4c;border-radius:.75rem;padding:1rem;background:#2b2730}a:focus,a:hover{border-color:#a4d3bc}</style><main><h1>Sign in to LocalBase</h1><nav>${links}</nav></main></html>`,
    {
      headers: {
        "cache-control": "no-store",
        "content-security-policy":
          "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
        "content-type": "text/html; charset=utf-8",
        "referrer-policy": "no-referrer",
        "x-content-type-options": "nosniff",
      },
    },
  );
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
    throw new Error("Identity provider response exceeded its size limit.");
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
        throw new Error("Identity provider response exceeded its size limit.");
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

export function createDirectSessionManager({
  provider,
  origin,
  keyResolver,
  fetcher = fetch,
  now = Date.now,
}: {
  provider: DirectAccessProvider;
  origin: string;
  keyResolver?: JWTVerifyGetKey;
  fetcher?: Fetcher;
  now?: () => number;
}) {
  const loginStates = new Map<string, LoginState>();
  const sessions = new Map<string, BrowserSession>();
  const runtimes = new Map(
    provider.registrations
      .filter((registration) => registration.kind === "oidc")
      .map((registration) => [
        registration.id,
        {
          registration,
          metadataPromise: null as Promise<Metadata> | null,
          keys: keyResolver ?? null,
        },
      ]),
  );

  const metadata = async (runtime: {
    registration: OidcAccessRegistration;
    metadataPromise: Promise<Metadata> | null;
    keys: JWTVerifyGetKey | null;
  }): Promise<Metadata> => {
    if (runtime.metadataPromise) return runtime.metadataPromise;
    runtime.metadataPromise = (async () => {
      const response = await fetcher(
        discoveryUrl(runtime.registration.issuer),
        {
          headers: { accept: "application/json" },
          redirect: "error",
          signal: AbortSignal.timeout(5_000),
        },
      );
      if (!response.ok) throw new Error("OpenID Connect discovery failed.");
      const value = metadataSchema.parse(await boundedJson(response));
      if (
        value.issuer !== runtime.registration.issuer ||
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
        runtime.registration.clientAuthentication.kind === "client-secret-basic"
          ? "client_secret_basic"
          : "none";
      const supportedAuthentication =
        value.token_endpoint_auth_methods_supported ?? ["client_secret_basic"];
      if (!supportedAuthentication.includes(authentication))
        throw new Error(
          "OpenID Connect token endpoint authentication is incompatible.",
        );
      runtime.keys ??= createRemoteJWKSet(new URL(value.jwks_uri), {
        timeoutDuration: 5_000,
        cooldownDuration: 30_000,
        cacheMaxAge: 600_000,
      });
      return value;
    })().catch((error: unknown) => {
      runtime.metadataPromise = null;
      throw error;
    });
    return runtime.metadataPromise;
  };

  const exchange = async (
    code: string,
    state: Extract<LoginState, { kind: "oidc" }>,
    request: Request,
  ): Promise<BrowserSession> => {
    const runtime = runtimes.get(state.registrationId);
    if (!runtime)
      throw new Error("OpenID Connect registration is unavailable.");
    const { registration } = runtime;
    const discovered = await metadata(runtime);
    const body = new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: `${origin}${oidcCallbackPath}`,
      code_verifier: state.verifier,
    });
    const headers = new Headers({
      accept: "application/json",
      "content-type": "application/x-www-form-urlencoded",
    });
    if (registration.clientAuthentication.kind === "none") {
      body.set("client_id", registration.clientId);
    } else {
      const basic = Buffer.from(
        `${formComponent(registration.clientId)}:${formComponent(registration.clientAuthentication.clientSecret)}`,
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
    if (!runtime.keys) throw new Error("OpenID Connect keys are unavailable.");
    const algorithms = discovered.id_token_signing_alg_values_supported.filter(
      (algorithm): algorithm is (typeof allowedAlgorithms)[number] =>
        allowedAlgorithms.includes(
          algorithm as (typeof allowedAlgorithms)[number],
        ),
    );
    const { payload } = await jwtVerify(token.id_token, runtime.keys, {
      algorithms,
      issuer: registration.issuer,
      audience: registration.clientId,
      requiredClaims: ["exp", "iat", "sub", "nonce"],
    });
    const identity = identitySchema.parse(payload);
    if (!equalTokens(identity.nonce, state.nonce))
      throw new Error("OpenID Connect nonce did not match.");
    if (
      Array.isArray(identity.aud) && identity.aud.length > 1
        ? identity.azp !== registration.clientId
        : identity.azp !== undefined && identity.azp !== registration.clientId
    )
      throw new Error("OpenID Connect authorized party did not match.");
    const verifiedEmail =
      identity.email_verified === true
        ? z.string().email().safeParse(identity.email).data
        : undefined;
    return {
      ownerId: `browser:${new Bun.CryptoHasher("sha256")
        .update(
          JSON.stringify([
            registration.kind,
            registration.issuer,
            identity.sub,
          ]),
        )
        .digest("hex")}`,
      identity: {
        issuer: registration.issuer,
        subject: identity.sub,
        ...(verifiedEmail ? { verifiedEmail } : {}),
      },
      expiresAt: Math.min(identity.exp * 1_000, now() + sessionTtlMs),
    };
  };

  const exchangeGithub = async (
    code: string,
    registration: GithubAccessRegistration,
    request: Request,
  ): Promise<BrowserSession> => {
    const signal = AbortSignal.any([
      request.signal,
      AbortSignal.timeout(5_000),
    ]);
    const tokenResponse = await fetcher(githubTokenEndpoint, {
      method: "POST",
      headers: {
        accept: "application/json",
        "content-type": "application/x-www-form-urlencoded",
      },
      body: new URLSearchParams({
        client_id: registration.clientId,
        client_secret: registration.clientSecret,
        code,
        redirect_uri: `${origin}${githubCallbackPath}`,
      }),
      redirect: "error",
      signal,
    });
    if (!tokenResponse.ok) throw new Error("GitHub token exchange failed.");
    const token = githubTokenResponseSchema.parse(
      await boundedJson(tokenResponse),
    );
    const headers = {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token.access_token}`,
      "user-agent": "LocalBase",
    };
    const [userResponse, emailsResponse] = await Promise.all([
      fetcher(githubUserEndpoint, { headers, redirect: "error", signal }),
      fetcher(githubEmailsEndpoint, { headers, redirect: "error", signal }),
    ]);
    if (!userResponse.ok || !emailsResponse.ok)
      throw new Error("GitHub identity lookup failed.");
    const user = githubUserSchema.parse(await boundedJson(userResponse));
    const emails = z
      .array(githubEmailSchema)
      .max(1_000)
      .parse(await boundedJson(emailsResponse));
    const verifiedEmail = emails.find(
      (email) => email.primary && email.verified,
    )?.email;
    return {
      ownerId: `browser:${new Bun.CryptoHasher("sha256")
        .update(JSON.stringify([registration.kind, githubIssuer, user.id]))
        .digest("hex")}`,
      identity: {
        issuer: githubIssuer,
        subject: String(user.id),
        ...(verifiedEmail ? { verifiedEmail } : {}),
      },
      expiresAt: now() + sessionTtlMs,
    };
  };

  return {
    async startLogin(registrationId: string | null): Promise<Response> {
      if (registrationId === null && provider.registrations.length > 1)
        return providerChoice(provider);
      const registration =
        registrationId === null
          ? provider.registrations[0]
          : provider.registrations.find(
              (registration) => registration.id === registrationId,
            );
      if (!registration)
        return new Response("Identity provider not found.", {
          status: 404,
          headers: { "cache-control": "no-store" },
        });
      const state = randomToken();
      if (registration.kind === "github-oauth") {
        prune(loginStates, now(), maximumLoginStates);
        loginStates.set(state, {
          kind: registration.kind,
          registrationId: registration.id,
          expiresAt: now() + loginStateTtlMs,
        });
        const authorization = new URL(githubAuthorizationEndpoint);
        for (const [key, value] of Object.entries({
          client_id: registration.clientId,
          redirect_uri: `${origin}${githubCallbackPath}`,
          scope: "read:user user:email",
          state,
        }))
          authorization.searchParams.set(key, value);
        return redirect(authorization.href, [
          secureCookie(
            loginStateCookie,
            state,
            Math.floor(loginStateTtlMs / 1_000),
          ),
        ]);
      }
      const oidcRuntime = runtimes.get(registration.id);
      if (!oidcRuntime)
        throw new Error("OpenID Connect registration is unavailable.");
      const discovered = await metadata(oidcRuntime);
      const verifier = randomToken(64);
      const nonce = randomToken();
      prune(loginStates, now(), maximumLoginStates);
      loginStates.set(state, {
        kind: registration.kind,
        registrationId: registration.id,
        verifier,
        nonce,
        expiresAt: now() + loginStateTtlMs,
      });
      const authorization = new URL(discovered.authorization_endpoint);
      for (const [key, value] of Object.entries({
        response_type: "code",
        client_id: registration.clientId,
        redirect_uri: `${origin}${oidcCallbackPath}`,
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
        const registration = provider.registrations.find(
          (candidate) => candidate.id === state.registrationId,
        );
        if (!registration || registration.kind !== state.kind) return failed();
        if (
          (state.kind === "oidc" && url.pathname !== oidcCallbackPath) ||
          (state.kind === "github-oauth" && url.pathname !== githubCallbackPath)
        )
          return failed();
        let identity: BrowserSession;
        if (state.kind === "oidc") {
          if (registration.kind !== "oidc") return failed();
          identity = await exchange(code, state, request);
        } else {
          if (registration.kind !== "github-oauth") return failed();
          identity = await exchangeGithub(code, registration, request);
        }
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
