import { expect, test } from "bun:test";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  browserAccessConfigPath,
  defaultBrowserPermissions,
  disableBrowserAccess,
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
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
