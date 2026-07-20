import { afterEach, describe, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { createHash, randomUUID } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { arch, platform, tmpdir } from "node:os";
import { join } from "node:path";
import { CATALOG, type ModelArtifact, type ModelSpec } from "./catalog";
import {
  defaultConfig,
  installModel,
  installedModels,
  launchLlamaServer,
  loadConfig,
  managedRuntimeUnavailableError,
  platformSupportTier,
  saveConfig,
  startLlamaServerProcess,
  type LocalBaseConfig,
} from "./manager";

const testRoots: string[] = [];
const testModelIds: string[] = [];
const testServerClosers: Array<() => Promise<void>> = [];

type ArtifactRequest = { path: string; range?: string };

async function createArtifactServer(
  files: Record<string, Uint8Array>,
  interruptedRequests = new Map<string, number>(),
): Promise<{
  source: string;
  requests: ArtifactRequest[];
}> {
  const requests: ArtifactRequest[] = [];
  const server = createServer((request, response) => {
    const path = new URL(
      request.url ?? "/",
      `http://${request.headers.host ?? "127.0.0.1"}`,
    ).pathname;
    const body = files[path];
    requests.push({ path, range: request.headers.range });

    if (!body) {
      response.writeHead(404).end();
      return;
    }

    const interruptionsLeft = interruptedRequests.get(path) ?? 0;
    if (interruptionsLeft > 0) {
      interruptedRequests.set(path, interruptionsLeft - 1);
      response.writeHead(200, {
        Connection: "close",
        "Content-Length": String(body.byteLength),
      });
      response.end(
        body.subarray(0, Math.max(1, Math.floor(body.byteLength / 2))),
        () => request.socket.destroy(),
      );
      return;
    }

    const range = /^bytes=(\d+)-$/.exec(request.headers.range ?? "");
    if (range) {
      const start = Number(range[1]);
      if (start >= body.byteLength) {
        response
          .writeHead(416, {
            "Content-Range": `bytes */${body.byteLength}`,
          })
          .end();
        return;
      }
      const chunk = body.subarray(start);
      response.writeHead(206, {
        "Accept-Ranges": "bytes",
        "Content-Length": String(chunk.byteLength),
        "Content-Range": `bytes ${start}-${body.byteLength - 1}/${body.byteLength}`,
      });
      response.end(chunk);
      return;
    }

    response.writeHead(200, {
      "Accept-Ranges": "bytes",
      "Content-Length": String(body.byteLength),
    });
    response.end(body);
  });

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  testServerClosers.push(
    () =>
      new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
      }),
  );

  const { port } = server.address() as AddressInfo;
  return { source: `http://127.0.0.1:${port}/repo`, requests };
}

function sha256(content: Uint8Array): string {
  return createHash("sha256").update(content).digest("hex");
}

function artifact(
  sourcePath: string,
  content: Uint8Array,
  role: ModelArtifact["role"],
): ModelArtifact {
  return {
    sourcePath,
    filename: sourcePath.split("/").at(-1)!,
    expectedSizeBytes: content.byteLength,
    sha256: sha256(content),
    role,
  };
}

function installFixtureModel(
  source: string,
  artifacts: ModelArtifact[],
): string {
  const modelId = `test-install-${randomUUID()}`;
  (CATALOG as ModelSpec[]).push({
    modelId,
    kind: "llm",
    provider: "Test",
    family: "Test",
    version: "1",
    size: "1B",
    quant: "Q4_K_M",
    minVramGb: 1,
    storageGb: 1,
    source,
    repositoryRevision: "test-revision",
    artifacts,
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["test"],
    commercialStatus: "open",
    catch: "Test only.",
    notes: "Test only.",
  });
  testModelIds.push(modelId);
  return modelId;
}

function createInstallConfig(): LocalBaseConfig {
  const root = mkdtempSync(join(tmpdir(), "local-base-install-"));
  testRoots.push(root);
  return defaultConfig(root);
}

function artifactPath(source: string, sourcePath: string): string {
  return `${new URL(source).pathname}/resolve/test-revision/${sourcePath}`;
}

function createLegacyConfigRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "local-base-parallel-"));
  testRoots.push(root);
  const db = new Database(join(root, "local-base.db"));
  db.exec(`
    create table config (
      id text primary key,
      root text not null,
      llm_models_dir text not null,
      stt_models_dir text not null,
      image_models_dir text,
      runtime_backend text not null,
      stt_backend text not null,
      host text not null,
      port integer not null,
      ctx_size integer not null,
      stt_host text not null,
      stt_port integer not null,
      startup_on_boot integer not null,
      selected_llm_models text not null,
      selected_stt_models text not null,
      selected_image_models text,
      active_llm_model text not null,
      active_stt_model text not null,
      active_image_model text,
      system_prompt text,
      hf_token text
    );
  `);
  db.prepare(
    `insert into config values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    "default",
    root,
    join(root, "models", "llm"),
    join(root, "models", "stt"),
    join(root, "models", "image"),
    "llama.cpp",
    "whisper.cpp",
    "127.0.0.1",
    18000,
    8192,
    "127.0.0.1",
    18080,
    0,
    '["qwen2.5-coder-7b-instruct-q4_k_m"]',
    '["whisper-base-q8_0"]',
    "[]",
    "qwen2.5-coder-7b-instruct-q4_k_m",
    "whisper-base-q8_0",
    "",
    "",
    "",
  );
  db.close();
  return root;
}

async function createLlamaLaunchFixture(
  parallel: LocalBaseConfig["parallel"],
): Promise<{
  argsPath: string;
  config: LocalBaseConfig;
  modelFile: string;
  modelPath: string;
}> {
  const root = mkdtempSync(join(tmpdir(), "local-base-llama-launch-"));
  testRoots.push(root);

  const config = defaultConfig(root, 9.5);
  config.activeLlmModel = "qwen2.5-coder-7b-instruct-q4_k_m";
  config.parallel = parallel;

  const modelFile = "model.gguf";
  const modelPath = join(config.llmModelsDir, modelFile);
  const binDir = join(root, "bin");
  const binPath = join(binDir, "llama-server");
  const argsPath = join(binDir, "llama-server.args");
  mkdirSync(config.llmModelsDir, { recursive: true });
  mkdirSync(binDir, { recursive: true });
  await Bun.write(modelPath, "model placeholder");
  await Bun.write(
    binPath,
    `#!/bin/sh
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
printf '%s\\n' "$@" > "$script_dir/llama-server.args"
`,
  );
  chmodSync(binPath, 0o755);

  return { argsPath, config, modelFile, modelPath };
}

function expectedLlamaArgs(modelPath: string, parallel: string): string[] {
  const args = [
    "-m",
    modelPath,
    "--host",
    "127.0.0.1",
    "--port",
    "18000",
    "-c",
    "8192",
    "--parallel",
    parallel,
    "--jinja",
    "--embeddings",
  ];
  if (platform() === "darwin" && arch() === "arm64") {
    args.push("--flash-attn", "auto");
  }
  return args;
}

function readCapturedArgs(argsPath: string): string[] {
  return readFileSync(argsPath, "utf8").trim().split("\n");
}

afterEach(async () => {
  await Promise.all(testServerClosers.splice(0).map((close) => close()));
  for (const modelId of testModelIds.splice(0)) {
    const index = (CATALOG as ModelSpec[]).findIndex(
      (model) => model.modelId === modelId,
    );
    if (index >= 0) (CATALOG as ModelSpec[]).splice(index, 1);
  }
  for (const root of testRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

describe("transactional model artifact installation", () => {
  test("keeps single-file installs compatible and supports a filename override", async () => {
    const content = Buffer.from("single model");
    const server = await createArtifactServer({
      "/repo/resolve/test-revision/model.gguf": content,
    });
    const modelId = installFixtureModel(server.source, [
      {
        sourcePath: "model.gguf",
        filename: "model.gguf",
        role: "primary",
      },
    ]);
    const config = createInstallConfig();

    const installed = await installModel(config, modelId, "renamed.gguf");

    expect(installed).toBe(join(config.llmModelsDir, "renamed.gguf"));
    expect(readFileSync(installed)).toEqual(content);
    expect(existsSync(join(config.llmModelsDir, "renamed.gguf.partial"))).toBe(
      false,
    );
  });

  test("installs every shard sequentially and returns the primary artifact", async () => {
    const primary = Buffer.from("primary shard");
    const supplementary = Buffer.from("supplementary shard");
    const primaryArtifact = artifact("model-00001.gguf", primary, "primary");
    const supplementaryArtifact = artifact(
      "model-00002.gguf",
      supplementary,
      "supplementary",
    );
    const server = await createArtifactServer({
      "/repo/resolve/test-revision/model-00001.gguf": primary,
      "/repo/resolve/test-revision/model-00002.gguf": supplementary,
    });
    const modelId = installFixtureModel(server.source, [
      primaryArtifact,
      supplementaryArtifact,
    ]);
    const config = createInstallConfig();

    const installed = await installModel(config, modelId);

    expect(installed).toBe(join(config.llmModelsDir, primaryArtifact.filename));
    expect(readFileSync(installed)).toEqual(primary);
    expect(
      readFileSync(join(config.llmModelsDir, supplementaryArtifact.filename)),
    ).toEqual(supplementary);
    expect(server.requests.map((request) => request.path)).toEqual([
      artifactPath(server.source, primaryArtifact.sourcePath),
      artifactPath(server.source, supplementaryArtifact.sourcePath),
    ]);
  });

  test("skips a verified shard and resumes a truncated artifact with Range", async () => {
    const primary = Buffer.from("already verified");
    const supplementary = Buffer.from("resume this shard");
    const primaryArtifact = artifact("model-00001.gguf", primary, "primary");
    const supplementaryArtifact = artifact(
      "model-00002.gguf",
      supplementary,
      "supplementary",
    );
    const server = await createArtifactServer({
      "/repo/resolve/test-revision/model-00001.gguf": primary,
      "/repo/resolve/test-revision/model-00002.gguf": supplementary,
    });
    const modelId = installFixtureModel(server.source, [
      primaryArtifact,
      supplementaryArtifact,
    ]);
    const config = createInstallConfig();
    mkdirSync(config.llmModelsDir, { recursive: true });
    writeFileSync(join(config.llmModelsDir, primaryArtifact.filename), primary);
    const partial = join(
      config.llmModelsDir,
      `${supplementaryArtifact.filename}.partial`,
    );
    const prefix = supplementary.subarray(0, 9);
    const shorterPartial = supplementary.subarray(0, 4);
    writeFileSync(
      join(config.llmModelsDir, supplementaryArtifact.filename),
      prefix,
    );
    writeFileSync(partial, shorterPartial);

    await installModel(config, modelId);

    expect(
      server.requests.map((request) => ({
        path: request.path,
        range: request.range,
      })),
    ).toEqual([
      {
        path: artifactPath(server.source, supplementaryArtifact.sourcePath),
        range: `bytes=${prefix.byteLength}-`,
      },
    ]);
    expect(
      readFileSync(join(config.llmModelsDir, supplementaryArtifact.filename)),
    ).toEqual(supplementary);
    expect(existsSync(partial)).toBe(false);
  });

  test("preserves completed shards and partial failures, then repairs the set on retry", async () => {
    const primary = Buffer.from("primary shard");
    const supplementary = Buffer.from("supplementary shard that interrupts");
    const primaryArtifact = artifact("model-00001.gguf", primary, "primary");
    const supplementaryArtifact = artifact(
      "model-00002.gguf",
      supplementary,
      "supplementary",
    );
    const secondPath = "/repo/resolve/test-revision/model-00002.gguf";
    const server = await createArtifactServer(
      {
        "/repo/resolve/test-revision/model-00001.gguf": primary,
        [secondPath]: supplementary,
      },
      new Map([[secondPath, 1]]),
    );
    const modelId = installFixtureModel(server.source, [
      primaryArtifact,
      supplementaryArtifact,
    ]);
    const config = createInstallConfig();
    const partial = join(
      config.llmModelsDir,
      `${supplementaryArtifact.filename}.partial`,
    );

    await expect(installModel(config, modelId)).rejects.toThrow(
      "Failed to download model",
    );
    expect(
      readFileSync(join(config.llmModelsDir, primaryArtifact.filename)),
    ).toEqual(primary);
    const partialSize = readFileSync(partial).byteLength;
    expect(partialSize).toBeGreaterThan(0);

    await expect(installModel(config, modelId)).resolves.toBe(
      join(config.llmModelsDir, primaryArtifact.filename),
    );
    expect(
      readFileSync(join(config.llmModelsDir, supplementaryArtifact.filename)),
    ).toEqual(supplementary);
    expect(existsSync(partial)).toBe(false);
    expect(server.requests.at(-1)).toEqual({
      path: artifactPath(server.source, supplementaryArtifact.sourcePath),
      range: `bytes=${partialSize}-`,
    });
  });

  test("cleans partials when authoritative size or checksum validation fails", async () => {
    const wrongSize = Buffer.from("wrong size");
    const wrongHash = Buffer.from("wrong hash");
    const server = await createArtifactServer({
      "/repo/resolve/test-revision/wrong-size.gguf": wrongSize,
      "/repo/resolve/test-revision/wrong-hash.gguf": wrongHash,
    });
    const sizeModel = installFixtureModel(server.source, [
      {
        ...artifact("wrong-size.gguf", wrongSize, "primary"),
        expectedSizeBytes: wrongSize.byteLength + 1,
      },
    ]);
    const hashModel = installFixtureModel(server.source, [
      {
        ...artifact("wrong-hash.gguf", wrongHash, "primary"),
        sha256: "0".repeat(64),
      },
    ]);
    const config = createInstallConfig();

    await expect(installModel(config, sizeModel)).rejects.toThrow(
      "Size mismatch",
    );
    await expect(installModel(config, hashModel)).rejects.toThrow(
      "Checksum mismatch",
    );

    expect(existsSync(join(config.llmModelsDir, "wrong-size.gguf"))).toBe(
      false,
    );
    expect(
      existsSync(join(config.llmModelsDir, "wrong-size.gguf.partial")),
    ).toBe(false);
    expect(existsSync(join(config.llmModelsDir, "wrong-hash.gguf"))).toBe(
      false,
    );
    expect(
      existsSync(join(config.llmModelsDir, "wrong-hash.gguf.partial")),
    ).toBe(false);
  });

  test("rejects filename overrides for multi-artifact models before downloading", async () => {
    const primary = Buffer.from("primary");
    const supplementary = Buffer.from("supplementary");
    const server = await createArtifactServer({
      "/repo/resolve/test-revision/model-00001.gguf": primary,
      "/repo/resolve/test-revision/model-00002.gguf": supplementary,
    });
    const modelId = installFixtureModel(server.source, [
      artifact("model-00001.gguf", primary, "primary"),
      artifact("model-00002.gguf", supplementary, "supplementary"),
    ]);

    await expect(
      installModel(createInstallConfig(), modelId, "renamed.gguf"),
    ).rejects.toThrow("filename override is not supported for multi-artifact");
    expect(server.requests).toEqual([]);
  });
});

describe("installed model reporting", () => {
  test("reports complete catalog sets once and preserves unmatched files", () => {
    const primary = Buffer.from("primary");
    const supplementary = Buffer.from("supplementary");
    const modelId = installFixtureModel("https://example.com/models", [
      artifact("reporting-00001.gguf", primary, "primary"),
      artifact("reporting-00002.gguf", supplementary, "supplementary"),
    ]);
    const config = createInstallConfig();
    mkdirSync(config.llmModelsDir, { recursive: true });
    writeFileSync(join(config.llmModelsDir, "reporting-00001.gguf"), primary);
    writeFileSync(
      join(config.llmModelsDir, "reporting-00002.gguf"),
      supplementary,
    );
    writeFileSync(join(config.llmModelsDir, "z-manual.gguf"), "manual");

    expect(installedModels(config, "llm")).toEqual([modelId, "z-manual.gguf"]);
    expect(installedModels(config)).toEqual([
      `llm:${modelId}`,
      "llm:z-manual.gguf",
    ]);

    rmSync(join(config.llmModelsDir, "reporting-00002.gguf"));
    expect(installedModels(config, "llm")).toEqual(["z-manual.gguf"]);

    writeFileSync(join(config.llmModelsDir, "reporting-00002.gguf"), "short");
    expect(installedModels(config, "llm")).toEqual(["z-manual.gguf"]);
  });

  test("keeps complete single-file catalog models compatible", () => {
    const config = createInstallConfig();
    const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    mkdirSync(config.llmModelsDir, { recursive: true });
    writeFileSync(join(config.llmModelsDir, `${modelId}.gguf`), "model");

    expect(installedModels(config, "llm")).toEqual([modelId]);
  });
});

describe("parallel configuration persistence", () => {
  test("migrates legacy SQLite config and round-trips auto and manual values", () => {
    const root = createLegacyConfigRoot();
    const config = loadConfig(root);

    expect(config.parallel).toBe("auto");

    config.parallel = 4;
    saveConfig(config);
    expect(loadConfig(root).parallel).toBe(4);

    config.parallel = "auto";
    saveConfig(config);
    expect(loadConfig(root).parallel).toBe("auto");
  });
});

describe("platform support tiers", () => {
  test("classifies managed, CLI-only, and unsupported targets", () => {
    expect(platformSupportTier({ os: "darwin", cpu: "arm64" })).toBe("managed");
    expect(platformSupportTier({ os: "linux", cpu: "x64" })).toBe("managed");
    expect(platformSupportTier({ os: "darwin", cpu: "x64" })).toBe("cli-only");
    expect(platformSupportTier({ os: "linux", cpu: "arm64" })).toBe("cli-only");
    expect(platformSupportTier({ os: "win32", cpu: "x64" })).toBe(
      "unsupported",
    );
  });

  test("explains how to provide missing CLI-only runtimes", () => {
    expect(
      managedRuntimeUnavailableError(
        "whisper-server",
        { os: "darwin", cpu: "x64" },
        "/tmp/local-base/bin",
      ).message,
    ).toBe(
      "LocalBase CLI-only compatibility on macOS x64 does not include a managed whisper-server runtime. Place a compatible whisper-server executable in /tmp/local-base/bin/whisper-server or on PATH.",
    );
    expect(
      managedRuntimeUnavailableError(
        "sd-server",
        { os: "linux", cpu: "arm64" },
        "/tmp/local-base/bin",
      ).message,
    ).toContain("/tmp/local-base/bin/sd-server or on PATH");
    expect(
      managedRuntimeUnavailableError(
        "sd-server",
        { os: "win32", cpu: "x64" },
        "/tmp/local-base/bin",
      ).message,
    ).toContain("/tmp/local-base/bin/sd-server or on PATH");
  });
});

describe("llama server argument construction", () => {
  test("passes exact argv to async startup and logs auto allocation", async () => {
    const fixture = await createLlamaLaunchFixture("auto");
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...values: unknown[]) => output.push(values.join(" "));

    try {
      const process = await startLlamaServerProcess(
        fixture.config,
        fixture.modelFile,
        "127.0.0.1",
        18000,
        8192,
        { memoryGb: 9.5 },
      );
      expect(await process.exited).toBe(0);
    } finally {
      console.log = originalLog;
    }

    expect(readCapturedArgs(fixture.argsPath)).toEqual(
      expectedLlamaArgs(fixture.modelPath, "2"),
    );
    expect(
      output.filter((line) => line.includes("Dynamic Concurrency")),
    ).toEqual([
      "🤖 Dynamic Concurrency: Calculated 2 parallel slots based on 9.5 GB VRAM and context memory constraints. 4096 tokens per slot.",
    ]);
  });

  test("passes exact argv to synchronous startup", async () => {
    const fixture = await createLlamaLaunchFixture(3);

    expect(
      await launchLlamaServer(
        fixture.config,
        fixture.modelFile,
        "127.0.0.1",
        18000,
        8192,
      ),
    ).toBe(0);
    expect(readCapturedArgs(fixture.argsPath)).toEqual(
      expectedLlamaArgs(fixture.modelPath, "3"),
    );
  });
});
