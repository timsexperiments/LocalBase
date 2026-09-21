import { expect, test } from "bun:test";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  browserAccessConfigPath,
  defaultBrowserPermissions,
  disableBrowserAccess,
  loadBrowserAccessConfig,
  removeOidcRegistration,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
  upsertOidcRegistration,
} from "./browser-access";

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

test("upserts and removes named OIDC registrations", () => {
  const first = upsertOidcRegistration(null, {
    registration: {
      id: "google",
      name: "Google",
      issuer: "https://accounts.google.com",
      clientId: "google-client",
      clientAuthentication: { kind: "none" },
    },
    origin: "https://localbase.example.com",
    permissions: defaultBrowserPermissions,
  });
  const second = upsertOidcRegistration(first, {
    registration: {
      id: "microsoft",
      name: "Microsoft",
      issuer: "https://login.microsoftonline.com/common/v2.0",
      clientId: "microsoft-client",
      clientAuthentication: { kind: "none" },
    },
    origin: first.origin,
    permissions: first.permissions,
  });
  expect(
    second.provider.kind === "oidc"
      ? second.provider.registrations.map(({ id }) => id)
      : [],
  ).toEqual(["google", "microsoft"]);

  const removed = removeOidcRegistration(second, "google");
  expect(removed).toMatchObject({
    kind: "configured",
    config: { provider: { registrations: [{ id: "microsoft" }] } },
  });
  if (removed.kind !== "configured") throw new Error("Expected a config.");
  expect(removeOidcRegistration(removed.config, "microsoft")).toEqual({
    kind: "disabled",
  });
  expect(removeOidcRegistration(second, "unknown")).toEqual({
    kind: "not-found",
  });
});

test("persists OIDC credentials privately and redacts command output", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-browser-access-"));
  const clientSecret = "private-oidc-secret";
  const config = {
    provider: {
      kind: "oidc" as const,
      registrations: [
        {
          id: "primary",
          name: "Primary",
          issuer: "https://identity.example.com/tenant",
          clientId: "localbase-client",
          clientAuthentication: {
            kind: "client-secret-basic" as const,
            clientSecret,
          },
        },
      ],
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
        registrations: [
          {
            ...config.provider.registrations[0],
            clientAuthentication: "client-secret-basic",
          },
        ],
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
