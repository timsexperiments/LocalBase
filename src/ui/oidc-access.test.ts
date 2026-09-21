import { beforeAll, expect, test } from "bun:test";
import { createLocalJWKSet, exportJWK, generateKeyPair, SignJWT } from "jose";
import { defaultBrowserPermissions } from "../domains/auth/browser-access";
import { createUiAccess, uiAccessConfigSchema } from "./access";

const issuer = "https://identity.example.com/tenant";
const origin = "https://ui.example.com";
const clientId = "localbase-client";
const config = uiAccessConfigSchema.parse({
  provider: {
    kind: "oidc",
    issuer,
    clientId,
    clientAuthentication: {
      kind: "client-secret-basic",
      clientSecret: "secret",
    },
  },
  origin,
  permissions: defaultBrowserPermissions,
});

let keys: Awaited<ReturnType<typeof generateKeyPair>>;
let resolver: ReturnType<typeof createLocalJWKSet>;

beforeAll(async () => {
  keys = await generateKeyPair("RS256");
  resolver = createLocalJWKSet({
    keys: [{ ...(await exportJWK(keys.publicKey)), kid: "oidc", alg: "RS256" }],
  });
});

function directRequest(
  path: string,
  options: { method?: string; cookie?: string; marker?: boolean } = {},
): Request {
  const headers = new Headers();
  if (options.cookie) headers.set("cookie", options.cookie);
  if (options.marker) {
    headers.set("x-localbase-ui", "1");
    headers.set("origin", origin);
    headers.set("sec-fetch-site", "same-origin");
  }
  return new Request(`${origin}${path}`, {
    method: options.method ?? "GET",
    headers,
  });
}

async function responseFor(
  access: ReturnType<typeof createUiAccess>,
  request: Request,
): Promise<Response> {
  const result = await access.handle(request);
  if (result.kind !== "response") throw new Error("Expected a response.");
  return result.response;
}

function cookieFrom(response: Response, name: string): string {
  const value = response.headers
    .getSetCookie()
    .find((cookie) => cookie.startsWith(`${name}=`));
  if (!value) throw new Error(`Missing ${name} cookie.`);
  return value.split(";", 1)[0] ?? "";
}

function discovery() {
  return {
    issuer,
    authorization_endpoint: `${issuer}/authorize?provider=fixed`,
    token_endpoint: `${issuer}/token`,
    jwks_uri: `${issuer}/keys`,
    response_types_supported: ["code"],
    code_challenge_methods_supported: ["S256"],
    id_token_signing_alg_values_supported: ["RS256"],
    token_endpoint_auth_methods_supported: ["client_secret_basic"],
  };
}

test("completes an OIDC code flow and keeps the opaque session server-side", async () => {
  let nonce = "";
  let tokenRequests = 0;
  const fetcher = async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.includes(".well-known")) {
      expect(url).toBe(`${issuer}/.well-known/openid-configuration`);
      return Response.json(discovery());
    }
    expect(url).toBe(`${issuer}/token`);
    tokenRequests += 1;
    expect(init?.method).toBe("POST");
    const headers = new Headers(init?.headers);
    expect(headers.get("authorization")).toBe(
      `Basic ${Buffer.from("localbase-client:secret").toString("base64")}`,
    );
    const body = new URLSearchParams(String(init?.body));
    expect(body.get("grant_type")).toBe("authorization_code");
    expect(body.get("code")).toBe("authorization-code");
    expect(body.get("redirect_uri")).toBe(`${origin}/app/callback`);
    expect(body.get("code_verifier")).toMatch(/^[A-Za-z0-9_-]{80,128}$/);
    return Response.json({
      id_token: await new SignJWT({
        iss: issuer,
        aud: clientId,
        exp: Math.floor(Date.now() / 1_000) + 300,
        sub: "person-one",
        nonce,
      })
        .setProtectedHeader({ alg: "RS256", kid: "oidc" })
        .setIssuedAt()
        .sign(keys.privateKey),
    });
  };
  const access = createUiAccess({ config, keyResolver: resolver, fetcher });

  const login = await responseFor(access, directRequest("/app/login"));
  expect(login.status).toBe(303);
  const authorization = new URL(login.headers.get("location") ?? "");
  expect(authorization.origin + authorization.pathname).toBe(
    `${issuer}/authorize`,
  );
  expect(authorization.searchParams.get("response_type")).toBe("code");
  expect(authorization.searchParams.get("provider")).toBe("fixed");
  expect(authorization.searchParams.get("client_id")).toBe(clientId);
  expect(authorization.searchParams.get("redirect_uri")).toBe(
    `${origin}/app/callback`,
  );
  expect(authorization.searchParams.get("code_challenge_method")).toBe("S256");
  expect(authorization.searchParams.get("code_challenge")).toMatch(
    /^[A-Za-z0-9_-]{43}$/,
  );
  nonce = authorization.searchParams.get("nonce") ?? "";
  const state = authorization.searchParams.get("state") ?? "";
  const stateCookie = cookieFrom(login, "__Host-localbase-oidc-state");

  const callback = await responseFor(
    access,
    directRequest(`/app/callback?code=authorization-code&state=${state}`, {
      cookie: stateCookie,
    }),
  );
  expect(callback.status).toBe(303);
  expect(callback.headers.get("location")).toBe(`${origin}/app`);
  expect(tokenRequests).toBe(1);
  const sessionCookie = cookieFrom(callback, "__Host-localbase-session");
  expect(sessionCookie).not.toContain("person-one");

  const sessionRequest = directRequest("/app/session", {
    cookie: sessionCookie,
    marker: true,
  });
  const session = await responseFor(access, sessionRequest);
  expect(session.status).toBe(200);
  const ownerId = access.credential(sessionRequest)?.ownerId;
  expect(access.credential(sessionRequest)).toMatchObject({
    ownerId: expect.stringMatching(/^browser:[0-9a-f]{64}$/),
    permissions: defaultBrowserPermissions,
  });

  const apiRequest = directRequest("/app/api/_localbase/models", {
    cookie: sessionCookie,
    marker: true,
  });
  const forwarded = await access.handle(apiRequest);
  if (forwarded.kind !== "forward") throw new Error("Expected forwarding.");
  expect(forwarded.request.headers.has("cookie")).toBe(false);
  expect(access.credential(forwarded.request)?.ownerId).toBe(ownerId);

  const replay = await responseFor(
    access,
    directRequest(`/app/callback?code=authorization-code&state=${state}`, {
      cookie: stateCookie,
    }),
  );
  expect(replay.headers.get("location")).toBe(`${origin}/app?signin=failed`);
  expect(tokenRequests).toBe(1);
});

test("rejects unbound callbacks and incompatible discovery", async () => {
  let currentTime = Date.now();
  const incompatible = createUiAccess({
    config,
    keyResolver: resolver,
    fetcher: async () =>
      Response.json({
        ...discovery(),
        code_challenge_methods_supported: ["plain"],
      }),
  });
  expect(
    (await responseFor(incompatible, directRequest("/app/login"))).status,
  ).toBe(503);

  const publicClientConfig = uiAccessConfigSchema.parse({
    ...config,
    provider: {
      kind: "oidc",
      issuer,
      clientId,
      clientAuthentication: { kind: "none" },
    },
  });
  const omittedAuthenticationMethods = createUiAccess({
    config: publicClientConfig,
    keyResolver: resolver,
    fetcher: async () => {
      const { token_endpoint_auth_methods_supported: _, ...metadata } =
        discovery();
      return Response.json(metadata);
    },
  });
  expect(
    (
      await responseFor(
        omittedAuthenticationMethods,
        directRequest("/app/login"),
      )
    ).status,
  ).toBe(503);

  let nonce = "";
  const access = createUiAccess({
    config,
    keyResolver: resolver,
    now: () => currentTime,
    fetcher: async (input) => {
      if (String(input).includes(".well-known"))
        return Response.json(discovery());
      return Response.json({
        id_token: await new SignJWT({
          iss: issuer,
          aud: clientId,
          exp: Math.floor(currentTime / 1_000) + 60,
          sub: "person-two",
          nonce,
        })
          .setProtectedHeader({ alg: "RS256", kid: "oidc" })
          .setIssuedAt()
          .sign(keys.privateKey),
      });
    },
  });
  const login = await responseFor(access, directRequest("/app/login"));
  const authorization = new URL(login.headers.get("location") ?? "");
  nonce = authorization.searchParams.get("nonce") ?? "";
  const state = authorization.searchParams.get("state") ?? "";
  const wrongCookie = `__Host-localbase-oidc-state=${"a".repeat(43)}`;
  const rejected = await responseFor(
    access,
    directRequest(`/app/callback?code=code&state=${state}`, {
      cookie: wrongCookie,
    }),
  );
  expect(rejected.headers.get("location")).toBe(`${origin}/app?signin=failed`);
});

test("rejects ID tokens outside the exact OIDC transaction", async () => {
  for (const claims of [
    { iss: "https://other.example.com" },
    { aud: "other-client" },
    { nonce: "other-nonce" },
    { aud: [clientId, "other-client"], azp: "other-client" },
    { exp: 1 },
  ]) {
    let nonce = "";
    const access = createUiAccess({
      config,
      keyResolver: resolver,
      fetcher: async (input) => {
        if (String(input).includes(".well-known"))
          return Response.json(discovery());
        return Response.json({
          id_token: await new SignJWT({
            iss: issuer,
            aud: clientId,
            exp: Math.floor(Date.now() / 1_000) + 300,
            sub: "person-three",
            nonce,
            ...claims,
          })
            .setProtectedHeader({ alg: "RS256", kid: "oidc" })
            .setIssuedAt()
            .sign(keys.privateKey),
        });
      },
    });
    const login = await responseFor(access, directRequest("/app/login"));
    const authorization = new URL(login.headers.get("location") ?? "");
    nonce = authorization.searchParams.get("nonce") ?? "";
    const state = authorization.searchParams.get("state") ?? "";
    const callback = await responseFor(
      access,
      directRequest(`/app/callback?code=code&state=${state}`, {
        cookie: cookieFrom(login, "__Host-localbase-oidc-state"),
      }),
    );
    expect(callback.headers.get("location")).toBe(
      `${origin}/app?signin=failed`,
    );
    const session = await responseFor(
      access,
      directRequest("/app/session", { marker: true }),
    );
    expect(session.status).toBe(401);
  }
});
