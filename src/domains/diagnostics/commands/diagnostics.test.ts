import { expect, test } from "bun:test";
import {
  mkdirSync,
  symlinkSync,
  mkdtempSync,
  rmSync,
  statSync,
  renameSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { unzipSync } from "fflate";
import { initConfig, readConfig } from "../../../manager";
import { DatabaseSession, databasePath } from "../../../db/client";
import {
  createLogger,
  diagnosticsLogEventSchema,
} from "../../observability/logging";
import { configTable } from "../../../db/schema";
import { eq } from "drizzle-orm";
import {
  openSecureDirectory,
  setSecureFilePublicationTestHooksForTests,
} from "../../../utils/secure-file-publication";
import { diagnosticsManifestSchema, runDiagnostics } from "./diagnostics";

function execution() {
  return {
    global: { json: true, nonInteractive: true },
    output: { info() {}, error() {}, lifecycle() {} },
  } as const;
}

async function fixture() {
  const root = mkdtempSync(join(tmpdir(), "localbase-diagnostics-"));
  const database = new DatabaseSession();
  initConfig(database, root, 64);
  database.close();
  return root;
}

async function archiveEntries(path: string) {
  return unzipSync(await Bun.file(path).bytes());
}

test("creates a strict decoded archive without sensitive or path data", async () => {
  const root = await fixture();
  const output = join(root, "report.zip");
  const logger = createLogger();
  try {
    const database = new DatabaseSession();
    database
      .get(root)
      .update(configTable)
      .set({
        host: "https://user:password@example.test:8443/api?token=config-secret",
        sttHost: "https://stt-user:stt-password@example.test?key=stt-secret",
        otelEndpoint: "https://otel.example.test:4318/v1/logs",
        otelHeaders: "authorization=Bearer%20config-header-secret",
      })
      .where(eq(configTable.id, "default"))
      .run();
    database.close();
    await logger.enableFileLogging(root);
    logger.event({
      severity: "error",
      eventName: "diagnostics.test",
      category: "gateway",
      component: "test",
      runtime: "gateway",
      message:
        "Authorization: Bearer direct-span-secret https://user:password@example.test/x?token=hidden",
      attributes: {
        token: "secret-value",
        prompt: "do not archive this content",
      },
      requestId: "request-secret-id",
      error: {
        type: "internal-secret-type",
        message: "prompt=secret-value",
        code: "internal-secret-code",
      },
    });
    await logger.close();

    await runDiagnostics({ output }, { logger, config: { root } }, execution());
    const entries = await archiveEntries(output);
    expect(Object.keys(entries)).toEqual([
      "manifest.json",
      "logs/events.jsonl",
    ]);

    const manifest = diagnosticsManifestSchema.parse(
      JSON.parse(new TextDecoder().decode(entries["manifest.json"])),
    );
    expect(manifest.configuration.available).toBe(true);
    expect(manifest.hardware.available).toBe(true);
    expect(manifest.models.available).toBe(true);
    const text = new TextDecoder().decode(entries["logs/events.jsonl"]);
    for (const line of text.trim().split("\n")) {
      if (line) {
        const event = diagnosticsLogEventSchema.parse(JSON.parse(line));
        expect(event).not.toHaveProperty("requestId");
        expect(event).not.toHaveProperty("trace");
        expect(event).not.toHaveProperty("http");
        expect(event.error).not.toHaveProperty("type");
        expect(event.error).not.toHaveProperty("code");
      }
    }
    const archiveBytes = await Bun.file(output).bytes();
    const archiveText = new TextDecoder().decode(archiveBytes);
    for (const bytes of Object.values(entries)) {
      const entryText = new TextDecoder().decode(bytes);
      expect(entryText).not.toContain("direct-span-secret");
      expect(entryText).not.toContain("secret-value");
      expect(entryText).not.toContain("do not archive this content");
      expect(entryText).not.toContain(root);
      expect(entryText).not.toContain("/Users/");
      expect(entryText).not.toContain("requestId");
      expect(entryText).not.toContain("traceId");
      expect(entryText).not.toContain("internal-secret-type");
      expect(entryText).not.toContain("internal-secret-code");
      expect(entryText).not.toContain("config-secret");
      expect(entryText).not.toContain("stt-secret");
      expect(entryText).not.toContain("otel-secret");
      expect(entryText).not.toContain("config-header-secret");
    }
    expect(archiveText).not.toContain("direct-span-secret");
    expect(archiveText).not.toContain("secret-value");
    expect(archiveText).not.toContain("do not archive this content");
    expect(archiveText).not.toContain(root);
    expect(archiveText).not.toContain("/Users/");
    expect(archiveText).not.toContain("requestId");
    expect(archiveText).not.toContain("traceId");
    expect(archiveText).not.toContain("internal-secret-type");
    expect(archiveText).not.toContain("internal-secret-code");
    expect(archiveText).not.toContain("config-secret");
    expect(archiveText).not.toContain("stt-secret");
    expect(archiveText).not.toContain("otel-secret");
    expect(archiveText).not.toContain("config-header-secret");
    if (manifest.configuration.available) {
      expect(manifest.configuration.data).not.toHaveProperty("host");
      expect(manifest.configuration.data).not.toHaveProperty("sttHost");
    }
  } finally {
    await logger.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("keeps partial collection failures bounded and reports stable codes", async () => {
  const root = await fixture();
  const output = join(root, "broken.zip");
  try {
    await Bun.write(join(root, "local-base.db"), "not sqlite");
    await runDiagnostics(
      { output },
      { logger: createLogger(), config: { root } },
      execution(),
    );
    const entries = await archiveEntries(output);
    const manifest = diagnosticsManifestSchema.parse(
      JSON.parse(new TextDecoder().decode(entries["manifest.json"])),
    );
    expect(manifest.configuration).toEqual({
      available: false,
      error: "configuration_unavailable",
    });
    expect(manifest.models).toEqual({
      available: false,
      error: "models_unavailable",
    });
    expect(JSON.stringify(manifest)).not.toContain("not sqlite");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("reports corrupted log snapshots without archiving parser errors", async () => {
  const root = await fixture();
  const output = join(root, "corrupt-logs.zip");
  const logger = createLogger();
  try {
    await logger.enableFileLogging(root);
    await logger.close();
    await Bun.write(
      join(root, "logs", "events.jsonl"),
      '{"schemaVersion":2}\n',
    );
    await runDiagnostics({ output }, { logger, config: { root } }, execution());
    const entries = await archiveEntries(output);
    const manifest = diagnosticsManifestSchema.parse(
      JSON.parse(new TextDecoder().decode(entries["manifest.json"])),
    );
    expect(manifest.logs).toEqual({
      available: true,
      data: { includedEvents: 0, window: {}, bytes: 0 },
    });
    expect(new TextDecoder().decode(entries["logs/events.jsonl"])).toBe("");
    expect(new TextDecoder().decode(entries["manifest.json"])).not.toContain(
      "Unexpected token",
    );
  } finally {
    await logger.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects existing outputs and symlinked output parents", async () => {
  const root = await fixture();
  const existing = join(root, "existing.zip");
  const realParent = join(root, "real-parent");
  const aliasParent = join(root, "alias-parent");
  await Bun.write(existing, "existing");
  await Bun.write(join(root, "sentinel.txt"), "sentinel");
  mkdirSync(realParent);
  await Bun.write(join(root, "real-parent", "placeholder"), "placeholder");
  symlinkSync(realParent, aliasParent);
  try {
    await expect(
      runDiagnostics(
        { output: existing },
        { logger: createLogger(), config: { root } },
        execution(),
      ),
    ).rejects.toThrow("already exists");
    await expect(
      runDiagnostics(
        { output: join(aliasParent, "report.zip") },
        { logger: createLogger(), config: { root } },
        execution(),
      ),
    ).rejects.toThrow("directory open");
    expect(await Bun.file(join(root, "sentinel.txt")).text()).toBe("sentinel");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("keeps secure output creation private before writing and cleans failed writes", async () => {
  const root = await fixture();
  const output = join(root, "failed.zip");
  let observedMode: number | undefined;
  setSecureFilePublicationTestHooksForTests({
    afterCreate(_file, name) {
      observedMode = statSync(join(root, name)).mode & 0o777;
    },
    beforeWrite() {
      throw new Error("injected archive write failure");
    },
  });
  try {
    await expect(
      runDiagnostics(
        { output },
        { logger: createLogger(), config: { root } },
        execution(),
      ),
    ).rejects.toThrow("injected archive write failure");
    expect(observedMode).toBe(0o600);
    expect(await Bun.file(output).exists()).toBe(false);
    expect(
      (
        await Array.fromAsync(
          new Bun.Glob(".localbase-diagnostics-*.tmp").scan({
            cwd: root,
            onlyFiles: true,
          }),
        )
      ).length,
    ).toBe(0);
  } finally {
    setSecureFilePublicationTestHooksForTests();
    rmSync(root, { recursive: true, force: true });
  }
});

test("publishes through the held parent when the visible parent is swapped", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-secure-parent-"));
  const parent = join(root, "parent");
  const replacement = join(root, "replacement");
  mkdirSync(parent);
  mkdirSync(replacement);
  const directory = openSecureDirectory(parent);
  try {
    const heldParent = join(root, "parent-old");
    renameSync(parent, heldParent);
    symlinkSync(replacement, parent);
    const temp = directory.createExclusiveFile("archive.tmp", 0o600);
    directory.write(temp, new TextEncoder().encode("held parent"));
    directory.closeFile(temp);
    directory.publish("archive.tmp", "archive.zip");
    directory.remove("archive.tmp");
    expect(
      await Bun.file(join(root, "replacement", "archive.zip")).exists(),
    ).toBe(false);
    expect(await Bun.file(join(heldParent, "archive.zip")).exists()).toBe(true);
  } finally {
    directory.close();
    rmSync(root, { recursive: true, force: true });
  }
});

test("reads configuration through the authoritative read-only schema", async () => {
  const root = await fixture();
  const path = databasePath(root);
  const before = statSync(path);
  const database = new DatabaseSession();
  try {
    database
      .get(root)
      .update(configTable)
      .set({ host: "https://user:password@example.test:8443/api?token=secret" })
      .where(eq(configTable.id, "default"))
      .run();
  } finally {
    database.close();
  }
  const updated = statSync(path);
  expect(updated.size).toBe(before.size);
  await expect(readConfig(root)).resolves.toMatchObject({
    host: "https://user:password@example.test:8443/api?token=secret",
  });
  expect(statSync(path).size).toBe(updated.size);

  const missingRoot = join(root, "missing");
  await expect(readConfig(missingRoot)).rejects.toThrow(
    "configuration database is missing",
  );
  expect(await Bun.file(databasePath(missingRoot)).exists()).toBe(false);
  rmSync(root, { recursive: true, force: true });
});

test("allows only one concurrent publication of an output", async () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-secure-concurrent-"));
  const directory = openSecureDirectory(root);
  try {
    const publish = () => {
      const temp = `temp-${crypto.randomUUID()}`;
      const file = directory.createExclusiveFile(temp, 0o600);
      directory.write(file, new TextEncoder().encode("archive"));
      directory.closeFile(file);
      try {
        directory.publish(temp, "archive.zip");
        return true;
      } catch {
        return false;
      } finally {
        directory.remove(temp);
      }
    };
    expect([publish(), publish()].filter(Boolean)).toHaveLength(1);
  } finally {
    directory.close();
    rmSync(root, { recursive: true, force: true });
  }
});
