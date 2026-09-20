import { beforeAll, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  createLocalJWKSet,
  exportJWK,
  generateKeyPair,
  SignJWT,
  type JWTPayload,
} from "jose";
import {
  createUiAccess,
  loadUiAccessConfig,
  uiAccessConfigSchema,
} from "./access";
import { defaultBrowserPermissions } from "../domains/auth/browser-access";
import { startGatewayFixture } from "../test/gateway-fixture";
import { VideoJobManager } from "../domains/runtime/video/video-job-manager";

const config = uiAccessConfigSchema.parse({
  provider: {
    kind: "cloudflare-access",
    teamDomain: "test-team.cloudflareaccess.com",
    audience: "test-audience",
  },
  origin: "https://ui.example.com",
  permissions: defaultBrowserPermissions,
});
const issuer = `https://${config.provider.teamDomain}`;
let keys: Awaited<ReturnType<typeof generateKeyPair>>;
let resolver: ReturnType<typeof createLocalJWKSet>;

beforeAll(async () => {
  keys = await generateKeyPair("RS256");
  resolver = createLocalJWKSet({
    keys: [{ ...(await exportJWK(keys.publicKey)), kid: "test", alg: "RS256" }],
  });
});

async function token(overrides: JWTPayload = {}) {
  return new SignJWT({
    iss: issuer,
    aud: config.provider.audience,
    exp: Math.floor(Date.now() / 1000) + 300,
    sub: "person-one",
    email: "person@example.com",
    type: "app",
    ...overrides,
  })
    .setProtectedHeader({ alg: "RS256", kid: "test" })
    .sign(keys.privateKey);
}

function request(
  jwt: string,
  path = "/app/session",
  method = "GET",
  headers: HeadersInit = {},
) {
  return new Request(`http://ui.example.com${path}`, {
    method,
    headers: {
      "x-localbase-ui": "1",
      "cf-access-jwt-assertion": jwt,
      "sec-fetch-site": "same-origin",
      origin: config.origin,
      ...headers,
    },
  });
}

async function responseFor(
  access: ReturnType<typeof createUiAccess>,
  req: Request,
) {
  const result = await access.handle(req);
  if (result.kind !== "response")
    throw new Error("Expected a direct response.");
  return result.response;
}

test("loads only strict startup configuration; missing disables and malformed fails", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-ui-access-config-"));
  try {
    expect(await loadUiAccessConfig(root)).toBeNull();
    await Bun.write(join(root, "ui-access.json"), JSON.stringify(config));
    expect(await loadUiAccessConfig(root)).toEqual(config);
    for (const invalid of [
      "{",
      JSON.stringify({ ...config, extra: true }),
      JSON.stringify({ ...config, origin: "http://ui.example.com" }),
    ]) {
      await Bun.write(join(root, "ui-access.json"), invalid);
      await expect(loadUiAccessConfig(root)).rejects.toThrow(
        "Invalid ui-access.json",
      );
    }
    for (const teamDomain of [
      "https://test.cloudflareaccess.com",
      "test.cloudflareaccess.com.evil.com",
      "evil.com",
      "test.cloudflareaccess.com/path",
      "test@team.cloudflareaccess.com",
    ]) {
      expect(
        uiAccessConfigSchema.safeParse({
          ...config,
          provider: { ...config.provider, teamDomain },
        }).success,
      ).toBe(false);
    }
    for (const origin of [
      "not-a-url",
      "https://ui.example.com/",
      "https://user@ui.example.com",
      "https://ui.example.com/path",
      "https://ui.example.com?query",
    ]) {
      expect(
        uiAccessConfigSchema.safeParse({ ...config, origin }).success,
      ).toBe(false);
    }
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("verifies every human session and never falls back after a JWT failure", async () => {
  const access = createUiAccess({ config, keyResolver: resolver });
  const validRequest = request(await token());
  const valid = await responseFor(access, validRequest);
  expect(valid.status).toBe(200);
  expect(await valid.json()).toEqual({ authenticated: true });
  expect(valid.headers.get("cache-control")).toBe("no-store");
  expect(access.credential(validRequest)).toMatchObject({
    ownerId: expect.stringMatching(/^browser:[0-9a-f]{64}$/),
    permissions: defaultBrowserPermissions,
  });
  const forgedKeys = await generateKeyPair("RS256");
  const forged = await new SignJWT({
    iss: issuer,
    aud: config.provider.audience,
    exp: 9999999999,
    sub: "person",
    email: "person@example.com",
    type: "app",
  })
    .setProtectedHeader({ alg: "RS256", kid: "test" })
    .sign(forgedKeys.privateKey);
  const hmac = await new SignJWT({
    iss: issuer,
    aud: config.provider.audience,
    exp: 9999999999,
    sub: "person",
    email: "person@example.com",
    type: "app",
  })
    .setProtectedHeader({ alg: "HS256" })
    .sign(new Uint8Array(32));
  const invalid = ["", "not-a-jwt", forged, hmac];
  for (const claims of [
    { exp: 1 },
    { exp: undefined },
    { aud: "other" },
    { iss: "https://other.cloudflareaccess.com" },
    { sub: "" },
    { sub: undefined },
    { email: undefined },
    { email: "not-an-email" },
    { email: "non_identity@test-team.cloudflareaccess.com" },
    { type: "org" },
    { type: undefined },
    { common_name: "client.access", sub: "", email: undefined },
    { common_name: "client.access" },
    { service_token_id: "service" },
    { service_token_status: true },
    { nbf: Math.floor(Date.now() / 1000) + 300 },
  ])
    invalid.push(await token(claims));
  for (const jwt of invalid) {
    const response = await responseFor(
      access,
      request(jwt, "/app/session", "GET", {
        "cf-access-authenticated-user-email": "person@example.com",
      }),
    );
    expect(response.status).toBe(401);
    expect(await response.json()).not.toHaveProperty("mode");
    expect(
      (await responseFor(access, request(jwt, "/app/api/_localbase/models")))
        .status,
    ).toBe(401);
  }
  const unavailableKeys = createUiAccess({
    config,
    keyResolver: async () => {
      throw new Error("Key lookup failed.");
    },
  });
  expect(
    (await responseFor(unavailableKeys, request(await token()))).status,
  ).toBe(401);
  const cookieOnly = request("");
  cookieOnly.headers.delete("cf-access-jwt-assertion");
  cookieOnly.headers.set("cookie", `CF_Authorization=${await token()}`);
  expect((await responseFor(access, cookieOnly)).status).toBe(401);
});

test("enforces exact host, origin, fetch-site, marker, method, and path boundaries", async () => {
  const access = createUiAccess({ config, keyResolver: resolver });
  const jwt = await token();
  const invalidHeaders: Record<string, string>[] = [
    { origin: "https://evil.example.com" },
    { origin: "null" },
    { origin: `${config.origin}/` },
    { "sec-fetch-site": "cross-site" },
    { "sec-fetch-site": "same-site" },
    { "sec-fetch-site": "none" },
    { "x-localbase-ui": "0" },
    { host: "evil.example.com" },
  ];
  for (const headers of invalidHeaders)
    expect(
      (await responseFor(access, request(jwt, "/app/session", "GET", headers)))
        .status,
    ).toBe(403);
  const wrongHost = new Request("http://evil.example.com/app/session", {
    headers: request(jwt).headers,
  });
  wrongHost.headers.set("x-forwarded-host", "ui.example.com");
  expect((await responseFor(access, wrongHost)).status).toBe(403);
  for (const method of ["POST", "DELETE"]) {
    const missingOrigin = request(jwt, "/app/api/v1/videos", method);
    missingOrigin.headers.delete("origin");
    expect((await responseFor(access, missingOrigin)).status).toBe(403);
  }
  const missingMarker = request(jwt);
  missingMarker.headers.delete("x-localbase-ui");
  expect((await responseFor(access, missingMarker)).status).toBe(403);
  for (const [method, path] of [
    ["GET", "/app/api/v1/models"],
    ["POST", "/app/api/v1/audio/translations"],
    ["GET", "/app/api/_localbase/models/model"],
    ["GET", "/app/api/_localbase/model-management/model"],
    ["POST", "/app/api/_localbase/model-management/model"],
    ["POST", "/app/api/_localbase/model-management/"],
    ["DELETE", "/app/api/_localbase/model-management"],
    ["PUT", "/app/api/_localbase/model-management"],
    ["POST", "/app/api/_localbase/models"],
    ["GET", "/app/api/health"],
    ["OPTIONS", "/app/api/v1/videos"],
    ["GET", "/app/api/v1/videos"],
    ["POST", "/app/api/v1/chat/completions/"],
    ["GET", "/app/api/v1/videos/not-a-uuid"],
  ])
    expect((await responseFor(access, request(jwt, path, method))).status).toBe(
      404,
    );
  expect(
    (await responseFor(access, request(jwt, "/app/session", "POST"))).status,
  ).toBe(405);
});

test("maps only allowlisted UI calls and keeps credentials off headers and request clones", async () => {
  const access = createUiAccess({ config, keyResolver: resolver });
  const jwt = await token();
  const id = crypto.randomUUID();
  const routes = [
    ["GET", "/_localbase/model-management"],
    ["POST", "/_localbase/model-management"],
    ["GET", "/_localbase/models"],
    ["POST", "/v1/chat/completions"],
    ["POST", "/v1/embeddings"],
    ["POST", "/v1/images/generations"],
    ["POST", "/v1/audio/speech"],
    ["POST", "/v1/audio/transcriptions"],
    ["POST", "/v1/videos"],
    ["GET", `/v1/videos/${id}`],
    ["GET", `/v1/videos/${id}/content`],
    ["POST", `/v1/videos/${id}/cancel`],
    ["DELETE", `/v1/videos/${id}`],
  ];
  for (const [method, path] of routes) {
    const incoming = request(jwt, `/app/api${path}`, method, {
      cookie: "private-cookie",
      authorization: "Bearer should-not-be-used",
      "x-api-key": "unused",
      "cf-access-client-secret": "private-secret",
      "cf-access-client-id": "service-id",
      "cf-access-authenticated-user-email": "untrusted@example.com",
    });
    const mapped = await access.handle(incoming);
    if (mapped.kind !== "forward") throw new Error("Expected mapped route.");
    expect(mapped.pathname).toBe(path);
    expect(new URL(mapped.request.url).pathname).toBe(path);
    expect(mapped.request.method).toBe(method);
    for (const name of [
      "cookie",
      "authorization",
      "x-api-key",
      "cf-access-jwt-assertion",
      "cf-access-client-id",
      "cf-access-client-secret",
      "cf-access-authenticated-user-email",
    ])
      expect(mapped.request.headers.has(name)).toBe(false);
    expect(access.credential(mapped.request)).toMatchObject({
      ownerId: expect.stringMatching(/^browser:[0-9a-f]{64}$/),
      permissions: defaultBrowserPermissions,
    });
    expect(access.credential(mapped.request.clone())).toBeUndefined();
    expect(access.credential(incoming)).toBeUndefined();
  }
  for (const path of [
    "/v1/chat/completions",
    "/_localbase/models",
    "/health/ready",
  ]) {
    const standard = request(jwt, path);
    expect(await access.handle(standard)).toEqual({ kind: "pass" });
    expect(access.credential(standard)).toBeUndefined();
  }
});

test("preserves streaming bodies and abort signals through HTTP request mapping", async () => {
  const access = createUiAccess({ config, keyResolver: resolver });
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(incoming) {
      const result = await access.handle(incoming);
      if (result.kind === "response") return result.response;
      if (result.kind !== "forward") return new Response(null, { status: 404 });
      return new Response(result.request.body, {
        headers: {
          "content-type": result.request.headers.get("content-type") ?? "",
        },
      });
    },
  });
  try {
    const form = new FormData();
    form.append("file", new Blob(["audio bytes"]), "audio.wav");
    const response = await fetch(
      `${server.url}app/api/v1/audio/transcriptions`,
      {
        method: "POST",
        body: form,
        headers: {
          ...Object.fromEntries(request(await token()).headers),
          host: "ui.example.com",
        },
      },
    );
    expect(response.status).toBe(200);
    const echoed = await response.formData();
    const file = echoed.get("file");
    expect(file instanceof File && (await file.text())).toBe("audio bytes");
    const controller = new AbortController();
    const result = await access.handle(
      new Request(
        request(await token(), "/app/api/v1/chat/completions", "POST"),
        { body: "{}", signal: controller.signal },
      ),
    );
    if (result.kind !== "forward") throw new Error("Expected mapped request.");
    controller.abort();
    expect(result.request.signal.aborted).toBe(true);
  } finally {
    server.stop(true);
  }
});

test("isolates video ownership by issuer and subject, independent of email and JWT renewal", async () => {
  const access = createUiAccess({ config, keyResolver: resolver });
  async function owner(claims: JWTPayload = {}, selected = access) {
    const mapped = await selected.handle(
      request(await token(claims), "/app/api/v1/videos", "POST"),
    );
    if (mapped.kind !== "forward") throw new Error("Expected mapped request.");
    const credential = selected.credential(mapped.request);
    if (!credential) throw new Error("Expected scoped identity.");
    return credential.ownerId;
  }
  const first = await owner();
  expect(
    await owner({
      email: "changed@example.com",
      exp: Math.floor(Date.now() / 1000) + 600,
    }),
  ).toBe(first);
  const second = await owner({ sub: "person-two" });
  expect(second).not.toBe(first);
  const otherConfig = {
    ...config,
    provider: {
      ...config.provider,
      teamDomain: "other.cloudflareaccess.com",
    },
  };
  expect(
    await owner(
      { iss: `https://${otherConfig.provider.teamDomain}` },
      createUiAccess({ config: otherConfig, keyResolver: resolver }),
    ),
  ).not.toBe(first);
  const root = await mkdtemp(join(tmpdir(), "localbase-ui-access-video-"));
  const manager = new VideoJobManager({
    temporaryDirectory: root,
    backend: {
      async submitVideo() {
        return { id: "local-job", status: "queued" };
      },
      async getJob() {
        return {
          id: "local-job",
          status: "completed",
          media: {
            bytes: new Uint8Array([1, 2]),
            mimeType: "video/x-msvideo",
            outputFormat: "avi",
            fps: 16,
            frameCount: 33,
          },
        };
      },
      async cancelJob() {
        return { id: "local-job", status: "cancelled" };
      },
    },
    acquireAdmission: async () => ({ ready: Promise.resolve(), release() {} }),
    supervisedStop: async () => {},
    onContainmentFailure() {},
  });
  try {
    const job = await manager.start({
      ownerId: first,
      jobDeadlineMs: 1000,
      input: { kind: "text", prompt: "test" },
    });
    if (job.kind !== "accepted") throw new Error("Expected job.");
    await job.terminal;
    expect(manager.get({ ownerId: first, id: job.job.id })).toBeDefined();
    expect(manager.artifact({ ownerId: first, id: job.job.id })).toBeDefined();
    expect(manager.get({ ownerId: second, id: job.job.id })).toBeUndefined();
    expect(
      manager.artifact({ ownerId: second, id: job.job.id }),
    ).toBeUndefined();
    expect(
      await manager.cancel({ ownerId: second, id: job.job.id }),
    ).toBeUndefined();
    expect(manager.delete({ ownerId: second, id: job.job.id })).toBe(false);
    expect(manager.delete({ ownerId: first, id: job.job.id })).toBe(true);
  } finally {
    await manager.shutdown();
    await rm(root, { recursive: true, force: true });
  }
});

test("disabled browser authentication denies UI sessions and Access JWTs never authenticate standard APIs", async () => {
  const gateway = await startGatewayFixture({ auth: { mode: "either" } });
  try {
    const jwt = await token();
    const headers = request(jwt).headers;
    const session = await fetch(`${gateway.baseUrl}/app/session`, { headers });
    expect(session.status).toBe(401);
    expect(await session.json()).toMatchObject({
      error: { code: "ui_access_denied" },
    });
    expect(session.headers.has("access-control-allow-origin")).toBe(false);
    for (const [method, path] of [
      ["GET", "/_localbase/models"],
      ["GET", "/_localbase/model-management"],
      ["POST", "/_localbase/model-management"],
      ["POST", "/v1/chat/completions"],
      ["GET", "/app/api/_localbase/models"],
    ]) {
      expect(
        (await fetch(`${gateway.baseUrl}${path}`, { method, headers })).status,
      ).toBe(401);
    }
    const preflight = await fetch(
      `${gateway.baseUrl}/app/api/v1/chat/completions`,
      { method: "OPTIONS", headers },
    );
    expect(preflight.headers.has("access-control-allow-origin")).toBe(false);
    expect(preflight.status).not.toBe(204);
    expect(
      (
        await fetch(`${gateway.baseUrl}/_localbase/models`, {
          headers: { authorization: `Bearer ${gateway.apiKey}` },
        })
      ).status,
    ).toBe(200);
    const managementHeaders = { authorization: `Bearer ${gateway.apiKey}` };
    const management = await fetch(
      `${gateway.baseUrl}/_localbase/model-management`,
      {
        headers: managementHeaders,
      },
    );
    expect(management.status).toBe(200);
    expect(management.headers.get("cache-control")).toBe("no-store");
    expect(await management.json()).toMatchObject({
      canManage: false,
      models: expect.any(Array),
    });
    const denied = await fetch(
      `${gateway.baseUrl}/_localbase/model-management`,
      {
        method: "POST",
        headers: {
          ...managementHeaders,
          "x-localbase-owner-id": `browser:${"a".repeat(64)}`,
        },
        body: "not JSON",
      },
    );
    expect(denied.status).toBe(403);
    expect(await denied.json()).toMatchObject({
      error: { code: "insufficient_permissions" },
    });
    const chat = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${gateway.apiKey}`,
        "content-type": "application/json",
        cookie: "CF_Authorization=private",
        "cf-access-jwt-assertion": jwt,
        "cf-access-client-secret": "private-secret",
        "cf-access-client-id": "service",
        "cf-access-authenticated-user-email": "private@example.com",
      },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "user", content: "fixture only" }],
      }),
    });
    expect(chat.status).toBe(200);
    await chat.text();
    const upstream = gateway.upstreamRequests.find(
      (entry) => entry.path === "/v1/chat/completions",
    );
    expect(upstream).toBeDefined();
    for (const name of [
      "cookie",
      "cf-access-jwt-assertion",
      "cf-access-client-secret",
      "cf-access-client-id",
      "cf-access-authenticated-user-email",
    ])
      expect(upstream?.headers.has(name)).toBe(false);
  } finally {
    await gateway.stop();
  }
}, 60_000);
