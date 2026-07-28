import { expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { mkdir, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  createAppContext,
  parseEnvironmentOverrides,
  resolveEffectiveRoot,
} from "./context";
import { withRootOperation } from "./domains/service/ownership";
import {
  ACTIVE_LOG_FILENAME,
  bootstrapDiagnosticPath,
  logDirectory,
  logEventSchema,
  readLogSnapshot,
} from "./domains/observability/logging";
import { ensureLocalBaseRootMarker } from "./utils/root";

test("validates environment overrides without mutating process state", () => {
  expect(() =>
    parseEnvironmentOverrides({ LOCALBASE_PORT: "not-a-port" }),
  ).toThrow("LOCALBASE_PORT: LOCALBASE_PORT must be an integer");

  expect(
    parseEnvironmentOverrides({
      LOCALBASE_HOST: "127.0.0.1",
      LOCALBASE_PORT: "2274",
      LOCALBASE_STT_HOST: "localhost",
      LOCALBASE_STT_PORT: "8080",
      LOCALBASE_CTX_SIZE: "8192",
    }),
  ).toEqual({
    host: "127.0.0.1",
    port: 2274,
    sttHost: "localhost",
    sttPort: 8080,
    ctxSize: 8192,
  });
});

test("resolves data roots with CLI, environment, and configured precedence", () => {
  expect(
    resolveEffectiveRoot("/tmp/cli", "/tmp/environment", "/tmp/configured"),
  ).toBe("/tmp/cli");
  expect(
    resolveEffectiveRoot(undefined, "/tmp/environment", "/tmp/configured"),
  ).toBe("/tmp/environment");
  expect(resolveEffectiveRoot(undefined, undefined, "/tmp/configured")).toBe(
    "/tmp/configured",
  );
});

test("serve context initialization waits for the external root operation lock", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-context-lock-"));
  const root = join(directory, "root");
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  const holding = withRootOperation(root, "reset", async () => await blocked);

  try {
    await Bun.sleep(25);
    const creating = createAppContext(
      { root, nonInteractive: false, json: false },
      true,
      true,
    );
    await Bun.sleep(50);
    expect(existsSync(root)).toBe(false);
    release();
    await holding;
    const context = await creating;
    expect(existsSync(join(root, "local-base.db"))).toBe(true);
    await context.initializationOperation?.release();
    context.database.close();
  } finally {
    release();
    await holding;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("managed pre-context failures leave one bounded stable bootstrap record", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-bootstrap-"));
  try {
    for (const failure of ["logger", "database", "environment"] as const) {
      const root = join(directory, failure);
      ensureLocalBaseRootMarker(root);
      if (failure === "logger") {
        await mkdir(logDirectory(root), { mode: 0o700 });
        const target = join(directory, "outside.log");
        await writeFile(target, "");
        await symlink(target, join(logDirectory(root), ACTIVE_LOG_FILENAME));
      } else {
        await writeFile(join(root, "local-base.db"), "not sqlite");
      }

      await expect(
        createAppContext(
          { root, nonInteractive: false, json: false },
          true,
          true,
          {
            ...process.env,
            LOCALBASE_SERVICE_TOKEN: crypto.randomUUID(),
            ...(failure === "environment"
              ? { LOCALBASE_PORT: "not-a-port" }
              : {}),
          },
        ),
      ).rejects.toThrow();
      const raw = await Bun.file(bootstrapDiagnosticPath(root)).text();
      expect(Buffer.byteLength(raw)).toBeLessThan(256 * 1024);
      const record = logEventSchema.parse(JSON.parse(raw));
      expect(record.eventName).toBe("gateway.bootstrap-failed");
      expect(await readLogSnapshot(root)).toContainEqual(record);
      expect(await readLogSnapshot(root)).toContainEqual(record);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
