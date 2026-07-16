import { existsSync, mkdirSync, readdirSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { createHash, randomBytes, randomUUID, timingSafeEqual } from "node:crypto";
import { homedir, platform, arch } from "node:os";
import { basename, extname, join } from "node:path";
import { computeSha256, parseChecksumFile, readChecksumStore, recordChecksum, verifyChecksum, verifyStoredChecksum, writeChecksumStore } from "./utils/checksum";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { eq } from "drizzle-orm";
import { integer, sqliteTable, text } from "drizzle-orm/sqlite-core";
import { Database } from "bun:sqlite";
import { byId, type ModelKind, type ModelSpec, recommendedForVram, recommendedSttForVram } from "./catalog";

// ---------------------------------------------------------------------------
// Pinned upstream versions — update these when qualifying a new binary release.
// whisper-server is sourced from our own GitHub releases (built in CI).
// llama-server is sourced from the ggml-org/llama.cpp prebuilt releases.
// ---------------------------------------------------------------------------
const LLAMA_CPP_VERSION = "b9741";
const LOCALBASE_RELEASES_BASE = "https://github.com/timsexperiments/LocalBase/releases/latest/download";

export type LocalBaseConfig = {
  root: string;
  llmModelsDir: string;
  sttModelsDir: string;
  runtimeBackend: "llama.cpp";
  sttBackend: "whisper.cpp";
  host: string;
  port: number;
  ctxSize: number;
  sttHost: string;
  sttPort: number;
  startupOnBoot: boolean;
  selectedLlmModels: string[];
  selectedSttModels: string[];
  activeLlmModel: string;
  activeSttModel: string;
};

export type ApiKeyRecord = {
  id: string;
  name: string;
  prefix: string;
  keyHash: string;
  createdAt: string;
  lastRotatedAt: string;
  expiresAt?: string;
  revokedAt?: string;
};

const configTable = sqliteTable("config", {
  id: text("id").primaryKey(),
  root: text("root").notNull(),
  llmModelsDir: text("llm_models_dir").notNull(),
  sttModelsDir: text("stt_models_dir").notNull(),
  runtimeBackend: text("runtime_backend").notNull(),
  sttBackend: text("stt_backend").notNull(),
  host: text("host").notNull(),
  port: integer("port").notNull(),
  ctxSize: integer("ctx_size").notNull(),
  sttHost: text("stt_host").notNull(),
  sttPort: integer("stt_port").notNull(),
  startupOnBoot: integer("startup_on_boot").notNull(),
  selectedLlmModels: text("selected_llm_models").notNull(),
  selectedSttModels: text("selected_stt_models").notNull(),
  activeLlmModel: text("active_llm_model").notNull(),
  activeSttModel: text("active_stt_model").notNull()
});

const apiKeysTable = sqliteTable("api_keys", {
  id: text("id").primaryKey(),
  name: text("name").notNull(),
  prefix: text("prefix").notNull(),
  keyHash: text("key_hash").notNull(),
  createdAt: text("created_at").notNull(),
  lastRotatedAt: text("last_rotated_at").notNull(),
  expiresAt: text("expires_at"),
  revokedAt: text("revoked_at")
});

function dbPath(root: string): string {
  return join(root, "local-base.db");
}

function openDb(root: string) {
  mkdirSync(root, { recursive: true });
  const sqlite = new Database(dbPath(root));
  sqlite.exec(`
    create table if not exists config (
      id text primary key,
      root text not null,
      llm_models_dir text not null,
      stt_models_dir text not null,
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
      active_llm_model text not null,
      active_stt_model text not null
    );

    create table if not exists api_keys (
      id text primary key,
      name text not null,
      prefix text not null,
      key_hash text not null,
      created_at text not null,
      last_rotated_at text not null,
      expires_at text,
      revoked_at text
    );
  `);
  return drizzle(sqlite);
}

function toConfigRow(config: LocalBaseConfig) {
  return {
    id: "default",
    root: config.root,
    llmModelsDir: config.llmModelsDir,
    sttModelsDir: config.sttModelsDir,
    runtimeBackend: config.runtimeBackend,
    sttBackend: config.sttBackend,
    host: config.host,
    port: config.port,
    ctxSize: config.ctxSize,
    sttHost: config.sttHost,
    sttPort: config.sttPort,
    startupOnBoot: config.startupOnBoot ? 1 : 0,
    selectedLlmModels: JSON.stringify(config.selectedLlmModels),
    selectedSttModels: JSON.stringify(config.selectedSttModels),
    activeLlmModel: config.activeLlmModel,
    activeSttModel: config.activeSttModel
  };
}

function fromConfigRow(row: (typeof configTable.$inferSelect)): LocalBaseConfig {
  return {
    root: row.root,
    llmModelsDir: row.llmModelsDir,
    sttModelsDir: row.sttModelsDir,
    runtimeBackend: "llama.cpp",
    sttBackend: "whisper.cpp",
    host: row.host,
    port: row.port,
    ctxSize: row.ctxSize,
    sttHost: row.sttHost,
    sttPort: row.sttPort,
    startupOnBoot: row.startupOnBoot === 1,
    selectedLlmModels: JSON.parse(row.selectedLlmModels) as string[],
    selectedSttModels: JSON.parse(row.selectedSttModels) as string[],
    activeLlmModel: row.activeLlmModel,
    activeSttModel: row.activeSttModel
  };
}



function safeEqual(a: string, b: string): boolean {
  const ab = Buffer.from(a, "utf8");
  const bb = Buffer.from(b, "utf8");
  if (ab.length !== bb.length) return false;
  return timingSafeEqual(ab, bb);
}

function isKeyActive(expiresAt?: string, revokedAt?: string): boolean {
  if (revokedAt) return false;
  if (!expiresAt) return true;
  const expiresMs = Date.parse(expiresAt);
  if (!Number.isFinite(expiresMs)) return true;
  return expiresMs > Date.now();
}
function hashApiKey(key: string): string {
  return createHash("sha256").update(key).digest("hex");
}

function makeRawApiKey(): { key: string; prefix: string } {
  const raw = randomBytes(24).toString("base64url");
  const prefix = raw.slice(0, 8);
  return { key: `lb_${raw}`, prefix };
}

export function defaultRoot(): string {
  return join(homedir(), ".local", "share", "local-base");
}

function defaultConfig(root: string, vramGb = 0): LocalBaseConfig {
  const llm = recommendedForVram(vramGb)[0]?.modelId ?? "qwen2.5-coder-7b-instruct-q4_k_m";
  const stt = recommendedSttForVram(vramGb)[2]?.modelId ?? recommendedSttForVram(vramGb)[0]?.modelId ?? "whisper-base-q8_0";
  const defaultCtxSize = 131072;
  return {
    root,
    llmModelsDir: join(root, "models", "llm"),
    sttModelsDir: join(root, "models", "stt"),
    runtimeBackend: "llama.cpp",
    sttBackend: "whisper.cpp",
    host: "0.0.0.0",
    port: 8000,
    ctxSize: defaultCtxSize,
    sttHost: "0.0.0.0",
    sttPort: 8080,
    startupOnBoot: false,
    selectedLlmModels: [llm],
    selectedSttModels: [stt],
    activeLlmModel: llm,
    activeSttModel: stt
  };
}

export function ensureDirs(config: LocalBaseConfig): void {
  mkdirSync(config.root, { recursive: true });
  mkdirSync(config.llmModelsDir, { recursive: true });
  mkdirSync(config.sttModelsDir, { recursive: true });
}

export function saveConfig(config: LocalBaseConfig): void {
  ensureDirs(config);
  const db = openDb(config.root);
  const existing = db.select().from(configTable).where(eq(configTable.id, "default")).get();
  if (existing) {
    db.update(configTable).set(toConfigRow(config)).where(eq(configTable.id, "default")).run();
  } else {
    db.insert(configTable).values(toConfigRow(config)).run();
  }
}

export function initConfig(root?: string, vramGb?: number): LocalBaseConfig {
  const selectedRoot = root ?? defaultRoot();
  const config = defaultConfig(selectedRoot, vramGb ?? 0);
  saveConfig(config);
  return config;
}

export function loadConfig(root?: string, vramGb?: number): LocalBaseConfig {
  const selectedRoot = root ?? defaultRoot();
  const db = openDb(selectedRoot);
  const row = db.select().from(configTable).where(eq(configTable.id, "default")).get();
  if (!row) {
    return initConfig(selectedRoot, vramGb);
  }
  const merged = { ...defaultConfig(selectedRoot, vramGb ?? 0), ...fromConfigRow(row) } as LocalBaseConfig;
  ensureDirs(merged);
  return merged;
}

export function resetDatabase(root?: string, vramGb?: number): LocalBaseConfig {
  const selectedRoot = root ?? defaultRoot();
  const path = dbPath(selectedRoot);
  if (existsSync(path)) {
    rmSync(path, { force: true });
  }
  return initConfig(selectedRoot, vramGb);
}

export function uninstallManaged(root?: string): string {
  const selectedRoot = root ?? defaultRoot();
  if (existsSync(selectedRoot)) {
    rmSync(selectedRoot, { recursive: true, force: true });
  }
  return selectedRoot;
}

function kindDir(config: LocalBaseConfig, kind: ModelKind): string {
  if (kind === "llm") return config.llmModelsDir;
  if (kind === "stt") return config.sttModelsDir;
  return join(config.root, "models", kind);
}

export function installedModels(config: LocalBaseConfig, kind?: ModelKind): string[] {
  const dir = kind ? kindDir(config, kind) : undefined;
  if (dir) {
    if (!existsSync(dir)) return [];
    return readdirSync(dir)
      .filter((name) => [".gguf", ".bin", ".onnx", ".safetensors", ".pth"].includes(extname(name)))
      .sort();
  }

  const kinds: ModelKind[] = ["llm", "stt"];
  const files = kinds.flatMap((k) => {
    const d = kindDir(config, k);
    if (!existsSync(d)) return [];
    return readdirSync(d)
      .filter((name) => [".gguf", ".bin", ".onnx", ".safetensors", ".pth"].includes(extname(name)))
      .map((name) => `${k}:${name}`);
  });
  return files.sort();
}

function resolveDownload(spec: ModelSpec): string {
  const base = spec.source.replace(/\/$/, "");
  const path = spec.downloadPath ?? "resolve/main/model.gguf";
  return `${base}/${path.replace(/^\//, "")}`;
}

export async function installModel(config: LocalBaseConfig, modelId: string, filename?: string): Promise<string> {
  const spec = byId(modelId);
  if (!spec) {
    throw new Error(`Unknown model id: ${modelId}`);
  }

  const targetDir = kindDir(config, spec.kind);
  ensureDirs(config);
  mkdirSync(targetDir, { recursive: true });

  const url = resolveDownload(spec);
  const inferred = filename ?? spec.filename ?? basename(url);
  const output = join(targetDir, inferred);

  // If already installed, verify checksum integrity before returning.
  if (existsSync(output)) {
    const known = await verifyStoredChecksum(targetDir, inferred, output);
    if (!known) {
      // First run after upgrade or fresh install without a stored hash — record it.
      console.log(`📝 Recording checksum for existing model file "${inferred}"...`);
      await recordChecksum(targetDir, inferred, output);
    }
    return output;
  }

  console.log(`⬇️  Downloading model "${modelId}" from ${url}...`);
  const result = spawnSync("curl", ["-L", "--fail", "-o", output, url], { stdio: "inherit" });
  if (result.status !== 0) {
    try { rmSync(output, { force: true }); } catch {}
    throw new Error(`Failed to download model from ${url}`);
  }

  // Record SHA-256 of freshly downloaded file for future integrity checks.
  console.log(`📝 Recording checksum for "${inferred}"...`);
  await recordChecksum(targetDir, inferred, output);

  return output;
}

export function loadApiKeys(config: LocalBaseConfig): ApiKeyRecord[] {
  const db = openDb(config.root);
  return db.select().from(apiKeysTable).all().map((row) => ({
    id: row.id,
    name: row.name,
    prefix: row.prefix,
    keyHash: row.keyHash,
    createdAt: row.createdAt,
    lastRotatedAt: row.lastRotatedAt,
    expiresAt: row.expiresAt ?? undefined,
    revokedAt: row.revokedAt ?? undefined
  }));
}



export function validateApiKey(config: LocalBaseConfig, presentedKey: string): boolean {
  if (!presentedKey) return false;
  const presentedHash = hashApiKey(presentedKey);
  const keys = loadApiKeys(config);
  for (const key of keys) {
    if (!isKeyActive(key.expiresAt, key.revokedAt)) continue;
    if (safeEqual(key.keyHash, presentedHash)) return true;
  }
  return false;
}
export function createApiKey(config: LocalBaseConfig, name: string, expiresDays?: number): { record: ApiKeyRecord; rawKey: string } {
  const db = openDb(config.root);
  const now = new Date().toISOString();
  const { key, prefix } = makeRawApiKey();
  const record: ApiKeyRecord = {
    id: `key_${randomUUID()}`,
    name,
    prefix,
    keyHash: hashApiKey(key),
    createdAt: now,
    lastRotatedAt: now,
    expiresAt: expiresDays && expiresDays > 0 ? new Date(Date.now() + expiresDays * 86400_000).toISOString() : undefined
  };
  db.insert(apiKeysTable).values({
    id: record.id,
    name: record.name,
    prefix: record.prefix,
    keyHash: record.keyHash,
    createdAt: record.createdAt,
    lastRotatedAt: record.lastRotatedAt,
    expiresAt: record.expiresAt,
    revokedAt: null
  }).run();
  return { record, rawKey: key };
}

export function revokeApiKey(config: LocalBaseConfig, id: string): ApiKeyRecord {
  const db = openDb(config.root);
  const record = db.select().from(apiKeysTable).where(eq(apiKeysTable.id, id)).get();
  if (!record) {
    throw new Error(`API key not found: ${id}`);
  }
  const revokedAt = new Date().toISOString();
  db.update(apiKeysTable).set({ revokedAt }).where(eq(apiKeysTable.id, id)).run();
  return {
    id: record.id,
    name: record.name,
    prefix: record.prefix,
    keyHash: record.keyHash,
    createdAt: record.createdAt,
    lastRotatedAt: record.lastRotatedAt,
    expiresAt: record.expiresAt ?? undefined,
    revokedAt
  };
}

export function rotateApiKey(config: LocalBaseConfig, id: string): { record: ApiKeyRecord; rawKey: string } {
  const db = openDb(config.root);
  const record = db.select().from(apiKeysTable).where(eq(apiKeysTable.id, id)).get();
  if (!record) {
    throw new Error(`API key not found: ${id}`);
  }
  const { key, prefix } = makeRawApiKey();
  const lastRotatedAt = new Date().toISOString();
  const keyHash = hashApiKey(key);
  db.update(apiKeysTable)
    .set({ prefix, keyHash, lastRotatedAt, revokedAt: null })
    .where(eq(apiKeysTable.id, id))
    .run();

  return {
    record: {
      id: record.id,
      name: record.name,
      prefix,
      keyHash,
      createdAt: record.createdAt,
      lastRotatedAt,
      expiresAt: record.expiresAt ?? undefined,
      revokedAt: undefined
    },
    rawKey: key
  };
}



/**
 * Resolves a binary by checking the LocalBase-managed bin dir first, then PATH.
 * Returns null if the binary is not available anywhere on the system.
 */
export function resolveBinaryPath(config: LocalBaseConfig, name: string): string | null {
  const localBin = join(config.root, "bin", name);
  if (existsSync(localBin)) {
    return localBin;
  }

  const check = spawnSync("which", [name], { encoding: "utf8" });
  if (check.status === 0 && check.stdout.trim()) {
    return check.stdout.trim();
  }

  return null;
}

/**
 * Downloads a file from the given URL using curl.
 * Throws if the download fails.
 */
function curlDownload(url: string, dest: string): void {
  const result = spawnSync("curl", ["-L", "--fail", "-o", dest, url], { stdio: "inherit" });
  if (result.status !== 0) {
    throw new Error(`Failed to download ${url}`);
  }
}

/**
 * Returns the platform-specific asset name fragment used in binary release filenames.
 * e.g. "macos-arm64", "linux-x64".
 * Throws on unsupported platforms.
 */
function platformAssetSuffix(): string {
  const os = platform();
  const cpu = arch();
  if (os === "darwin" && cpu === "arm64") return "macos-arm64";
  if (os === "darwin" && cpu === "x64")   return "macos-x64";
  if (os === "linux"  && cpu === "x64")   return "linux-x64";
  if (os === "linux"  && cpu === "arm64") return "linux-arm64";
  throw new Error(
    `Unsupported platform for prebuilt binaries: ${os} ${cpu}.\n` +
    `Install llama-server / whisper-server manually and ensure they are on PATH.`
  );
}

/**
 * Downloads the whisper-server binary from the LocalBase GitHub release assets,
 * verifies its SHA-256 against the release checksums.txt, marks it executable,
 * records the checksum locally, and returns the installed binary path.
 */
async function downloadWhisperServer(config: LocalBaseConfig): Promise<string> {
  const binDir = join(config.root, "bin");
  mkdirSync(binDir, { recursive: true });

  const suffix   = platformAssetSuffix();
  const assetName = `whisper-server-${suffix}`;
  const binaryUrl  = `${LOCALBASE_RELEASES_BASE}/${assetName}`;
  const checksumUrl = `${LOCALBASE_RELEASES_BASE}/checksums.txt`;

  console.log(`\n⬇️  Fetching whisper-server checksums from LocalBase releases...`);
  const csRes = await fetch(checksumUrl, { headers: { "User-Agent": "LocalBase-CLI" } });
  if (!csRes.ok) {
    throw new Error(
      `Could not fetch checksums from LocalBase releases (${csRes.status}).\n` +
      `Ensure a release exists at ${LOCALBASE_RELEASES_BASE}.`
    );
  }
  const checksumMap = parseChecksumFile(await csRes.text());
  const expectedHash = checksumMap.get(assetName);
  if (!expectedHash) {
    throw new Error(`No checksum entry found for "${assetName}" in release checksums.txt.`);
  }

  const destPath = join(binDir, "whisper-server");
  console.log(`⬇️  Downloading ${assetName} from LocalBase releases...`);
  curlDownload(binaryUrl, destPath);

  await verifyChecksum(destPath, expectedHash, assetName);

  spawnSync("chmod", ["+x", destPath]);
  if (platform() === "darwin") {
    spawnSync("xattr", ["-rd", "com.apple.quarantine", destPath]);
  }

  // Record checksum for future integrity checks on this installation.
  const store = readChecksumStore(binDir);
  store["whisper-server"] = expectedHash;
  writeChecksumStore(binDir, store);

  console.log(`\n✅ whisper-server installed to ${destPath}`);
  return destPath;
}

/**
 * Downloads the llama-server prebuilt binary from the pinned ggml-org/llama.cpp
 * release, marks it executable, records the computed SHA-256 for future integrity
 * checks, and returns the installed binary path.
 */
async function downloadLlamaServer(config: LocalBaseConfig): Promise<string> {
  const binDir = join(config.root, "bin");
  mkdirSync(binDir, { recursive: true });

  const suffix = platformAssetSuffix();
  const assetName = `llama-${LLAMA_CPP_VERSION}-bin-${suffix === "macos-arm64" ? "macos-arm64" : suffix === "macos-x64" ? "macos-x64" : suffix === "linux-x64" ? "ubuntu-x64" : "ubuntu-arm64"}.tar.gz`;
  const url = `https://github.com/ggml-org/llama.cpp/releases/download/${LLAMA_CPP_VERSION}/${assetName}`;

  console.log(`\n⬇️  Downloading llama-server ${LLAMA_CPP_VERSION} (${suffix})...`);
  const archivePath = join(binDir, assetName);
  curlDownload(url, archivePath);

  console.log("Extracting llama.cpp release...");
  const ext = spawnSync("tar", ["-zxf", archivePath, "-C", binDir, "--strip-components=1"], { stdio: "inherit" });
  try { Bun.spawnSync(["rm", "-f", archivePath]); } catch {}
  if (ext.status !== 0) throw new Error("Failed to extract llama.cpp archive.");

  const destPath = join(binDir, "llama-server");
  if (!existsSync(destPath)) throw new Error("llama-server binary not found after extraction.");

  spawnSync("chmod", ["+x", destPath]);
  if (platform() === "darwin") {
    spawnSync("xattr", ["-rd", "com.apple.quarantine", binDir]);
  }


  // Record the SHA-256 we got from ggml-org for future integrity checks.
  await recordChecksum(binDir, "llama-server", destPath);
  console.log(`\n✅ llama-server ${LLAMA_CPP_VERSION} installed to ${destPath}`);
  return destPath;
}

/**
 * Ensures the named backend binary is available, in order:
 *  1. Locally installed in <root>/bin — verified against stored checksum.
 *  2. Available on system PATH — used as-is (user-managed installation).
 *  3. Downloaded from the appropriate prebuilt release and checksum-verified.
 */
export async function ensureBinary(config: LocalBaseConfig, name: "llama-server" | "whisper-server"): Promise<string> {
  const binDir = join(config.root, "bin");
  const localBin = join(binDir, name);

  // 1. Check locally managed binary and verify stored checksum.
  if (existsSync(localBin)) {
    const known = await verifyStoredChecksum(binDir, name, localBin);
    if (!known) {
      // Binary exists but no stored hash (e.g. manually placed) — record it.
      await recordChecksum(binDir, name, localBin);
    }
    return localBin;
  }

  // 2. Check system PATH.
  const systemBin = spawnSync("which", [name], { encoding: "utf8" });
  if (systemBin.status === 0 && systemBin.stdout.trim()) {
    console.log(`ℹ️  Using system-installed ${name} at ${systemBin.stdout.trim()}`);
    return systemBin.stdout.trim();
  }

  // 3. Download prebuilt release.
  if (name === "whisper-server") return downloadWhisperServer(config);
  return downloadLlamaServer(config);
}



/**
 * Spawns the llama-server background subprocess with memory/attention optimizations.
 */
export async function startLlamaServerProcess(config: LocalBaseConfig, modelFile: string, host: string, port: number, ctxSize: number): Promise<Bun.Subprocess> {
  const modelPath = join(config.llmModelsDir, modelFile);
  if (!existsSync(modelPath)) {
    throw new Error(`Model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "llama-server");
  const args = [
    binPath,
    "-m", modelPath,
    "--host", host,
    "--port", String(port),
    "-c", String(ctxSize),
    // Force --parallel 1 so the single active agent session gets the full context limit.
    // llama-server's default is 4, which splits context size equally among 4 slots.
    "--parallel", "1",
    // Force --jinja to parse model's embedded tokenizer template correctly instead of standard fallback.
    "--jinja",
    // Expose the /v1/embeddings endpoint for local vector indexing/search in coding clients.
    "--embeddings"
  ];

  // Enable --flash-attn on Apple Silicon GPUs for up to 2x faster prompt prefill and reduced VRAM.
  if (platform() === "darwin" && arch() === "arm64") {
    args.push("--flash-attn");
  }

  return Bun.spawn(args, {
    stdout: "pipe",
    stderr: "pipe",
    stdin: "inherit"
  });
}

export async function startWhisperServerProcess(config: LocalBaseConfig, modelFile: string, host: string, port: number): Promise<Bun.Subprocess> {
  const modelPath = join(config.sttModelsDir, modelFile);
  if (!existsSync(modelPath)) {
    throw new Error(`STT model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "whisper-server");

  return Bun.spawn([binPath, "--model", modelPath, "--host", host, "--port", String(port)], {
    stdout: "pipe",
    stderr: "pipe",
    stdin: "inherit"
  });
}

export async function launchLlamaServer(config: LocalBaseConfig, modelFile: string, host: string, port: number, ctxSize: number): Promise<number> {
  const modelPath = join(config.llmModelsDir, modelFile);
  if (!existsSync(modelPath)) {
    throw new Error(`Model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "llama-server");
  const args = [
    "-m", modelPath,
    "--host", host,
    "--port", String(port),
    "-c", String(ctxSize),
    "--parallel", "1",
    "--jinja",
    "--embeddings"
  ];

  if (platform() === "darwin" && arch() === "arm64") {
    args.push("--flash-attn");
  }

  const result = spawnSync(binPath, args, {
    stdio: "inherit"
  });

  return result.status ?? 1;
}

export async function launchWhisperServer(config: LocalBaseConfig, modelFile: string, host: string, port: number): Promise<number> {
  const modelPath = join(config.sttModelsDir, modelFile);
  if (!existsSync(modelPath)) {
    throw new Error(`STT model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "whisper-server");

  const result = spawnSync(binPath, ["--model", modelPath, "--host", host, "--port", String(port)], {
    stdio: "inherit"
  });

  return result.status ?? 1;
}
