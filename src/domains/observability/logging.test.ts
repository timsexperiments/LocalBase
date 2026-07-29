import { afterAll, expect, test } from "bun:test";
import { lstatSync, mkdtempSync, rmSync } from "node:fs";
import {
  appendFile,
  open,
  readdir,
  mkdir,
  symlink,
  unlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ensureLocalBaseRootMarker } from "../../utils/root";
import {
  ACTIVE_LOG_FILENAME,
  LocalBaseLogger,
  RotatingLogWriter,
  bootstrapDiagnosticPath,
  createLogEvent,
  followLogEvents,
  logEventSchema,
  logDirectory,
  matchesLogFilters,
  readLogSnapshot,
  writeBootstrapDiagnostic,
} from "./logging";

const directories: string[] = [];

function createRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "local-base-logs-"));
  directories.push(root);
  ensureLocalBaseRootMarker(root);
  return root;
}

function event(
  sequence: number,
  overrides: Partial<Parameters<typeof createLogEvent>[0]> = {},
) {
  return createLogEvent({
    severity: "info",
    eventName: "gateway.test",
    category: "gateway",
    component: "gateway",
    runtime: "gateway",
    message: `event ${sequence}`,
    attributes: { sequence },
    ...overrides,
  });
}

afterAll(() => {
  while (directories.length > 0) {
    rmSync(directories.pop()!, { recursive: true, force: true });
  }
});

test("validates one redacted event contract before console or file sinks", () => {
  const logged = createLogEvent({
    severity: "error",
    eventName: "http.request-failed",
    category: "http",
    component: "gateway",
    runtime: "gateway",
    message:
      'Authorization: Bearer secret-value hf_abcdefghijklmnop {"messages":["never persist this"]}',
    requestId: "request-42",
    error: { type: "Error", message: "token=private-token" },
    attributes: {
      authorization: "Bearer private-token",
      prompt: "never persist this",
      safeValue: "Bearer another-secret",
    },
  });

  expect(logEventSchema.parse(logged)).toEqual(logged);
  const serialized = JSON.stringify(logged);
  expect(serialized).not.toContain("private-token");
  expect(serialized).not.toContain("never persist this");
  expect(logged.attributes).toEqual({
    authorization: "[REDACTED]",
    prompt: "[REDACTED]",
    safevalue: "Bearer [REDACTED]",
  });

  const bypasses = createLogEvent({
    severity: "error",
    eventName: "http.bypass",
    category: "http",
    component: "gateway",
    runtime: "gateway",
    message:
      "https://user:pass@example.test/a?access_token=secret Cookie: sid=secret sk-proj-abcdefghijklmnop",
    requestId: "sk-proj-credential-shaped-request-id",
    http: {
      method: "GET",
      path: "/a?api_key=secret",
      status: 401,
      durationMs: 1,
    },
    error: {
      message: "Set-Cookie: session=secret",
      code: "hf_abcdefghijklmnop",
    },
    attributes: { url: "https://host/a?token=secret" },
  });
  const bypassSerialized = JSON.stringify(bypasses);
  for (const secret of ["user:pass", "sid=secret", "sk-proj-", "hf_"]) {
    expect(bypassSerialized).not.toContain(secret);
  }
  expect(bypasses.requestId).toBeUndefined();
  expect(bypasses.http?.path).toBe("unmatched-route");
  expect(
    createLogEvent({
      severity: "info",
      eventName: "http.request",
      category: "http",
      component: "gateway",
      runtime: "gateway",
      message: "request",
      http: {
        method: "POST",
        path: "/v1/chat/completions?api_key=never-log",
        status: 200,
        durationMs: 1,
      },
    }).http?.path,
  ).toBe("/v1/chat/completions");
  for (const requestId of [
    "ghp_abcdefghijklmnopqrstuvwxyz",
    "AKIAABCDEFGHIJKLMNOP",
    "sk%2Dproj%2Dabcdefghijklmnop",
  ]) {
    expect(
      createLogEvent({
        severity: "info",
        eventName: "http.request",
        category: "http",
        component: "gateway",
        runtime: "gateway",
        message: "request",
        requestId,
      }).requestId,
    ).toBeUndefined();
  }
});

test("never persists raw backend output", async () => {
  const root = createRoot();
  const logger = new LocalBaseLogger("json");
  const originalLog = console.log;
  console.log = () => {};
  try {
    await logger.enableFileLogging(root);
    logger.pipeStream(
      new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(
            new TextEncoder().encode('{"messages":["private prompt"]}\n'),
          );
          controller.close();
        },
      }),
      "llama-server",
    );
    const deadline = Date.now() + 1_000;
    while (
      Date.now() < deadline &&
      (await readLogSnapshot(root)).length === 0
    ) {
      await Bun.sleep(10);
    }
    await logger.close();
  } finally {
    console.log = originalLog;
  }

  const events = await readLogSnapshot(root);
  expect(events).toHaveLength(1);
  expect(events[0].message).toBe("Backend emitted a log line.");
  expect(JSON.stringify(events)).not.toContain("private prompt");
});

test("writes private JSONL files, rotates them, and preserves chronological snapshots", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root, {
    maxActiveBytes: 350,
    maxArchives: 2,
  });
  await writer.open();
  for (let sequence = 1; sequence <= 8; sequence += 1) {
    writer.enqueue(event(sequence));
  }
  await writer.close();

  const active = join(logDirectory(root), ACTIVE_LOG_FILENAME);
  expect(lstatSync(logDirectory(root)).mode & 0o077).toBe(0);
  expect(lstatSync(active).mode & 0o077).toBe(0);
  const events = await readLogSnapshot(root);
  expect(events.map((entry) => entry.attributes?.sequence)).toEqual([6, 7, 8]);
  expect(events.map((entry) => entry.id)).toEqual([
    ...new Set(events.map((entry) => entry.id)),
  ]);
});

test("reports file failures without crashing and records dropped events after recovery", async () => {
  const root = createRoot();
  let attempts = 0;
  const failures: string[] = [];
  const writer = new RotatingLogWriter(root, {
    append: async (path, contents) => {
      attempts += 1;
      if (attempts === 1) throw new Error("disk token=private-token");
      await appendFile(path, contents, { encoding: "utf8", mode: 0o600 });
    },
    onFailure: (message) => failures.push(message),
  });
  await writer.open();
  writer.enqueue(event(1));
  writer.enqueue(event(2));
  await writer.close();

  expect(failures).toHaveLength(1);
  expect(failures[0]).not.toContain("private-token");
  const events = await readLogSnapshot(root);
  expect(events.map((entry) => entry.eventName)).toEqual([
    "logging.events-dropped",
    "gateway.test",
  ]);
  expect(events[0].attributes).toEqual({ dropped: 1 });
  expect(events[1].attributes?.sequence).toBe(2);
});

test("bounds a saturated write queue and reports the discarded event count", async () => {
  const root = createRoot();
  let releaseWrite!: () => void;
  const blockedWrite = new Promise<void>((resolve) => {
    releaseWrite = resolve;
  });
  const writer = new RotatingLogWriter(root, {
    maxQueueEvents: 1,
    append: async (path, contents) => {
      await blockedWrite;
      await appendFile(path, contents, { encoding: "utf8", mode: 0o600 });
    },
  });
  await writer.open();
  writer.enqueue(event(1));
  await Bun.sleep(10);
  writer.enqueue(event(2));
  writer.enqueue(event(3));
  releaseWrite();
  await writer.close();

  const events = await readLogSnapshot(root);
  expect(events.map((entry) => entry.attributes?.sequence)).toEqual([
    1,
    undefined,
    2,
  ]);
  expect(events[1].eventName).toBe("logging.events-dropped");
  expect(events[1].attributes).toEqual({ dropped: 1 });
});

test("paces a permanently failed sink and recovers without a retry loop", async () => {
  const root = createRoot();
  let attempts = 0;
  let available = false;
  const scheduled: number[] = [];
  const writer = new RotatingLogWriter(root, {
    retryBaseMs: 40,
    retryMaxMs: 40,
    onRetryScheduled: (delay) => scheduled.push(delay),
    append: async (path, contents) => {
      attempts += 1;
      if (!available) throw new Error("permanent failure");
      await appendFile(path, contents, { encoding: "utf8", mode: 0o600 });
    },
  });
  await writer.open();
  for (let sequence = 1; sequence <= 20; sequence += 1) {
    writer.enqueue(event(sequence));
  }
  await Bun.sleep(20);
  expect(attempts).toBe(1);
  expect(scheduled).toHaveLength(1);
  available = true;
  await Bun.sleep(50);
  await writer.close();
  const recoveredEvents = await readLogSnapshot(root);
  expect(
    recoveredEvents.some(
      (entry) => entry.eventName === "logging.events-dropped",
    ),
  ).toBe(true);
  expect(recoveredEvents.at(-1)?.attributes?.sequence).toBe(20);

  const failedWriter = new RotatingLogWriter(createRoot(), {
    append: async () => {
      throw new Error("still unavailable");
    },
  });
  await failedWriter.open();
  failedWriter.enqueue(event(21));
  await expect(
    Promise.race([
      failedWriter.close().then(() => "closed"),
      Bun.sleep(500).then(() => "timed-out"),
    ]),
  ).resolves.toBe("closed");
});

test("snapshot tolerates rapid concurrent rotation without duplicate identities", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root, {
    maxActiveBytes: 400,
    maxArchives: 5,
  });
  await writer.open();
  const rotating = (async () => {
    for (let sequence = 1; sequence <= 100; sequence += 1) {
      writer.enqueue(event(sequence));
      await writer.flush();
      if (sequence % 3 === 0) await Bun.sleep(0);
    }
  })();
  for (let attempt = 0; attempt < 40; attempt += 1) {
    const snapshot = await readLogSnapshot(root, {}, 100);
    expect(snapshot.map((entry) => entry.id)).toEqual([
      ...new Set(snapshot.map((entry) => entry.id)),
    ]);
    await Bun.sleep(0);
  }
  await rotating;
  await writer.close();
});

test("filters snapshots and follows records across an atomic rotation without duplicates", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root, {
    maxActiveBytes: 300,
    maxArchives: 3,
  });
  await writer.open();
  writer.enqueue(event(1, { runtime: "llm", requestId: "follow-42" }));
  await writer.flush();

  const controller = new AbortController();
  const received: number[] = [];
  const following = followLogEvents(
    root,
    { runtime: "llm" },
    (entry) => {
      received.push(entry.attributes?.sequence as number);
      if (received.length === 4) controller.abort();
    },
    controller.signal,
  );
  await Bun.sleep(25);
  const since = new Date().toISOString();
  await Bun.sleep(10);
  for (let sequence = 2; sequence <= 4; sequence += 1) {
    writer.enqueue(event(sequence, { runtime: "llm", requestId: "follow-42" }));
  }
  await writer.flush();
  await following;
  await writer.close();

  expect(received).toEqual([1, 2, 3, 4]);
  const filtered = await readLogSnapshot(root, {
    since,
    level: "info",
    runtime: "llm",
    requestId: "follow-42",
  });
  expect(filtered.map((entry) => entry.attributes?.sequence)).toEqual([
    2, 3, 4,
  ]);
});

test("flushes gateway events before shutdown", async () => {
  const root = createRoot();
  const logger = new LocalBaseLogger("json");
  const originalLog = console.log;
  console.log = () => {};
  try {
    await logger.enableFileLogging(root);
    logger.event({
      severity: "info",
      eventName: "gateway.stopped",
      category: "gateway",
      component: "gateway",
      runtime: "gateway",
      message: "Gateway stopped.",
    });
    await logger.close();
  } finally {
    console.log = originalLog;
  }

  expect(await readLogSnapshot(root)).toHaveLength(1);
});

test("fails closed for active, archive, and dangling log symlinks", async () => {
  const targetRoot = createRoot();
  const target = join(targetRoot, "target.log");
  await writeFile(target, "outside\n");

  for (const name of [ACTIVE_LOG_FILENAME, "events.1.jsonl"]) {
    const root = createRoot();
    await mkdir(logDirectory(root), { mode: 0o700 });
    if (name === ACTIVE_LOG_FILENAME) {
      await writeFile(activeLogPathForTest(root), "", { mode: 0o600 });
      await unlink(activeLogPathForTest(root));
    }
    await symlink(target, join(logDirectory(root), name));
    if (name === ACTIVE_LOG_FILENAME) {
      const writer = new RotatingLogWriter(root);
      await expect(writer.open()).rejects.toThrow("not a regular file");
    } else {
      await expect(readLogSnapshot(root)).rejects.toThrow("not a regular file");
    }
  }

  const danglingRoot = createRoot();
  await mkdir(logDirectory(danglingRoot), { mode: 0o700 });
  await symlink(
    join(danglingRoot, "missing"),
    join(logDirectory(danglingRoot), ACTIVE_LOG_FILENAME),
  );
  await expect(new RotatingLogWriter(danglingRoot).open()).rejects.toThrow(
    "not a regular file",
  );
});

test("repairs a trailing partial record before the next append", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root);
  await writer.open();
  writer.enqueue(event(1));
  await writer.close();
  await appendFile(activeLogPathForTest(root), '{"partial":');

  const recovered = new RotatingLogWriter(root);
  await recovered.open();
  recovered.enqueue(event(2));
  await recovered.close();
  expect(
    (await readLogSnapshot(root)).map((entry) => entry.attributes?.sequence),
  ).toEqual([1, 2]);
});

test("bounds snapshots and compares since filters by timestamp instant", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root);
  await writer.open();
  for (let sequence = 1; sequence <= 5; sequence += 1) {
    writer.enqueue(event(sequence));
  }
  await writer.close();
  expect(
    (await readLogSnapshot(root, {}, 2)).map(
      (entry) => entry.attributes?.sequence,
    ),
  ).toEqual([4, 5]);

  const equivalent = {
    ...event(6),
    timestamp: "2026-01-01T00:00:00.000Z",
  };
  expect(
    matchesLogFilters(logEventSchema.parse(equivalent), {
      since: "2025-12-31T18:00:00.000-06:00",
    }),
  ).toBe(true);
});

test("surfaces bounded bootstrap diagnostics through the validated contract", async () => {
  const root = createRoot();
  const written = await writeBootstrapDiagnostic(
    root,
    new Error("startup failed Authorization: Bearer private-token"),
  );
  const first = await readLogSnapshot(root);
  const second = await readLogSnapshot(root);
  expect(first).toEqual([written]);
  expect(second).toEqual(first);
  expect(JSON.stringify(first)).not.toContain("private-token");
  expect(
    (await Bun.file(bootstrapDiagnosticPath(root)).stat()).size,
  ).toBeLessThan(256 * 1024);
});

test("atomically replaces bootstrap diagnostics without missing or invalid reads", async () => {
  const root = createRoot();
  await writeBootstrapDiagnostic(root, new Error("initial failure"));
  const path = bootstrapDiagnosticPath(root);
  const failures: unknown[] = [];
  let replacing = true;
  const reader = (async () => {
    while (replacing) {
      try {
        const contents = await Bun.file(path).text();
        const lines = contents.trimEnd().split("\n");
        expect(lines).toHaveLength(1);
        logEventSchema.parse(JSON.parse(lines[0]!));
      } catch (error) {
        failures.push(error);
      }
      await Bun.sleep(0);
    }
  })();
  await Promise.all(
    Array.from({ length: 300 }, (_, index) =>
      writeBootstrapDiagnostic(root, new Error(`failure ${index}`)),
    ),
  );
  replacing = false;
  await reader;
  expect(failures).toEqual([]);
  logEventSchema.parse(JSON.parse((await Bun.file(path).text()).trimEnd()));
  expect(
    (await readdir(logDirectory(root))).filter((name) =>
      name.startsWith(".bootstrap."),
    ),
  ).toEqual([]);
});

test("follows appends and rotations occurring during a bounded handle read exactly once", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root, {
    maxActiveBytes: 500,
    maxArchives: 3,
  });
  await writer.open();
  writer.enqueue(event(1));
  await writer.flush();

  const controller = new AbortController();
  const received: number[] = [];
  let injected = false;
  const following = followLogEvents(
    root,
    {},
    (entry) => {
      received.push(entry.attributes?.sequence as number);
      if (received.length === 4) controller.abort();
    },
    controller.signal,
    {
      pollMs: 1,
      onRead: async () => {
        if (injected) return;
        injected = true;
        writer.enqueue(event(2));
        writer.enqueue(event(3));
        writer.enqueue(event(4));
        await writer.flush();
      },
    },
  );
  await following;
  await writer.close();
  expect(received).toEqual([1, 2, 3, 4]);
  expect(new Set(received).size).toBe(received.length);
});

test("follow detects a same-inode truncate and regrow between polls", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root);
  await writer.open();
  writer.enqueue(event(1));
  await writer.flush();

  const controller = new AbortController();
  const received: number[] = [];
  const following = followLogEvents(
    root,
    {},
    (entry) => {
      received.push(entry.attributes?.sequence as number);
      if (received.includes(2)) controller.abort();
    },
    controller.signal,
    { pollMs: 20 },
  );
  while (!received.includes(1)) await Bun.sleep(1);
  const path = activeLogPathForTest(root);
  const identity = lstatSync(path).ino;
  const replacement = Buffer.from(`${JSON.stringify(event(2))}\n`);
  const handle = await open(path, "r+");
  try {
    await handle.write(replacement, 0, replacement.length, 0);
    await handle.truncate(replacement.length);
  } finally {
    await handle.close();
  }
  expect(lstatSync(path).ino).toBe(identity);
  await following;
  await writer.close();
  expect(received).toEqual([1, 2]);
});

test("reverse snapshots preserve UTF-8 boundaries and discard oversized partial records", async () => {
  const root = createRoot();
  const writer = new RotatingLogWriter(root, { maxActiveBytes: 1024 * 1024 });
  await writer.open();
  for (let sequence = 1; sequence <= 300; sequence += 1) {
    writer.enqueue(
      event(sequence, { message: `unicode ${sequence} 🧪 こんにちは` }),
    );
  }
  await writer.close();
  await appendFile(activeLogPathForTest(root), Buffer.alloc(100_000, 0x78));
  const snapshot = await readLogSnapshot(root, {}, 300);
  expect(snapshot).toHaveLength(300);
  expect(snapshot.every((entry) => entry.message.includes("🧪"))).toBe(true);
});

function activeLogPathForTest(root: string): string {
  return join(logDirectory(root), ACTIVE_LOG_FILENAME);
}
