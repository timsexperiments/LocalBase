import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { byId } from "../../catalog";
import { defaultConfig } from "../../manager";
import {
  assertModelDiskSpace,
  assertPortAvailable,
  installMissingModel,
} from "./startup-preflight";

test("missing model downloads require explicit serve consent", async () => {
  let downloads = 0;
  const install = async () => {
    downloads += 1;
    return "/models/test.gguf";
  };
  await expect(
    installMissingModel(
      defaultConfig("/tmp/localbase-consent-test"),
      undefined,
      "test-model",
      false,
      install,
    ),
  ).rejects.toThrow(/models install test-model/);
  expect(downloads).toBe(0);
  await expect(
    installMissingModel(
      defaultConfig("/tmp/localbase-consent-test"),
      undefined,
      "test-model",
      true,
      install,
    ),
  ).resolves.toBe("/models/test.gguf");
  expect(downloads).toBe(1);
});

test("missing model downloads refuse when declared artifact size exceeds free space", () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-disk-preflight-"));
  try {
    const config = defaultConfig(root);
    const model = byId(config.activeLlmModel);
    if (!model) throw new Error("Expected default quickstart model in catalog");
    expect(() => assertModelDiskSpace(config, model, 0)).toThrow(
      /Insufficient disk space.*requires.*available/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("port preflight reports an occupied port", () => {
  const listener = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: () => new Response(),
  });
  try {
    const port = listener.port;
    if (port === undefined) throw new Error("Expected listener to bind a port");
    expect(() => assertPortAvailable("127.0.0.1", port)).toThrow(
      new RegExp(`Port ${port} is already in use`),
    );
  } finally {
    listener.stop(true);
  }
});
