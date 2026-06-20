import { existsSync, mkdirSync, readdirSync, rmSync, copyFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { createHash, randomBytes, randomUUID, timingSafeEqual } from "node:crypto";
import { homedir, platform, arch } from "node:os";
import { extname, join } from "node:path";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { eq } from "drizzle-orm";
import { integer, sqliteTable, text } from "drizzle-orm/sqlite-core";
import { Database } from "bun:sqlite";
import { byId, type ModelKind, type ModelSpec, recommendedForVram, recommendedSttForVram } from "./catalog";

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

  const kinds: ModelKind[] = ["llm", "stt", "tts", "image", "video", "audio"];
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

export function installModel(config: LocalBaseConfig, modelId: string, filename?: string): string {
  const spec = byId(modelId);
  if (!spec) {
    throw new Error(`Unknown model id: ${modelId}`);
  }

  const targetDir = kindDir(config, spec.kind);
  ensureDirs(config);
  mkdirSync(targetDir, { recursive: true });
  const inferred = filename ?? spec.filename ?? `${modelId}${spec.kind === "stt" ? ".gguf" : ".bin"}`;
  const output = join(targetDir, inferred);
  if (existsSync(output)) {
    return output;
  }

  const url = resolveDownload(spec);
  const result = spawnSync("curl", ["-L", "--fail", "-o", output, url], { stdio: "inherit" });
  if (result.status !== 0) {
    throw new Error(`Failed to download model from ${url}`);
  }

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
 * Checks for system-wide CMake, falling back to downloading and extracting
 * a portable, standalone CMake release binary to avoid external package manager dependencies (Homebrew/apt).
 */
export function ensureCMake(config: LocalBaseConfig): string {
  const systemCheck = spawnSync("which", ["cmake"], { encoding: "utf8" });
  if (systemCheck.status === 0 && systemCheck.stdout.trim()) {
    return systemCheck.stdout.trim();
  }

  const portableDir = join(config.root, "bin", "portable-cmake");
  const osName = platform();
  const cpuArch = arch();

  let cmakeBin = "";
  if (osName === "darwin") {
    cmakeBin = join(portableDir, "CMake.app", "Contents", "bin", "cmake");
  } else {
    cmakeBin = join(portableDir, "bin", "cmake");
  }

  if (existsSync(cmakeBin)) {
    return cmakeBin;
  }

  console.log("\n🛠️  CMake not found on system. Automatically installing portable CMake 3.31.5...");
  mkdirSync(portableDir, { recursive: true });

  let url = "";
  if (osName === "darwin") {
    url = "https://github.com/Kitware/CMake/releases/download/v3.31.5/cmake-3.31.5-macos-universal.tar.gz";
  } else if (osName === "linux" && cpuArch === "x64") {
    url = "https://github.com/Kitware/CMake/releases/download/v3.31.5/cmake-3.31.5-linux-x86_64.tar.gz";
  } else if (osName === "linux" && cpuArch === "arm64") {
    url = "https://github.com/Kitware/CMake/releases/download/v3.31.5/cmake-3.31.5-linux-aarch64.tar.gz";
  } else {
    throw new Error(`Unsupported platform/architecture for portable CMake: ${osName} ${cpuArch}`);
  }

  const archivePath = join(config.root, "bin", "cmake-temp.tar.gz");
  console.log(`Downloading CMake from ${url}...`);
  const download = spawnSync("curl", ["-L", "--fail", "-o", archivePath, url], { stdio: "inherit" });
  if (download.status !== 0) {
    throw new Error(`Failed to download portable CMake from ${url}`);
  }

  console.log("Extracting CMake...");
  const extract = spawnSync("tar", ["-zxf", archivePath, "-C", portableDir, "--strip-components=1"], { stdio: "inherit" });
  rmSync(archivePath, { force: true });

  if (extract.status !== 0) {
    throw new Error("Failed to extract portable CMake.");
  }

  if (osName === "darwin") {
    spawnSync("xattr", ["-rd", "com.apple.quarantine", portableDir]);
  }

  spawnSync("chmod", ["+x", cmakeBin]);

  if (!existsSync(cmakeBin)) {
    throw new Error(`Failed to verify portable CMake installation at ${cmakeBin}`);
  }

  console.log("✅ Portable CMake set up successfully.");
  return cmakeBin;
}

export async function fetchLatestLlamaReleaseTag(): Promise<string> {
  try {
    const res = await fetch("https://api.github.com/repos/ggml-org/llama.cpp/releases/latest", {
      headers: { "User-Agent": "LocalBase-CLI" }
    });
    if (res.ok) {
      const data = await res.json() as { tag_name: string };
      if (data.tag_name) return data.tag_name;
    }
  } catch (err) {
    // Ignore error
  }

  try {
    const htmlRes = await fetch("https://github.com/ggml-org/llama.cpp/releases");
    if (htmlRes.ok) {
      const text = await htmlRes.text();
      const match = text.match(/\/releases\/tag\/(b\d+)/);
      if (match && match[1]) return match[1];
    }
  } catch {
    // Ignore
  }

  return "b9692";
}

/**
 * Automatically provisions the backend binaries: downloads precompiled llama-server binaries
 * matching host architecture, or fallback clones and builds whisper-server from source.
 */
export async function compileBinary(config: LocalBaseConfig, name: "llama-server" | "whisper-server"): Promise<string> {
  const binDir = join(config.root, "bin");
  mkdirSync(binDir, { recursive: true });

  const tempDir = join(config.root, "temp-build");
  if (existsSync(tempDir)) {
    rmSync(tempDir, { recursive: true, force: true });
  }
  mkdirSync(tempDir, { recursive: true });

  const osName = platform();
  const cpuArch = arch();

  if (name === "llama-server") {
    try {
      console.log(`\n🔍 Checking for precompiled llama-server for ${osName}-${cpuArch}...`);
      const tag = await fetchLatestLlamaReleaseTag();
      
      let assetName = "";
      if (osName === "darwin" && cpuArch === "arm64") {
        assetName = `llama-${tag}-bin-macos-arm64.tar.gz`;
      } else if (osName === "darwin" && cpuArch === "x64") {
        assetName = `llama-${tag}-bin-macos-x64.tar.gz`;
      } else if (osName === "linux" && cpuArch === "x64") {
        assetName = `llama-${tag}-bin-ubuntu-x64.tar.gz`;
      } else if (osName === "linux" && cpuArch === "arm64") {
        assetName = `llama-${tag}-bin-ubuntu-arm64.tar.gz`;
      }

      if (assetName) {
        const url = `https://github.com/ggml-org/llama.cpp/releases/download/${tag}/${assetName}`;
        console.log(`Downloading precompiled llama-server from ${url}...`);
        const archivePath = join(tempDir, assetName);
        const dl = spawnSync("curl", ["-L", "--fail", "-o", archivePath, url], { stdio: "inherit" });
        if (dl.status === 0) {
          console.log("Extracting precompiled llama.cpp release...");
          const ext = spawnSync("tar", ["-zxf", archivePath, "-C", binDir, "--strip-components=1"], { stdio: "inherit" });
          if (ext.status === 0) {
            const binaryDest = join(binDir, "llama-server");
            if (existsSync(binaryDest)) {
              spawnSync("chmod", ["+x", binaryDest]);
              if (osName === "darwin") {
                spawnSync("xattr", ["-rd", "com.apple.quarantine", binDir]);
              }
              console.log(`\n✅ Successfully set up precompiled llama-server at ${binaryDest}`);
              try { rmSync(tempDir, { recursive: true, force: true }); } catch {}
              return binaryDest;
            }
          }
        }
      }
      console.log("⚠️  Precompiled binary not found or failed to download. Compiling from source...");
    } catch (err) {
      console.warn("⚠️  Failed to download precompiled binary:", err);
      console.log("Compiling from source...");
    }

    console.log("Cloning ggml-org/llama.cpp...");
    const clone = spawnSync("git", ["clone", "--depth", "1", "https://github.com/ggml-org/llama.cpp.git", "llama.cpp"], {
      cwd: tempDir,
      stdio: "inherit"
    });
    if (clone.status !== 0) throw new Error("Failed to clone llama.cpp repo.");

    console.log("Configuring llama.cpp with CMake...");
    const cmakeBin = ensureCMake(config);
    const configRes = spawnSync(cmakeBin, ["-B", "build", "-DLLAMA_BUILD_SERVER=ON", "-DLLAMA_BUILD_TESTS=OFF", "-DLLAMA_BUILD_EXAMPLES=ON"], {
      cwd: join(tempDir, "llama.cpp"),
      stdio: "inherit"
    });
    if (configRes.status !== 0) throw new Error("Failed to configure llama.cpp with CMake.");

    console.log("Compiling llama-server (this may take a minute)...");
    const buildRes = spawnSync(cmakeBin, ["--build", "build", "--config", "Release", "--target", "llama-server", "-j"], {
      cwd: join(tempDir, "llama.cpp"),
      stdio: "inherit"
    });
    if (buildRes.status !== 0) throw new Error("Failed to compile llama-server.");

    let binarySource = join(tempDir, "llama.cpp", "build", "bin", "llama-server");
    if (!existsSync(binarySource)) {
      binarySource = join(tempDir, "llama.cpp", "build", "llama-server");
    }
    if (!existsSync(binarySource)) {
      binarySource = join(tempDir, "llama.cpp", "build", "bin", "Release", "llama-server");
    }

    if (!existsSync(binarySource)) {
      throw new Error("Could not find compiled llama-server binary in build output.");
    }

    const binaryDest = join(binDir, "llama-server");
    copyFileSync(binarySource, binaryDest);

    const buildBinDir = join(tempDir, "llama.cpp", "build", "bin");
    if (existsSync(buildBinDir)) {
      const files = readdirSync(buildBinDir);
      for (const file of files) {
        if (file.endsWith(".dylib") || file.endsWith(".so")) {
          copyFileSync(join(buildBinDir, file), join(binDir, file));
        }
      }
    }

    spawnSync("chmod", ["+x", binaryDest]);
    if (osName === "darwin") {
      spawnSync("xattr", ["-rd", "com.apple.quarantine", binDir]);
    }
    console.log(`\n✅ Successfully compiled and installed llama-server to ${binaryDest}`);

  } else if (name === "whisper-server") {
    if (osName === "linux") {
      try {
        console.log(`\n🔍 Checking for precompiled whisper-server for ${osName}-${cpuArch}...`);
        const res = await fetch("https://api.github.com/repos/ggml-org/whisper.cpp/releases/latest", {
          headers: { "User-Agent": "LocalBase-CLI" }
        });
        if (res.ok) {
          const data = await res.json() as { tag_name: string };
          const tag = data.tag_name;
          if (tag) {
            let assetName = "";
            if (cpuArch === "x64") {
              assetName = "whisper-bin-ubuntu-x64.tar.gz";
            } else if (cpuArch === "arm64") {
              assetName = "whisper-bin-ubuntu-arm64.tar.gz";
            }

            if (assetName) {
              const url = `https://github.com/ggml-org/whisper.cpp/releases/download/${tag}/${assetName}`;
              console.log(`Downloading precompiled whisper-server from ${url}...`);
              const archivePath = join(tempDir, assetName);
              const dl = spawnSync("curl", ["-L", "--fail", "-o", archivePath, url], { stdio: "inherit" });
              if (dl.status === 0) {
                console.log("Extracting precompiled whisper.cpp release...");
                const ext = spawnSync("tar", ["-zxf", archivePath, "-C", binDir], { stdio: "inherit" });
                if (ext.status === 0) {
                  let binaryDest = join(binDir, "whisper-server");
                  if (!existsSync(binaryDest)) {
                    const find = spawnSync("find", [binDir, "-name", "whisper-server", "-o", "-name", "server"]);
                    if (find.status === 0 && find.stdout.toString().trim()) {
                      const foundPath = find.stdout.toString().trim().split("\n")[0];
                      if (foundPath !== binaryDest) {
                        copyFileSync(foundPath, binaryDest);
                      }
                    }
                  }
                  if (existsSync(binaryDest)) {
                    spawnSync("chmod", ["+x", binaryDest]);
                    console.log(`\n✅ Successfully set up precompiled whisper-server at ${binaryDest}`);
                    try { rmSync(tempDir, { recursive: true, force: true }); } catch {}
                    return binaryDest;
                  }
                }
              }
            }
          }
        }
      } catch (err) {
        console.warn("⚠️  Failed to download precompiled whisper-server:", err);
      }
    }

    console.log("Cloning ggml-org/whisper.cpp...");
    const clone = spawnSync("git", ["clone", "--depth", "1", "https://github.com/ggml-org/whisper.cpp.git", "whisper.cpp"], {
      cwd: tempDir,
      stdio: "inherit"
    });
    if (clone.status !== 0) throw new Error("Failed to clone whisper.cpp repo.");

    console.log("Configuring whisper.cpp with CMake...");
    const cmakeBin = ensureCMake(config);
    const libDir = join(config.root, "lib");
    mkdirSync(libDir, { recursive: true });

    // Configure relative runpath (RPATH) so the compiled binary can find its sibling
    // dynamic libraries (.dylib/.so) under <root>/lib relative to the executable path.
    const rpath = osName === "darwin" ? "@loader_path/../lib" : "$ORIGIN/../lib";
    const configRes = spawnSync(cmakeBin, [
      "-B", "build",
      "-DWHISPER_BUILD_TESTS=OFF",
      "-DWHISPER_BUILD_EXAMPLES=ON",
      `-DCMAKE_INSTALL_RPATH=${rpath}`,
      "-DCMAKE_INSTALL_PREFIX=" + config.root
    ], {
      cwd: join(tempDir, "whisper.cpp"),
      stdio: "inherit"
    });
    if (configRes.status !== 0) throw new Error("Failed to configure whisper.cpp with CMake.");

    console.log("Compiling whisper.cpp (this may take a minute)...");
    const buildRes = spawnSync(cmakeBin, ["--build", "build", "--config", "Release", "-j"], {
      cwd: join(tempDir, "whisper.cpp"),
      stdio: "inherit"
    });
    if (buildRes.status !== 0) throw new Error("Failed to compile whisper.cpp.");

    console.log("Installing whisper.cpp...");
    const installRes = spawnSync(cmakeBin, ["--install", "build", "--prefix", config.root], {
      cwd: join(tempDir, "whisper.cpp"),
      stdio: "inherit"
    });
    if (installRes.status !== 0) throw new Error("Failed to install whisper.cpp.");

    const binaryDest = join(binDir, "whisper-server");
    if (!existsSync(binaryDest)) {
      const possibleName = join(binDir, "server");
      if (existsSync(possibleName)) {
        copyFileSync(possibleName, binaryDest);
        rmSync(possibleName, { force: true });
      }
    }

    if (!existsSync(binaryDest)) {
      throw new Error(`Failed to find whisper-server at ${binaryDest} after installation.`);
    }

    spawnSync("chmod", ["+x", binaryDest]);
    if (osName === "darwin") {
      spawnSync("xattr", ["-rd", "com.apple.quarantine", binDir]);
      spawnSync("xattr", ["-rd", "com.apple.quarantine", libDir]);
    }
    console.log(`\n✅ Successfully compiled and installed whisper-server to ${binaryDest}`);
  }

  try {
    rmSync(tempDir, { recursive: true, force: true });
  } catch {
    // ignore
  }

  return join(binDir, name);
}

export async function ensureBinary(config: LocalBaseConfig, name: "llama-server" | "whisper-server"): Promise<string> {
  const resolved = resolveBinaryPath(config, name);
  if (resolved) {
    return resolved;
  }
  return await compileBinary(config, name);
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
