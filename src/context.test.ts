import { expect, test } from "bun:test";
import { diag } from "@opentelemetry/api";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { mkdir, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  createAppContext,
  otelDiagnostic,
  parseEnvironmentOverrides,
  resolveEffectiveRoot,
} from "./context";
import { withRootOperation } from "./domains/service/ownership";
import {
  ACTIVE_LOG_FILENAME,
  bootstrapDiagnosticPath,
  LocalBaseLogger,
  logDirectory,
  logEventSchema,
  readLogSnapshot,
  type ILogger,
  type LogEvent,
  type LogEventInput,
} from "./domains/observability/logging";
import {
  MAX_SERVICE_STDERR_BYTES,
  prepareServiceStderrLog,
  serviceStderrArchivePath,
  serviceStderrPath,
} from "./domains/observability/service-stderr";
import { createOtelRuntime } from "./domains/observability/otel";
import {
  canonicalLocalBaseRoot,
  ensureLocalBaseRootMarker,
} from "./utils/root";

test("validates environment overrides without mutating process state", () => {
  expect(() =>
    parseEnvironmentOverrides({ LOCALBASE_PORT: "not-a-port" }),
  ).toThrow("LOCALBASE_PORT: LOCALBASE_PORT must be an integer");
  expect(() =>
    parseEnvironmentOverrides({ LOCALBASE_DEFAULT_MAX_TOKENS: "0" }),
  ).toThrow("LOCALBASE_DEFAULT_MAX_TOKENS");
  expect(() =>
    parseEnvironmentOverrides({ LOCALBASE_DEFAULT_MAX_TOKENS: "1.5" }),
  ).toThrow("LOCALBASE_DEFAULT_MAX_TOKENS must be an integer");

  expect(
    parseEnvironmentOverrides({
      LOCALBASE_HOST: "127.0.0.1",
      LOCALBASE_PORT: "2274",
      LOCALBASE_STT_HOST: "localhost",
      LOCALBASE_STT_PORT: "8080",
      LOCALBASE_CTX_SIZE: "8192",
      LOCALBASE_DEFAULT_MAX_TOKENS: "2048",
    }),
  ).toEqual({
    host: "127.0.0.1",
    port: 2274,
    sttHost: "localhost",
    sttPort: 8080,
    ctxSize: 8192,
    defaultMaxTokens: 2048,
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

test("supports non-mutating default and persisted configuration contexts", async () => {
  const directory = mkdtempSync(
    join(tmpdir(), "local-base-read-only-context-"),
  );
  const root = join(directory, "root");
  const options = { root, nonInteractive: false, json: false };

  try {
    const defaults = await createAppContext(options, false);
    expect(defaults.config.root).toBe(canonicalLocalBaseRoot(root));
    expect(existsSync(root)).toBe(false);
    defaults.database.close();
    await defaults.logger.close();

    const readonly = await createAppContext(
      options,
      false,
      false,
      process.env,
      true,
    );
    expect(readonly.config.root).toBe(canonicalLocalBaseRoot(root));
    readonly.database.close();
    await readonly.logger.close();
    expect(existsSync(root)).toBe(false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("serve context initialization waits for the external root operation lock", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-context-lock-"));
  const root = join(directory, "root");
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  const holding = withRootOperation(root, "reset", async () => await blocked);
  let creating: ReturnType<typeof createAppContext> | undefined;
  let context: Awaited<ReturnType<typeof createAppContext>> | undefined;

  try {
    await Bun.sleep(25);
    creating = createAppContext(
      { root, nonInteractive: false, json: false },
      true,
      true,
    );
    await Bun.sleep(50);
    expect(existsSync(root)).toBe(false);
    release();
    await holding;
    context = await creating;
    expect(existsSync(join(root, "local-base.db"))).toBe(true);
  } finally {
    release();
    await holding;
    try {
      try {
        context ??= await creating;
      } catch {}
      await context?.initializationOperation?.release();
      context?.database.close();
      await context?.logger.close();
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
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

test.skipIf(process.platform !== "darwin")(
  "launchd-managed initialization bounds oversized captured stderr",
  async () => {
    const directory = mkdtempSync(join(tmpdir(), "local-base-stderr-bound-"));
    try {
      const root = join(directory, "root");
      ensureLocalBaseRootMarker(root);
      await prepareServiceStderrLog(root);
      await writeFile(
        serviceStderrPath(root),
        Buffer.alloc(MAX_SERVICE_STDERR_BYTES + 1024, "x"),
      );

      const ctx = await createAppContext(
        { root, nonInteractive: false, json: false },
        true,
        true,
        { ...process.env, LOCALBASE_SERVICE_TOKEN: crypto.randomUUID() },
      );
      try {
        expect(statSync(serviceStderrPath(root)).size).toBe(0);
        expect(statSync(serviceStderrArchivePath(root)).size).toBe(
          MAX_SERVICE_STDERR_BYTES,
        );
      } finally {
        await ctx.initializationOperation?.release();
        await ctx.logger.close();
        ctx.database.close();
      }
      expect(
        (await readLogSnapshot(root)).map((event) => event.eventName),
      ).toContain("service.stderr-rotated");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
);

test("OTel diagnostics stay local and name recovery separately", () => {
  const events: LogEventInput[] = [];
  const logger = {
    localDiagnostic: (event: LogEventInput) => events.push(event),
  } as unknown as ILogger;
  const diagnostic = otelDiagnostic(logger);
  diagnostic("warn", "OTLP export failed.", { signal: "logs" });
  diagnostic("info", "OTLP export recovered.", { signal: "logs" });
  expect(
    events.map(({ severity, eventName }) => [severity, eventName]),
  ).toEqual([
    ["warn", "observability.export-failed"],
    ["info", "observability.export-recovered"],
  ]);
});

async function closeLoggerAfterExportOutage(consoleWithFileSink: boolean) {
  const root = mkdtempSync(join(tmpdir(), "local-base-otel-diagnostics-"));
  ensureLocalBaseRootMarker(root);
  const writes: string[] = [];
  const originalWrites = {
    stdout: process.stdout.write,
    stderr: process.stderr.write,
  };
  const capture = ((chunk: string | Uint8Array) => {
    writes.push(String(chunk));
    return true;
  }) as typeof process.stdout.write;
  const exported: LogEvent[] = [];
  let sinkOpenedAt = 0;
  process.stdout.write = capture;
  process.stderr.write = capture;
  try {
    const logger = new LocalBaseLogger(undefined, { consoleWithFileSink });
    const runtime = createOtelRuntime(
      {
        enabled: true,
        tracesEndpoint: "http://127.0.0.1:1/v1/traces",
        logsEndpoint: "http://127.0.0.1:1/v1/logs",
        headers: {},
        tracesHeaders: {},
        logsHeaders: {},
        sampleRatio: 1,
        sampler: "parentbased_traceidratio",
        source: "persistent",
        displayEndpoint: "http://127.0.0.1:1/",
      },
      otelDiagnostic(logger),
    );
    const emit = runtime.emit.bind(runtime);
    runtime.emit = (event) => {
      exported.push(event);
      emit(event);
    };
    logger.setOtelRuntime(runtime);
    await logger.enableFileLogging(root);
    sinkOpenedAt = writes.length;
    // The batch exporter sends this on shutdown, where the collector refuses it.
    logger.warn("gateway", "queued for export");
    await logger.close();
  } finally {
    process.stdout.write = originalWrites.stdout;
    process.stderr.write = originalWrites.stderr;
    // The SDK logger is process-global; drop it so later runtimes do not report through this closed logger.
    diag.disable();
  }
  try {
    return {
      console: writes.slice(sinkOpenedAt).join(""),
      file: await readLogSnapshot(root),
      exported,
    };
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test("captured service stderr gets no OTel export diagnostics once the file sink opens", async () => {
  const { console, file, exported } = await closeLoggerAfterExportOutage(false);

  expect(
    file
      .filter(({ eventName }) => eventName === "observability.export-failed")
      .map(({ attributes }) => attributes?.signal),
  ).toEqual(["logs"]);
  expect(console).not.toContain("[otel]");
  expect(console).not.toContain("OTLP export");
  expect(console).not.toContain("queued for export");
  expect(exported.map(({ message }) => message)).toEqual(["queued for export"]);
});

test("foreground serve still prints OTel export diagnostics without re-exporting them", async () => {
  const { console, file, exported } = await closeLoggerAfterExportOutage(true);

  expect(console).toContain("[otel] OTLP export failed.");
  expect(
    file.some(({ eventName }) => eventName === "observability.export-failed"),
  ).toBe(true);
  expect(exported.map(({ message }) => message)).toEqual(["queued for export"]);
});
