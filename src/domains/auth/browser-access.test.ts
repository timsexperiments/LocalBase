import { expect, test } from "bun:test";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  browserAccessConfigPath,
  browserAccessPolicySchema,
  defaultBrowserPermissions,
  disableBrowserAccess,
  evaluateBrowserAccessPolicy,
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
} from "./browser-access";

const policy = browserAccessPolicySchema.parse({
  roles: {
    admin: ["access:manage", "models:manage"],
    reader: ["models:read", "access:manage"],
    chat: ["inference:chat"],
  },
  bindings: [
    {
      kind: "subject",
      role: "admin",
      issuer: "https://identity.example.com/tenant",
      subject: "person-one",
    },
    { kind: "email", role: "reader", email: "person@example.com" },
    { kind: "email-domain", role: "chat", domain: "example.com" },
  ],
});

const policyConfig = {
  provider: {
    kind: "oidc" as const,
    issuer: "https://identity.example.com/tenant",
    clientId: "localbase-client",
    clientAuthentication: { kind: "none" as const },
  },
  origin: "https://localbase.example.com",
  permissions: defaultBrowserPermissions,
  policy,
};

test("evaluates exact bindings with deterministic role and permission unions", () => {
  expect(
    evaluateBrowserAccessPolicy({
      config: policyConfig,
      identity: {
        issuer: policyConfig.provider.issuer,
        subject: "person-one",
        email: "PERSON@example.com",
      },
    }),
  ).toEqual({
    matchedRoles: ["admin", "chat", "reader"],
    permissions: [
      "inference:chat",
      "models:read",
      "models:manage",
      "access:manage",
    ],
  });
  expect(
    evaluateBrowserAccessPolicy({
      config: policyConfig,
      identity: {
        issuer: policyConfig.provider.issuer,
        subject: "person-one ",
        email: "person@example.net",
      },
    }),
  ).toEqual({ matchedRoles: [], permissions: [] });
  expect(
    evaluateBrowserAccessPolicy({
      config: policyConfig,
      identity: {
        issuer: policyConfig.provider.issuer,
        subject: "person-two",
      },
    }),
  ).toEqual({ matchedRoles: [], permissions: [] });
});

test("rejects policy bindings that reference unknown roles", () => {
  expect(
    browserAccessPolicySchema.safeParse({
      roles: { admin: ["access:manage"] },
      bindings: [
        {
          kind: "subject",
          role: "missing",
          issuer: "https://identity.example.com",
          subject: "person-one",
        },
      ],
    }).success,
  ).toBe(false);
});

test("persists strict browser access configuration atomically", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-browser-access-"));
  const config = {
    provider: {
      kind: "cloudflare-access" as const,
      teamDomain: "team.cloudflareaccess.com",
      audience: "audience",
    },
    origin: "https://localbase.example.com",
    permissions: defaultBrowserPermissions,
  };
  try {
    expect(await loadBrowserAccessConfig(root)).toBeNull();
    expect(await saveBrowserAccessConfig(root, config)).toEqual(config);
    expect(await loadBrowserAccessConfig(root)).toEqual(config);
    expect((await stat(browserAccessConfigPath(root))).mode & 0o777).toBe(
      0o600,
    );
    expect(await readFile(browserAccessConfigPath(root), "utf8")).not.toContain(
      "lb_",
    );
    expect(await disableBrowserAccess(root)).toBe(true);
    expect(await disableBrowserAccess(root)).toBe(false);
    expect(await loadBrowserAccessConfig(root)).toBeNull();
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("persists OIDC credentials privately and redacts command output", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-browser-access-"));
  const clientSecret = "private-oidc-secret";
  const config = {
    provider: {
      kind: "oidc" as const,
      issuer: "https://identity.example.com/tenant",
      clientId: "localbase-client",
      clientAuthentication: {
        kind: "client-secret-basic" as const,
        clientSecret,
      },
    },
    origin: "https://localbase.example.com",
    permissions: defaultBrowserPermissions,
  };
  try {
    await saveBrowserAccessConfig(root, config);
    expect(await loadBrowserAccessConfig(root)).toEqual(config);
    expect((await stat(browserAccessConfigPath(root))).mode & 0o777).toBe(
      0o600,
    );
    expect(summarizeBrowserAccessConfig(config)).toEqual({
      ...config,
      provider: {
        kind: "oidc",
        issuer: config.provider.issuer,
        clientId: config.provider.clientId,
        clientAuthentication: "client-secret-basic",
      },
    });
    expect(JSON.stringify(summarizeBrowserAccessConfig(config))).not.toContain(
      clientSecret,
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("rejects malformed provider, origin, and permission configuration", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-browser-access-"));
  try {
    for (const input of [
      "{",
      "null",
      JSON.stringify({}),
      JSON.stringify({
        provider: {
          kind: "cloudflare-access",
          teamDomain: "evil.example.com",
          audience: "audience",
        },
        origin: "http://localbase.example.com",
        permissions: ["models:write"],
      }),
    ]) {
      await Bun.write(browserAccessConfigPath(root), input);
      await expect(loadBrowserAccessConfig(root)).rejects.toThrow(
        "Invalid ui-access.json",
      );
    }
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
