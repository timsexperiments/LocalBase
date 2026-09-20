import { Database } from "bun:sqlite";
import { acknowledgeStaticConfiguration, restartPending } from "./activation";
import { persistConfiguration } from "./declarative";
import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { sql } from "drizzle-orm";
import { DatabaseSession, databasePath } from "../../db/client";
import {
  defaultConfig,
  loadConfig,
  readConfig,
  saveConfig,
} from "../../manager";
import { withRootOperation } from "../service/ownership";
import { type ServiceInspection } from "../service/manager";
import { applyConfiguration } from "./apply";
import { configurationDocument } from "./declarative";

const directories: string[] = [];
let previousRuntimeDirectory: string | undefined;
beforeEach(() => {
  previousRuntimeDirectory = process.env.XDG_RUNTIME_DIR;
  const directory = mkdtempSync(join(tmpdir(), "localbase-config-lock-"));
  directories.push(directory);
  process.env.XDG_RUNTIME_DIR = directory;
});
afterEach(() => {
  if (previousRuntimeDirectory === undefined)
    delete process.env.XDG_RUNTIME_DIR;
  else process.env.XDG_RUNTIME_DIR = previousRuntimeDirectory;
});
afterEach(() => {
  for (const directory of directories.splice(0))
    rmSync(directory, { recursive: true, force: true });
});
function fixture(initialize = true) {
  const root = mkdtempSync(join(tmpdir(), "localbase-config-apply-"));
  directories.push(root);
  const config = defaultConfig(root);
  config.hfToken = "preserved-secret";
  config.otelHeaders = "authorization=preserved-header";
  if (initialize) {
    const database = new DatabaseSession();
    saveConfig(database, config);
    database.close();
  }
  return { root: config.root, desired: configurationDocument(config) };
}

function serviceFixture(
  state: ServiceInspection["service"]["state"] = "running",
) {
  const calls: string[] = [];
  const inspection: ServiceInspection = {
    service: {
      state,
      manager: "launchd",
      serviceId: "test",
      root: "/tmp/test",
      definitionPath: "/tmp/test.plist",
      definitionInstalled: true,
      enabled: true,
      managerAvailable: true,
      managerState: "running",
      pid: null,
      uptimeSeconds: null,
      restartCount: null,
    },
    gateway: { state: "not_ready", detail: "test" },
  };
  return {
    calls,
    async inspect() {
      calls.push("inspect");
      return inspection;
    },
    async restart(root: string) {
      calls.push("restart");
      const database = new DatabaseSession();
      try {
        acknowledgeStaticConfiguration(database, root, await readConfig(root));
      } finally {
        database.close();
      }
      return inspection;
    },
    async wait() {
      calls.push("wait");
    },
  };
}

test("apply is idempotent, preserves secrets, and validates before initializing a database", async () => {
  const { root, desired } = fixture();
  desired.runtime.ctxSize = 8192;
  const services = serviceFixture();
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "auto", wait: true },
      services,
    ),
  ).toMatchObject({ changed: true, activation: "hot", readiness: "ready" });
  expect(services.calls).toEqual(["inspect", "wait"]);
  const saved = await readConfig(root);
  expect(saved.ctxSize).toBe(8192);
  expect(saved.hfToken).toBe("preserved-secret");
  expect(saved.otelHeaders).toBe("authorization=preserved-header");
  const hash = Bun.hash(await Bun.file(databasePath(root)).arrayBuffer());
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "auto", wait: false },
      services,
    ),
  ).toMatchObject({ changed: false, activation: "unchanged" });
  expect(Bun.hash(await Bun.file(databasePath(root)).arrayBuffer())).toBe(hash);
  const fresh = fixture(false);
  fresh.desired.gateway.port = 0;
  await expect(
    applyConfiguration(
      fresh.root,
      fresh.desired,
      { restart: "never", wait: false },
      services,
    ),
  ).rejects.toThrow();
  expect(existsSync(databasePath(fresh.root))).toBe(false);
});

test("never initializes a fresh root without requiring a service manager", async () => {
  const { root, desired } = fixture(false);
  const services = serviceFixture("unknown");
  const result = await applyConfiguration(
    root,
    desired,
    { restart: "never", wait: false },
    services,
  );
  expect(result).toMatchObject({
    changed: true,
    activation: "restart-required",
  });
  expect(services.calls).toEqual([]);
  expect(configurationDocument(await readConfig(root))).toEqual(desired);
});

test("operation lock serializes read, diff, and mutation against other root operations", async () => {
  const { root, desired } = fixture();
  let applying: ReturnType<typeof applyConfiguration> | undefined;
  await withRootOperation(root, "test", async () => {
    applying = applyConfiguration(root, desired, {
      restart: "never",
      wait: false,
    });
    const database = new DatabaseSession();
    const latest = loadConfig(database, root);
    latest.ctxSize = 8192;
    saveConfig(database, latest);
    database.close();
  });
  const result = await applying;
  expect(result?.changes).toEqual([
    {
      path: "runtime.ctxSize",
      before: 8192,
      after: desired.runtime.ctxSize,
      activation: "hot",
    },
  ]);
});

test("database failure rolls back all settings and does not invoke restart", async () => {
  const { root, desired } = fixture();
  const database = new DatabaseSession();
  database
    .get(root)
    .run(
      sql`CREATE TRIGGER reject_config BEFORE UPDATE ON config BEGIN SELECT RAISE(ABORT, 'test transaction failure'); END`,
    );
  desired.gateway.port += 1;
  desired.runtime.ctxSize = 8192;
  const services = serviceFixture();
  try {
    await expect(
      applyConfiguration(
        root,
        desired,
        { restart: "auto", wait: true },
        services,
      ),
    ).rejects.toThrow("test transaction failure");
    expect(await readConfig(root)).toMatchObject({
      ctxSize: 131072,
      gatewayPort: 2273,
    });
    expect(services.calls).toEqual(["inspect"]);
  } finally {
    database.close();
  }
});

test.each(["auto", "always", "never"] as const)(
  "restart policy %s classifies static changes",
  async (restart) => {
    const { root, desired } = fixture();
    desired.memory.systemReserve.percent = 20;
    const services = serviceFixture();
    const result = await applyConfiguration(
      root,
      desired,
      { restart, wait: restart !== "never" },
      services,
    );
    expect(result.activation).toBe(
      restart === "never" ? "restart-required" : "restarted",
    );
    expect(services.calls).toEqual(
      restart === "never" ? [] : ["inspect", "restart", "wait"],
    );
  },
);

test("auto starts a stopped managed service; always restarts even without config changes", async () => {
  const { root, desired } = fixture();
  const services = serviceFixture("stopped");
  desired.gateway.port += 1;
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "auto", wait: false },
      services,
    ),
  ).toMatchObject({ activation: "restarted", pendingRestart: false });
  expect(services.calls).toEqual(["inspect", "restart"]);
  services.calls.length = 0;
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "always", wait: true },
      services,
    ),
  ).toMatchObject({ changed: false, activation: "restarted" });
  expect(services.calls).toEqual(["inspect", "restart", "wait"]);
});

test.each(["foreground", "unknown", "stopping"] as const)(
  "auto refuses unsafe %s restart before mutation",
  async (state) => {
    const { root, desired } = fixture();
    desired.gateway.port += 1;
    const services = serviceFixture(state);
    await expect(
      applyConfiguration(
        root,
        desired,
        { restart: "auto", wait: false },
        services,
      ),
    ).rejects.toThrow("Cannot automatically restart");
    expect((await readConfig(root)).gatewayPort).toBe(2273);
  },
);

test("wait rejects deferred restart changes before saving and reports activation failures after saving", async () => {
  const { root, desired } = fixture();
  desired.gateway.port += 1;
  const services = serviceFixture();
  await expect(
    applyConfiguration(
      root,
      desired,
      { restart: "never", wait: true },
      services,
    ),
  ).rejects.toThrow("--wait requires");
  expect((await readConfig(root)).gatewayPort).toBe(2273);
  services.wait = async () => {
    throw new Error("not ready");
  };
  await expect(
    applyConfiguration(
      root,
      desired,
      { restart: "auto", wait: true },
      services,
    ),
  ).rejects.toThrow("Configuration is saved, but activation failed");
  expect((await readConfig(root)).gatewayPort).toBe(desired.gateway.port);
  expect(await restartPending(root)).toBe(true);
});

test("a deferred restart survives no-op applies and is consumed by a later auto apply", async () => {
  const { root, desired } = fixture();
  desired.memory.systemReserve.minimumGb = 10;
  const services = serviceFixture();
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "never", wait: false },
      services,
    ),
  ).toMatchObject({ pendingRestart: true });
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "never", wait: false },
      services,
    ),
  ).toMatchObject({
    changed: false,
    restartRequired: true,
    pendingRestart: true,
  });
  expect(
    await applyConfiguration(
      root,
      desired,
      { restart: "auto", wait: true },
      services,
    ),
  ).toMatchObject({
    changed: false,
    activation: "restarted",
    pendingRestart: false,
  });
  expect(await restartPending(root)).toBe(false);
});

test("failed restart keeps pending state for a separate retry", async () => {
  const { root, desired } = fixture();
  desired.gateway.port += 1;
  const services = serviceFixture();
  services.restart = async () => {
    throw new Error("manager failed");
  };
  await expect(
    applyConfiguration(
      root,
      desired,
      { restart: "auto", wait: false },
      services,
    ),
  ).rejects.toThrow("manager failed");
  expect(await restartPending(root)).toBe(true);
  const retry = await applyConfiguration(
    root,
    desired,
    { restart: "auto", wait: true },
    serviceFixture("failed"),
  );
  expect(retry).toMatchObject({
    changed: false,
    activation: "restarted",
    pendingRestart: false,
  });
});

test("startup acknowledgements cannot clear a newer marker or mismatched saved settings", async () => {
  const { root } = fixture();
  const database = new DatabaseSession();
  const started = await readConfig(root);
  const first = { ...started, gatewayPort: started.gatewayPort + 1 };
  const second = { ...started, gatewayPort: started.gatewayPort + 2 };
  try {
    persistConfiguration(database, first);
    persistConfiguration(database, second);
    acknowledgeStaticConfiguration(database, root, first);
    expect(await restartPending(root)).toBe(true);
    acknowledgeStaticConfiguration(database, root, started);
    expect(await restartPending(root)).toBe(true);
    acknowledgeStaticConfiguration(database, root, second);
    expect(await restartPending(root)).toBe(false);
    persistConfiguration(database, first);
    saveConfig(database, second);
    acknowledgeStaticConfiguration(database, root, first);
    expect(await restartPending(root)).toBe(true);
  } finally {
    database.close();
  }
});

test("apply migrates the prior database under the operation lock and preserves private settings", async () => {
  const { root, desired } = fixture();
  const old = new Database(databasePath(root));
  try {
    old.exec("DROP TABLE config_activation");
    old.exec("ALTER TABLE config DROP COLUMN gateway_host");
    old.exec("ALTER TABLE config DROP COLUMN gateway_port");
    old.exec(
      "DELETE FROM __drizzle_migrations WHERE created_at = (SELECT MAX(created_at) FROM __drizzle_migrations)",
    );
  } finally {
    old.close();
  }
  desired.gateway.port += 1;
  await applyConfiguration(root, desired, { restart: "never", wait: false });
  expect(await readConfig(root)).toMatchObject({
    gatewayPort: desired.gateway.port,
    hfToken: "preserved-secret",
    otelHeaders: "authorization=preserved-header",
  });
  expect(await restartPending(root)).toBe(true);
});
