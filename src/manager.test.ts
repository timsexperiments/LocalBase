import { afterEach, describe, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { readMigrationFiles } from "drizzle-orm/migrator";
import { chmodSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CATALOG, type ModelArtifact, type ModelSpec } from "./catalog";
import {
  createApiKey,
  defaultConfig,
  installModel,
  installedModels,
  loadApiKeys,
  loadConfig,
  managedRuntimeRelease,
  managedRuntimeUnavailableError,
  platformSupportTier,
  revokeApiKey,
  saveConfig,
  startLlamaServerProcess,
  validateApiKey,
  type LocalBaseConfig,
} from "./manager";
import { migrationsFolder } from "./db/migration-assets";
import {
  parseChecksumFile,
  readChecksumStore,
  verifyAuthoritativeFile,
  writeChecksumStore,
} from "./utils/checksum";

const testRoots: string[] = [];
const testModelIds: string[] = [];
const testServerClosers: Array<() => Promise<void>> = [];
const textEncoder = new TextEncoder();
const textBytes = (value: string) => textEncoder.encode(value);
const TEST_REVISION = "a".repeat(40);
const originalPath = process.env.PATH;

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
  return new Bun.CryptoHasher("sha256").update(content).digest("hex");
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
  const modelId = `test-install-${crypto.randomUUID()}`;
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
    repositoryRevision: TEST_REVISION,
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
  return `${new URL(source).pathname}/resolve/${TEST_REVISION}/${sourcePath}`;
}

function createUnsupportedConfigRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "local-base-unsupported-"));
  testRoots.push(root);
  const db = new Database(join(root, "local-base.db"));
  db.exec("create table config (id text primary key, root text not null);");
  db.close();
  return root;
}

function migrationJournal(root: string): Array<{
  hash: string;
  createdAt: number;
}> {
  const db = new Database(join(root, "local-base.db"));
  const rows = db
    .query(
      "SELECT hash, created_at AS createdAt FROM __drizzle_migrations ORDER BY created_at",
    )
    .all() as Array<{ hash: string; createdAt: number }>;
  db.close();
  return rows;
}

function generatedMigrationJournal(): Array<{
  hash: string;
  createdAt: number;
}> {
  return readMigrationFiles({ migrationsFolder: migrationsFolder() }).map(
    (migration) => ({
      hash: migration.hash,
      createdAt: migration.folderMillis,
    }),
  );
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
  const userBinDir = join(root, "user-bin");
  const binPath = join(userBinDir, "llama-server");
  const argsPath = join(userBinDir, "llama-server.args");
  mkdirSync(config.llmModelsDir, { recursive: true });
  mkdirSync(userBinDir, { recursive: true });
  await Bun.write(modelPath, "model placeholder");
  await Bun.write(
    binPath,
    `#!/bin/sh
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
printf '%s\\n' "$@" > "$script_dir/llama-server.args"
`,
  );
  chmodSync(binPath, 0o755);
  process.env.PATH = `${userBinDir}:${originalPath ?? ""}`;

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
  if (process.platform === "darwin" && process.arch === "arm64") {
    args.push("--flash-attn", "auto");
  }
  return args;
}

async function readCapturedArgs(argsPath: string): Promise<string[]> {
  return (await Bun.file(argsPath).text()).trim().split("\n");
}

afterEach(async () => {
  process.env.PATH = originalPath;
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

describe.serial("transactional model artifact installation", () => {
  test("keeps single-file installs compatible and supports a filename override", async () => {
    const content = textBytes("single model");
    const server = await createArtifactServer({
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model.gguf":
        content,
    });
    const modelId = installFixtureModel(server.source, [
      artifact("model.gguf", content, "primary"),
    ]);
    const config = createInstallConfig();

    const installed = await installModel(config, modelId, "renamed.gguf");

    expect(installed).toBe(join(config.llmModelsDir, "renamed.gguf"));
    expect(await Bun.file(installed).bytes()).toEqual(content);
    expect(
      await Bun.file(
        join(config.llmModelsDir, "renamed.gguf.partial"),
      ).exists(),
    ).toBe(false);
  });

  test("installs every shard sequentially and returns the primary artifact", async () => {
    const primary = textBytes("primary shard");
    const supplementary = textBytes("supplementary shard");
    const primaryArtifact = artifact("model-00001.gguf", primary, "primary");
    const supplementaryArtifact = artifact(
      "model-00002.gguf",
      supplementary,
      "supplementary",
    );
    const server = await createArtifactServer({
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00001.gguf":
        primary,
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00002.gguf":
        supplementary,
    });
    const modelId = installFixtureModel(server.source, [
      primaryArtifact,
      supplementaryArtifact,
    ]);
    const config = createInstallConfig();

    const installed = await installModel(config, modelId);

    expect(installed).toBe(join(config.llmModelsDir, primaryArtifact.filename));
    expect(await Bun.file(installed).bytes()).toEqual(primary);
    expect(
      await Bun.file(
        join(config.llmModelsDir, supplementaryArtifact.filename),
      ).bytes(),
    ).toEqual(supplementary);
    expect(server.requests.map((request) => request.path)).toEqual([
      artifactPath(server.source, primaryArtifact.sourcePath),
      artifactPath(server.source, supplementaryArtifact.sourcePath),
    ]);
  });

  test("skips a verified shard and resumes a truncated artifact with Range", async () => {
    const primary = textBytes("already verified");
    const supplementary = textBytes("resume this shard");
    const primaryArtifact = artifact("model-00001.gguf", primary, "primary");
    const supplementaryArtifact = artifact(
      "model-00002.gguf",
      supplementary,
      "supplementary",
    );
    const server = await createArtifactServer({
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00001.gguf":
        primary,
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00002.gguf":
        supplementary,
    });
    const modelId = installFixtureModel(server.source, [
      primaryArtifact,
      supplementaryArtifact,
    ]);
    const config = createInstallConfig();
    mkdirSync(config.llmModelsDir, { recursive: true });
    await Bun.write(
      join(config.llmModelsDir, primaryArtifact.filename),
      primary,
    );
    const partial = join(
      config.llmModelsDir,
      `${supplementaryArtifact.filename}.partial`,
    );
    const prefix = supplementary.subarray(0, 9);
    const shorterPartial = supplementary.subarray(0, 4);
    await Bun.write(
      join(config.llmModelsDir, supplementaryArtifact.filename),
      prefix,
    );
    await Bun.write(partial, shorterPartial);

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
      await Bun.file(
        join(config.llmModelsDir, supplementaryArtifact.filename),
      ).bytes(),
    ).toEqual(supplementary);
    expect(await Bun.file(partial).exists()).toBe(false);
  });

  test("preserves completed shards and partial failures, then repairs the set on retry", async () => {
    const primary = textBytes("primary shard");
    const supplementary = textBytes("supplementary shard that interrupts");
    const primaryArtifact = artifact("model-00001.gguf", primary, "primary");
    const supplementaryArtifact = artifact(
      "model-00002.gguf",
      supplementary,
      "supplementary",
    );
    const secondPath =
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00002.gguf";
    const server = await createArtifactServer(
      {
        "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00001.gguf":
          primary,
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
      await Bun.file(
        join(config.llmModelsDir, primaryArtifact.filename),
      ).bytes(),
    ).toEqual(primary);
    const partialSize = (await Bun.file(partial).stat()).size;
    expect(partialSize).toBeGreaterThan(0);

    await expect(installModel(config, modelId)).resolves.toBe(
      join(config.llmModelsDir, primaryArtifact.filename),
    );
    expect(
      await Bun.file(
        join(config.llmModelsDir, supplementaryArtifact.filename),
      ).bytes(),
    ).toEqual(supplementary);
    expect(await Bun.file(partial).exists()).toBe(false);
    expect(server.requests.at(-1)).toEqual({
      path: artifactPath(server.source, supplementaryArtifact.sourcePath),
      range: `bytes=${partialSize}-`,
    });
  });

  test("cleans partials when authoritative size or checksum validation fails", async () => {
    const wrongSize = textBytes("wrong size");
    const wrongHash = textBytes("wrong hash");
    const server = await createArtifactServer({
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/wrong-size.gguf":
        wrongSize,
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/wrong-hash.gguf":
        wrongHash,
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

    expect(
      await Bun.file(join(config.llmModelsDir, "wrong-size.gguf")).exists(),
    ).toBe(false);
    expect(
      await Bun.file(
        join(config.llmModelsDir, "wrong-size.gguf.partial"),
      ).exists(),
    ).toBe(false);
    expect(
      await Bun.file(join(config.llmModelsDir, "wrong-hash.gguf")).exists(),
    ).toBe(false);
    expect(
      await Bun.file(
        join(config.llmModelsDir, "wrong-hash.gguf.partial"),
      ).exists(),
    ).toBe(false);
  });

  test("rejects filename overrides for multi-artifact models before downloading", async () => {
    const primary = textBytes("primary");
    const supplementary = textBytes("supplementary");
    const server = await createArtifactServer({
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00001.gguf":
        primary,
      "/repo/resolve/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/model-00002.gguf":
        supplementary,
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

describe.serial("installed model reporting", () => {
  test("reports complete catalog sets once and preserves unmatched files", async () => {
    const primary = textBytes("primary");
    const supplementary = textBytes("supplementary");
    const modelId = installFixtureModel("https://example.com/models", [
      artifact("reporting-00001.gguf", primary, "primary"),
      artifact("reporting-00002.gguf", supplementary, "supplementary"),
    ]);
    const config = createInstallConfig();
    mkdirSync(config.llmModelsDir, { recursive: true });
    await Bun.write(join(config.llmModelsDir, "reporting-00001.gguf"), primary);
    await Bun.write(
      join(config.llmModelsDir, "reporting-00002.gguf"),
      supplementary,
    );
    await Bun.write(join(config.llmModelsDir, "z-manual.gguf"), "manual");

    expect(await installedModels(config, "llm")).toEqual([
      modelId,
      "z-manual.gguf",
    ]);
    expect(await installedModels(config)).toEqual([
      `llm:${modelId}`,
      "llm:z-manual.gguf",
    ]);

    await Bun.file(join(config.llmModelsDir, "reporting-00002.gguf")).delete();
    expect(await installedModels(config, "llm")).toEqual(["z-manual.gguf"]);

    await Bun.write(join(config.llmModelsDir, "reporting-00002.gguf"), "short");
    expect(await installedModels(config, "llm")).toEqual(["z-manual.gguf"]);
  });
});

describe.serial("parallel configuration persistence", () => {
  test("round-trips auto and manual parallel values", () => {
    const config = createInstallConfig();
    loadConfig(config.root);

    config.parallel = 4;
    saveConfig(config);
    expect(loadConfig(config.root).parallel).toBe(4);

    config.parallel = "auto";
    saveConfig(config);
    expect(loadConfig(config.root).parallel).toBe("auto");
  });
});

describe.serial("Drizzle database migrations", () => {
  test("migrates an empty database with the generated history", () => {
    const config = createInstallConfig();

    expect(loadConfig(config.root)).toEqual(config);
    expect(migrationJournal(config.root)).toEqual(generatedMigrationJournal());
  });

  test("rejects existing unmanaged schemas", () => {
    const root = createUnsupportedConfigRoot();

    expect(() => loadConfig(root)).toThrow();
  });

  test("rejects migration journals with a mismatched generated history", () => {
    const config = createInstallConfig();
    loadConfig(config.root);
    const db = new Database(join(config.root, "local-base.db"));
    db.prepare("UPDATE __drizzle_migrations SET hash = ?").run(
      "not-a-generated-migration-hash",
    );
    db.close();

    expect(() => loadConfig(config.root)).toThrow(
      "hashes or order do not match",
    );
  });
});

describe.serial("LocalBase database validation", () => {
  test("fails closed on invalid roots, paths, ports, and selected models", () => {
    const incompatibleId = installFixtureModel("https://example.com/models", [
      artifact("incompatible.gguf", textBytes("model"), "primary"),
    ]);
    const incompatible = (CATALOG as ModelSpec[]).find(
      (model) => model.modelId === incompatibleId,
    )!;
    incompatible.outputModalities = ["image"];
    const cases = [
      { column: "root", value: "/tmp/other-root", message: "root is" },
      {
        column: "llm_models_dir",
        value: "/tmp/models",
        message: "llmModelsDir",
      },
      { column: "port", value: 0, message: "port" },
      {
        column: "selected_llm_models",
        value: '["whisper-base-q8_0"]',
        message: "selectedLlmModels",
      },
      {
        column: "selected_llm_models",
        value: JSON.stringify([incompatibleId]),
        message: "compatible modalities",
      },
    ];

    for (const { column, value, message } of cases) {
      const config = createInstallConfig();
      saveConfig(config);
      const db = new Database(join(config.root, "local-base.db"));
      db.prepare(`UPDATE config SET ${column} = ? WHERE id = 'default'`).run(
        value,
      );
      db.close();

      expect(() => loadConfig(config.root)).toThrow(message);
    }
  });

  test("fails closed on malformed API-key rows and does not authenticate them", () => {
    const config = createInstallConfig();
    saveConfig(config);
    const { record, rawKey } = createApiKey(config, "test key");

    expect(validateApiKey(config, rawKey)).toBe(true);
    const db = new Database(join(config.root, "local-base.db"));
    db.prepare("UPDATE api_keys SET last_rotated_at = ? WHERE id = ?").run(
      "2020-01-01T00:00:00Z",
      record.id,
    );
    db.close();

    expect(() => validateApiKey(config, rawKey)).toThrow(
      "Invalid API key configuration",
    );
  });

  test("creates, revokes, and lists validated API keys", () => {
    const config = createInstallConfig();
    saveConfig(config);
    const { record, rawKey } = createApiKey(config, "deploy", 1);

    expect(validateApiKey(config, rawKey)).toBe(true);
    expect(loadApiKeys(config)).toEqual([record]);
    expect(revokeApiKey(config, record.id).revokedAt).toMatch(/^\d{4}-/);
    expect(validateApiKey(config, rawKey)).toBe(false);
  });
});

describe.serial("checksum inputs and continuity cache", () => {
  test("rejects malformed, unsafe, and duplicate external checksum rows", () => {
    expect(parseChecksumFile(`${"a".repeat(64)}  model.bin\n`)).toEqual(
      new Map([["model.bin", "a".repeat(64)]]),
    );
    expect(() => parseChecksumFile("not-a-checksum  model.bin\n")).toThrow(
      "line 1",
    );
    expect(() =>
      parseChecksumFile(`${"a".repeat(64)}  ../model.bin\n`),
    ).toThrow("line 1");
    expect(() =>
      parseChecksumFile(
        `${"a".repeat(64)}  model.bin\n${"b".repeat(64)}  model.bin\n`,
      ),
    ).toThrow("duplicate filename");
  });

  test("validates cache structure and rechecks when cached authority differs", async () => {
    const root = createInstallConfig().root;
    const file = join(root, "model.bin");
    await Bun.write(file, "verified model");
    const digest = await new Bun.CryptoHasher("sha256")
      .update("verified model")
      .digest("hex");
    const authority = {
      filename: "model.bin",
      expectedSizeBytes: (await Bun.file(file).stat()).size,
      sha256: digest,
    };

    await verifyAuthoritativeFile(file, authority, root);
    const cache = await readChecksumStore(root);
    cache.entries["model.bin"]!.authoritativeSha256 = "0".repeat(64);
    await writeChecksumStore(root, cache);
    await verifyAuthoritativeFile(file, authority, root);
    expect(
      (await readChecksumStore(root)).entries["model.bin"]?.authoritativeSha256,
    ).toBe(digest);

    await Bun.write(join(root, ".checksums.json"), "{not json");
    await expect(readChecksumStore(root)).rejects.toThrow(
      "Invalid continuity checksum cache",
    );
  });
});

describe.serial("platform support tiers", () => {
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
      "LocalBase CLI-only compatibility on macOS x64 does not include a managed whisper-server runtime. Place a compatible whisper-server executable on PATH outside /tmp/local-base/bin; it will be treated as user-managed and unverified.",
    );
    expect(
      managedRuntimeUnavailableError(
        "sd-server",
        { os: "linux", cpu: "arm64" },
        "/tmp/local-base/bin",
      ).message,
    ).toContain("on PATH outside /tmp/local-base/bin");
    expect(
      managedRuntimeUnavailableError(
        "sd-server",
        { os: "win32", cpu: "x64" },
        "/tmp/local-base/bin",
      ).message,
    ).toContain("on PATH outside /tmp/local-base/bin");
  });

  test("pins managed releases to a tag with independent size and digest metadata", () => {
    for (const [name, target] of [
      ["llama-server", { os: "darwin", cpu: "arm64" }],
      ["whisper-server", { os: "darwin", cpu: "arm64" }],
      ["sd-server", { os: "linux", cpu: "x64" }],
    ] as const) {
      const release = managedRuntimeRelease(name, target);
      expect(release).toBeDefined();
      expect(release?.url).toContain(`/download/${release?.tag}/`);
      expect(release?.url).not.toContain("/latest/");
      expect(release?.expectedSizeBytes).toBeGreaterThan(0);
      expect(release?.sha256).toMatch(/^[a-f0-9]{64}$/);
    }
  });
});

describe.serial("llama server argument construction", () => {
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

    expect(await readCapturedArgs(fixture.argsPath)).toEqual(
      expectedLlamaArgs(fixture.modelPath, "2"),
    );
    expect(
      output.filter((line) => line.includes("Dynamic Concurrency")),
    ).toEqual([
      "🤖 Dynamic Concurrency: Calculated 2 parallel slots based on 9.5 GB VRAM and context memory constraints. 4096 tokens per slot.",
    ]);
  });
});
