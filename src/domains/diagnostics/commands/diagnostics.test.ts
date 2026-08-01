import { expect, test } from "bun:test";
import { mkdirSync, symlinkSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { unzipSync } from "fflate";
import { initConfig } from "../../../manager";
import { DatabaseSession } from "../../../db/client";
import { createLogger, logEventSchema } from "../../observability/logging";
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
      if (line) logEventSchema.parse(JSON.parse(line));
    }
    expect(text).not.toContain("direct-span-secret");
    expect(text).not.toContain("secret-value");
    expect(text).not.toContain("do not archive this content");
    expect(text).not.toContain(root);
    expect(text).not.toContain("/Users/");
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
    ).rejects.toThrow("symbolic links");
    expect(await Bun.file(join(root, "sentinel.txt")).text()).toBe("sentinel");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
