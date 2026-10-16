import { afterEach, expect, test } from "bun:test";
import {
  appendFileSync,
  closeSync,
  constants,
  lstatSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { logDirectory } from "./logging";
import {
  boundServiceStderrLog,
  capturesServiceStderr,
  inspectServiceStderrLog,
  prepareServiceStderrLog,
  serviceStderrArchivePath,
  serviceStderrDirectory,
  serviceStderrPath,
} from "./service-stderr";

const directories: string[] = [];

function createRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "local-base-service-stderr-"));
  directories.push(root);
  return root;
}

afterEach(() => {
  for (const directory of directories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("captured stderr lives in the structured log directory", () => {
  const root = createRoot();
  expect(serviceStderrDirectory(root)).toBe(logDirectory(root));
});

test("only launchd-managed gateways capture stderr", () => {
  const token = { LOCALBASE_SERVICE_TOKEN: crypto.randomUUID() };
  expect(capturesServiceStderr(token, "darwin")).toBe(true);
  expect(capturesServiceStderr(token, "linux")).toBe(false);
  expect(capturesServiceStderr({}, "darwin")).toBe(false);
});

test("prepares a private stderr file before launchd opens it", async () => {
  const root = createRoot();
  await prepareServiceStderrLog(root);
  appendFileSync(serviceStderrPath(root), "panic\n");
  await prepareServiceStderrLog(root);

  expect(lstatSync(serviceStderrDirectory(root)).mode & 0o777).toBe(0o700);
  expect(lstatSync(serviceStderrPath(root)).mode & 0o777).toBe(0o600);
  expect(readFileSync(serviceStderrPath(root), "utf8")).toBe("panic\n");
});

test("leaves stderr under the cap untouched and ignores a missing file", async () => {
  const root = createRoot();
  expect(await boundServiceStderrLog(root, 16)).toBe(false);
  await prepareServiceStderrLog(root);
  writeFileSync(serviceStderrPath(root), "0123456789abcdef");

  expect(await boundServiceStderrLog(root, 16)).toBe(false);
  expect(readFileSync(serviceStderrPath(root), "utf8")).toBe(
    "0123456789abcdef",
  );
});

test("archives the newest bytes and truncates oversized stderr for an append-mode writer", async () => {
  const root = createRoot();
  await prepareServiceStderrLog(root);
  // launchd opens StandardErrorPath with O_APPEND; the gateway keeps writing through it.
  const descriptor = openSync(
    serviceStderrPath(root),
    constants.O_WRONLY | constants.O_APPEND,
  );
  try {
    writeSync(descriptor, `${"a".repeat(40)}${"b".repeat(16)}`);
    expect(await boundServiceStderrLog(root, 16)).toBe(true);
    writeSync(descriptor, "after\n");
  } finally {
    closeSync(descriptor);
  }

  expect(readFileSync(serviceStderrArchivePath(root), "utf8")).toBe(
    "b".repeat(16),
  );
  expect(lstatSync(serviceStderrArchivePath(root)).mode & 0o777).toBe(0o600);
  expect(readFileSync(serviceStderrPath(root), "utf8")).toBe("after\n");
  expect(statSync(serviceStderrPath(root)).size).toBe(6);
});

test("reports only stderr metadata and stays quiet when nothing was captured", async () => {
  const root = createRoot();
  expect(await inspectServiceStderrLog(root)).toBeUndefined();
  await prepareServiceStderrLog(root);
  expect(await inspectServiceStderrLog(root)).toBeUndefined();

  appendFileSync(serviceStderrPath(root), "secret prompt in a stack trace\n");
  const summary = await inspectServiceStderrLog(root);
  expect(summary).toEqual({
    path: serviceStderrPath(root),
    bytes: 31,
    modifiedAt: expect.any(String),
    archiveBytes: 0,
  });
  expect(JSON.stringify(summary)).not.toContain("secret prompt");
});
