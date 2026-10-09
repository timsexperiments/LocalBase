import { expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CATALOG, byId } from "../../catalog";
import { defaultConfig } from "../../manager";
import {
  assertModelDiskSpace,
  assertPortAvailable,
  assertServePortsAvailable,
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

test("disk admission sums remaining artifact bytes and accepts the exact boundary", () => {
  const root = mkdtempSync(join(tmpdir(), "localbase-disk-boundary-"));
  try {
    const config = defaultConfig(root);
    const model = CATALOG.find(
      (candidate) => candidate.kind === "llm" && candidate.artifacts.length > 1,
    );
    if (!model) throw new Error("Expected multi-artifact catalog model");
    const multi = {
      ...model,
      artifacts: [
        {
          ...model.artifacts[0]!,
          filename: "first.gguf",
          expectedSizeBytes: 10,
        },
        {
          ...model.artifacts[1]!,
          filename: "second.gguf",
          expectedSizeBytes: 20,
        },
      ],
    };
    mkdirSync(config.llmModelsDir, { recursive: true });
    writeFileSync(join(config.llmModelsDir, "first.gguf.partial"), "1234");
    expect(() => assertModelDiskSpace(config, multi, 26)).not.toThrow();
    expect(() => assertModelDiskSpace(config, multi, 25)).toThrow(/requires/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("lazy model installation refuses without consent", async () => {
  let downloads = 0;
  await expect(
    installMissingModel(
      defaultConfig("/tmp/localbase-lazy-consent-test"),
      undefined,
      "lazy-test-model",
      false,
      async () => {
        downloads++;
        return "/models/lazy.gguf";
      },
    ),
  ).rejects.toThrow(/restart serve with --install-missing/);
  expect(downloads).toBe(0);
});

test("port preflight rejects duplicate planned bindings", () => {
  const config = defaultConfig("/tmp/localbase-port-overlap-test");
  expect(() =>
    assertServePortsAvailable(config, {
      port: 24101,
      llmPort: 24101,
      stt: false,
      image: false,
      video: false,
    }),
  ).toThrow(/bindings overlap/);
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
