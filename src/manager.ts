import { mkdirSync, readdirSync, renameSync, rmSync } from "node:fs";
import { homedir } from "node:os";
import { extname, isAbsolute, join, relative, resolve } from "node:path";
import { SafeFilenameSchema, verifyAuthoritativeFile } from "./utils/checksum";
import { eq } from "drizzle-orm";
import { z } from "zod";
import { databasePath as dbPath, withDatabase } from "./db/client";
import { apiKeysTable, configTable } from "./db/schema";
import {
  artifactDownloadUrl,
  byId,
  CATALOG,
  primaryArtifact,
  resolveCatalogInstallation,
  type ModelArtifact,
  type ModelSpec,
  type ModelKind,
  recommendedForVram,
  recommendedSttForVram,
} from "./catalog";
import {
  allocateParallelSlots,
  parseParallelSlots,
  type ParallelAllocation,
  type ParallelSlots,
} from "./domains/config/parallel";
import { ensureBinary } from "./manager/binaries";
export {
  ensureBinary,
  managedRuntimeRelease,
  managedRuntimeUnavailableError,
  platformSupportTier,
  type ManagedRuntimeRelease,
  type PlatformSupportTier,
  type PlatformTarget,
  type RuntimeName,
} from "./manager/binaries";

const textEncoder = new TextEncoder();

export type LocalBaseConfig = {
  root: string;
  llmModelsDir: string;
  sttModelsDir: string;
  imageModelsDir: string;
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
  selectedImageModels: string[];
  activeLlmModel: string;
  activeSttModel: string;
  activeImageModel: string;
  systemPrompt: string;
  hfToken: string;
  parallel: ParallelSlots;
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

const absolutePathSchema = z
  .string()
  .min(1)
  .refine(isAbsolute, "must be an absolute path")
  .refine((value) => resolve(value) === value, "must be normalized");
const hostSchema = z
  .string()
  .min(1)
  .max(253)
  .refine(
    (value) => value === value.trim() && !/\s/.test(value),
    "must not contain whitespace",
  );
const portSchema = z.number().int().min(1).max(65535);
const timestampSchema = z.iso.datetime({ offset: true });

const ConfigRowSchema = z
  .object({
    id: z.literal("default"),
    root: absolutePathSchema,
    llmModelsDir: absolutePathSchema,
    sttModelsDir: absolutePathSchema,
    imageModelsDir: absolutePathSchema,
    runtimeBackend: z.literal("llama.cpp"),
    sttBackend: z.literal("whisper.cpp"),
    host: hostSchema,
    port: portSchema,
    ctxSize: z.number().int().min(2048).max(2_147_483_647),
    sttHost: hostSchema,
    sttPort: portSchema,
    startupOnBoot: z.union([z.literal(0), z.literal(1)]),
    selectedLlmModels: z.string(),
    selectedSttModels: z.string(),
    selectedImageModels: z.string(),
    activeLlmModel: z.string().min(1),
    activeSttModel: z.string(),
    activeImageModel: z.string(),
    systemPrompt: z.string(),
    hfToken: z.string(),
    parallel: z.enum(["auto", "1", "2", "3", "4"]),
  })
  .strict();

const ApiKeyRowSchema = z
  .object({
    id: z.string().min(1),
    name: z.string().min(1),
    prefix: z.string().min(1),
    keyHash: z.string().regex(/^[a-fA-F0-9]{64}$/),
    createdAt: timestampSchema,
    lastRotatedAt: timestampSchema,
    expiresAt: timestampSchema.nullable(),
    revokedAt: timestampSchema.nullable(),
  })
  .strict()
  .superRefine((key, ctx) => {
    const createdAt = Date.parse(key.createdAt);
    if (Date.parse(key.lastRotatedAt) < createdAt) {
      ctx.addIssue({
        code: "custom",
        path: ["lastRotatedAt"],
        message: "must not be before createdAt",
      });
    }
    if (key.revokedAt && Date.parse(key.revokedAt) < createdAt) {
      ctx.addIssue({
        code: "custom",
        path: ["revokedAt"],
        message: "must not be before createdAt",
      });
    }
  });

function issueSummary(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "value"}: ${issue.message}`)
    .join("; ");
}

function invalidConfiguration(
  root: string,
  detail: string,
  cause?: unknown,
): Error {
  return new Error(
    `Invalid LocalBase configuration in ${dbPath(root)}: ${detail}. ` +
      `Repair the invalid row or run "local-base reset --root ${root} --yes" to recreate the database.`,
    cause === undefined ? undefined : { cause },
  );
}

function toConfigRow(config: LocalBaseConfig) {
  return {
    id: "default",
    root: config.root,
    llmModelsDir: config.llmModelsDir,
    sttModelsDir: config.sttModelsDir,
    imageModelsDir: config.imageModelsDir,
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
    selectedImageModels: JSON.stringify(config.selectedImageModels),
    activeLlmModel: config.activeLlmModel,
    activeSttModel: config.activeSttModel,
    activeImageModel: config.activeImageModel,
    systemPrompt: config.systemPrompt,
    hfToken: config.hfToken || "",
    parallel: String(parseParallelSlots(config.parallel)),
  };
}

function modelHasExpectedModalities(
  model: ModelSpec,
  kind: ModelKind,
): boolean {
  const expected =
    kind === "llm"
      ? { input: "text", output: "text" }
      : kind === "stt"
        ? { input: "audio", output: "text" }
        : { input: "text", output: "image" };
  return (
    model.inputModalities.includes(expected.input) &&
    model.outputModalities.includes(expected.output)
  );
}

function selectedModelsSchema(kind: ModelKind, requireOne: boolean) {
  const schema = z
    .array(
      z.string().refine(
        (id) => {
          const model = byId(id);
          return (
            !!model &&
            model.kind === kind &&
            modelHasExpectedModalities(model, kind)
          );
        },
        {
          message: `must name a catalog ${kind} model with compatible modalities`,
        },
      ),
    )
    .refine(
      (ids) => new Set(ids).size === ids.length,
      "must not contain duplicates",
    );
  return requireOne ? schema.min(1) : schema;
}

function parseSelectedModels(
  value: string,
  field: string,
  kind: ModelKind,
  requireOne: boolean,
  root: string,
): string[] {
  let json: unknown;
  try {
    json = JSON.parse(value);
  } catch (error) {
    throw invalidConfiguration(root, `${field} contains malformed JSON`, error);
  }
  const parsed = selectedModelsSchema(kind, requireOne).safeParse(json);
  if (!parsed.success) {
    throw invalidConfiguration(root, `${field}: ${issueSummary(parsed.error)}`);
  }
  return parsed.data;
}

function pathWithin(root: string, path: string): boolean {
  const child = relative(root, path);
  return child !== "" && !child.startsWith("..") && !isAbsolute(child);
}

function fromConfigRow(row: unknown, openedRoot: string): LocalBaseConfig {
  const parsed = ConfigRowSchema.safeParse(row);
  if (!parsed.success) {
    throw invalidConfiguration(openedRoot, issueSummary(parsed.error));
  }
  const data = parsed.data;
  if (data.root !== openedRoot) {
    throw invalidConfiguration(
      openedRoot,
      `root is ${JSON.stringify(data.root)} but this database was opened for ${JSON.stringify(openedRoot)}`,
    );
  }
  for (const [field, path] of [
    ["llmModelsDir", data.llmModelsDir],
    ["sttModelsDir", data.sttModelsDir],
    ["imageModelsDir", data.imageModelsDir],
  ] as const) {
    if (!pathWithin(openedRoot, path)) {
      throw invalidConfiguration(
        openedRoot,
        `${field} must be inside the configured root`,
      );
    }
  }

  const selectedLlmModels = parseSelectedModels(
    data.selectedLlmModels,
    "selectedLlmModels",
    "llm",
    true,
    openedRoot,
  );
  const selectedSttModels = parseSelectedModels(
    data.selectedSttModels,
    "selectedSttModels",
    "stt",
    false,
    openedRoot,
  );
  const selectedImageModels = parseSelectedModels(
    data.selectedImageModels,
    "selectedImageModels",
    "image",
    false,
    openedRoot,
  );
  const activeModels = [
    ["activeLlmModel", data.activeLlmModel, "llm", selectedLlmModels, false],
    ["activeSttModel", data.activeSttModel, "stt", selectedSttModels, true],
    [
      "activeImageModel",
      data.activeImageModel,
      "image",
      selectedImageModels,
      true,
    ],
  ] as const;
  for (const [field, id, kind, selected, optional] of activeModels) {
    if (optional && id === "") continue;
    const model = byId(id);
    if (
      !model ||
      model.kind !== kind ||
      !modelHasExpectedModalities(model, kind)
    ) {
      throw invalidConfiguration(
        openedRoot,
        `${field} must name a catalog ${kind} model with compatible modalities`,
      );
    }
    if (!selected.includes(id)) {
      throw invalidConfiguration(
        openedRoot,
        `${field} must also be present in its selected model list`,
      );
    }
  }

  return {
    root: data.root,
    llmModelsDir: data.llmModelsDir,
    sttModelsDir: data.sttModelsDir,
    imageModelsDir: data.imageModelsDir,
    runtimeBackend: data.runtimeBackend,
    sttBackend: data.sttBackend,
    host: data.host,
    port: data.port,
    ctxSize: data.ctxSize,
    sttHost: data.sttHost,
    sttPort: data.sttPort,
    startupOnBoot: data.startupOnBoot === 1,
    selectedLlmModels,
    selectedSttModels,
    selectedImageModels,
    activeLlmModel: data.activeLlmModel,
    activeSttModel: data.activeSttModel,
    activeImageModel: data.activeImageModel,
    systemPrompt: data.systemPrompt,
    hfToken: data.hfToken,
    parallel: parseParallelSlots(data.parallel),
  };
}

function safeEqual(a: string, b: string): boolean {
  const ab = textEncoder.encode(a);
  const bb = textEncoder.encode(b);
  if (ab.length !== bb.length) return false;
  return crypto.timingSafeEqual(ab, bb);
}

function isKeyActive(expiresAt?: string, revokedAt?: string): boolean {
  if (revokedAt) return false;
  if (!expiresAt) return true;
  const expiresMs = Date.parse(expiresAt);
  if (!Number.isFinite(expiresMs)) return false;
  return expiresMs > Date.now();
}
function hashApiKey(key: string): string {
  return new Bun.CryptoHasher("sha256").update(key).digest("hex");
}

function makeRawApiKey(): { key: string; prefix: string } {
  const bytes = new Uint8Array(24);
  crypto.getRandomValues(bytes);
  const raw = bytes.toBase64({ alphabet: "base64url", omitPadding: true });
  const prefix = raw.slice(0, 8);
  return { key: `lb_${raw}`, prefix };
}

export function defaultRoot(): string {
  return join(homedir(), ".local", "share", "local-base");
}

export function defaultConfig(root: string, vramGb = 0): LocalBaseConfig {
  root = resolve(root);
  const llm =
    recommendedForVram(vramGb)[0]?.modelId ??
    "qwen2.5-coder-7b-instruct-q4_k_m";
  const stt =
    recommendedSttForVram(vramGb)[2]?.modelId ??
    recommendedSttForVram(vramGb)[0]?.modelId ??
    "whisper-base-q8_0";
  const defaultCtxSize = 131072;
  return {
    root,
    llmModelsDir: join(root, "models", "llm"),
    sttModelsDir: join(root, "models", "stt"),
    imageModelsDir: join(root, "models", "image"),
    runtimeBackend: "llama.cpp",
    sttBackend: "whisper.cpp",
    host: "0.0.0.0",
    port: 18000,
    ctxSize: defaultCtxSize,
    sttHost: "0.0.0.0",
    sttPort: 18080,
    startupOnBoot: false,
    selectedLlmModels: [llm],
    selectedSttModels: [stt],
    selectedImageModels: ["stable-diffusion-v1-5"],
    activeLlmModel: llm,
    activeSttModel: stt,
    activeImageModel: "stable-diffusion-v1-5",
    systemPrompt: "",
    hfToken: "",
    parallel: "auto",
  };
}

export function ensureDirs(config: LocalBaseConfig): void {
  mkdirSync(config.root, { recursive: true });
  mkdirSync(config.llmModelsDir, { recursive: true });
  mkdirSync(config.sttModelsDir, { recursive: true });
  mkdirSync(config.imageModelsDir, { recursive: true });
}

export function saveConfig(config: LocalBaseConfig): void {
  const row = toConfigRow(config);
  fromConfigRow(row, config.root);
  ensureDirs(config);
  withDatabase(config.root, (db) => {
    db.insert(configTable)
      .values(row)
      .onConflictDoUpdate({
        target: configTable.id,
        set: row,
      })
      .run();
  });
}

export function initConfig(root?: string, vramGb?: number): LocalBaseConfig {
  const selectedRoot = resolve(root ?? defaultRoot());
  const config = defaultConfig(selectedRoot, vramGb ?? 0);
  saveConfig(config);
  return config;
}

export function loadConfig(root?: string, vramGb?: number): LocalBaseConfig {
  const selectedRoot = resolve(root ?? defaultRoot());
  const row = withDatabase(selectedRoot, (db) =>
    db.select().from(configTable).where(eq(configTable.id, "default")).get(),
  );
  if (!row) {
    return initConfig(selectedRoot, vramGb);
  }
  const config = fromConfigRow(row, selectedRoot);
  ensureDirs(config);
  return config;
}

export async function resetDatabase(
  root?: string,
  vramGb?: number,
): Promise<LocalBaseConfig> {
  const selectedRoot = resolve(root ?? defaultRoot());
  await deleteFileIfExists(dbPath(selectedRoot));
  return initConfig(selectedRoot, vramGb);
}

export function uninstallManaged(root?: string): string {
  const selectedRoot = resolve(root ?? defaultRoot());
  rmSync(selectedRoot, { recursive: true, force: true });
  return selectedRoot;
}

function kindDir(config: LocalBaseConfig, kind: ModelKind): string {
  if (kind === "llm") return config.llmModelsDir;
  if (kind === "stt") return config.sttModelsDir;
  return config.imageModelsDir;
}

export async function installedModels(
  config: LocalBaseConfig,
  kind?: ModelKind,
): Promise<string[]> {
  const kinds: ModelKind[] = ["llm", "stt", "image"];
  const selectedKinds = kind ? [kind] : kinds;
  const installed: string[] = [];
  for (const currentKind of selectedKinds) {
    const dir = kindDir(config, currentKind);
    let directoryEntries: string[];
    try {
      directoryEntries = readdirSync(dir);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") continue;
      throw error;
    }

    const catalogModels = CATALOG.filter((model) => model.kind === currentKind);
    const installationStates = await Promise.all(
      catalogModels.map(async (model) => ({
        model,
        state: await resolveCatalogInstallation(model, dir),
      })),
    );
    const completeModelIds = installationStates
      .filter(({ state }) => state.complete)
      .map(({ model }) => model.modelId);
    const knownArtifactNames = new Set(
      catalogModels.flatMap((model) =>
        model.artifacts.map((artifact) => artifact.filename),
      ),
    );
    const manualFiles = directoryEntries.filter(
      (name) =>
        [".gguf", ".bin", ".onnx", ".safetensors", ".pth"].includes(
          extname(name),
        ) && !knownArtifactNames.has(name),
    );

    installed.push(
      ...[...completeModelIds, ...manualFiles].map((name) =>
        kind ? name : `${currentKind}:${name}`,
      ),
    );
  }

  return installed.sort();
}

export async function installModel(
  config: LocalBaseConfig,
  modelId: string,
  filename?: string,
): Promise<string> {
  const spec = byId(modelId);
  if (!spec) {
    throw new Error(`Unknown model id: ${modelId}`);
  }

  const targetDir = kindDir(config, spec.kind);
  ensureDirs(config);
  mkdirSync(targetDir, { recursive: true });

  if (filename && spec.artifacts.length > 1) {
    throw new Error(
      "A filename override is not supported for multi-artifact models because it breaks shard discovery.",
    );
  }

  const primaryFilename = filename ?? primaryArtifact(spec).filename;
  for (const artifact of spec.artifacts) {
    const artifactFilename =
      spec.artifacts.length === 1 ? primaryFilename : artifact.filename;
    await installArtifact(config, spec, artifact, targetDir, artifactFilename);
  }

  return join(targetDir, primaryFilename);
}

type AuthoritativeArtifact = ModelArtifact & {
  expectedSizeBytes: number;
  sha256: string;
};

function authoritativeArtifact(
  artifact: ModelArtifact,
  modelId: string,
): AuthoritativeArtifact {
  if (
    artifact.expectedSizeBytes === undefined ||
    artifact.sha256 === undefined
  ) {
    throw new Error(
      `Managed model ${modelId} is missing authoritative size and SHA-256 metadata; installation is disabled until the catalog is repaired.`,
    );
  }
  return artifact as AuthoritativeArtifact;
}

async function validateArtifact(
  path: string,
  artifact: AuthoritativeArtifact,
  filename: string,
  cacheDir: string,
): Promise<void> {
  await verifyAuthoritativeFile(
    path,
    {
      filename,
      expectedSizeBytes: artifact.expectedSizeBytes,
      sha256: artifact.sha256,
    },
    cacheDir,
  );
}

async function deleteFileIfExists(path: string): Promise<void> {
  try {
    await Bun.file(path).delete();
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

async function removeIfEmpty(path: string): Promise<void> {
  try {
    if ((await Bun.file(path).stat()).size === 0) {
      await Bun.file(path).delete();
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

async function installArtifact(
  config: LocalBaseConfig,
  spec: ModelSpec,
  artifact: ModelArtifact,
  targetDir: string,
  filename: string,
): Promise<void> {
  SafeFilenameSchema.parse(filename);
  const authority = authoritativeArtifact(artifact, spec.modelId);
  const output = join(targetDir, filename);
  const partial = `${output}.partial`;

  if (await Bun.file(output).exists()) {
    const existingSize = (await Bun.file(output).stat()).size;
    if (existingSize < authority.expectedSizeBytes) {
      if (!(await Bun.file(partial).exists())) {
        renameSync(output, partial);
      } else {
        const partialSize = (await Bun.file(partial).stat()).size;
        if (
          partialSize > authority.expectedSizeBytes ||
          existingSize > partialSize
        ) {
          await Bun.file(partial).delete();
          renameSync(output, partial);
        } else {
          await Bun.file(output).delete();
        }
      }
    } else {
      try {
        await validateArtifact(output, authority, filename, targetDir);
        return;
      } catch {
        await deleteFileIfExists(output);
        await deleteFileIfExists(partial);
      }
    }
  }

  if (await Bun.file(partial).exists()) {
    const partialSize = (await Bun.file(partial).stat()).size;
    if (partialSize > authority.expectedSizeBytes) {
      await Bun.file(partial).delete();
    } else if (partialSize === authority.expectedSizeBytes) {
      try {
        await validateArtifact(partial, authority, filename, targetDir);
        renameSync(partial, output);
        return;
      } catch {
        await deleteFileIfExists(partial);
      }
    }
  }

  const url = artifactDownloadUrl(spec, artifact);
  console.log(`⬇️  Downloading model "${spec.modelId}" from ${url}...`);
  const curlArgs = ["-L", "--fail", "--continue-at", "-"];
  const token = config.hfToken || process.env.HF_TOKEN;
  if (token) {
    curlArgs.push("-H", `Authorization: Bearer ${token}`);
  }
  curlArgs.push("-o", partial, url);

  const proc = Bun.spawn(["curl", ...curlArgs], {
    stdout: "inherit",
    stderr: "inherit",
    stdin: "inherit",
  });
  const exitCode = await proc.exited;
  if (exitCode !== 0) {
    await removeIfEmpty(partial);
    if (
      !token &&
      (url.toLowerCase().includes("gemma") ||
        url.toLowerCase().includes("llama"))
    ) {
      throw new Error(
        `Failed to download model from ${url}.\n\n⚠️  This model is gated on Hugging Face. To download it:\n1. Accept the model terms on Hugging Face (e.g., https://huggingface.co/google/gemma-3-27b-it)\n2. Save your access token in the database: local-base configure --hf-token "your_token" (or set the HF_TOKEN environment variable)\n3. Re-run the command.`,
      );
    }
    throw new Error(`Failed to download model from ${url}`);
  }

  try {
    await validateArtifact(partial, authority, filename, targetDir);
  } catch (error) {
    await deleteFileIfExists(partial);
    throw error;
  }
  renameSync(partial, output);
}

export function loadApiKeys(config: LocalBaseConfig): ApiKeyRecord[] {
  return withDatabase(config.root, (db) =>
    db
      .select()
      .from(apiKeysTable)
      .all()
      .map((row) => fromApiKeyRow(row, config.root)),
  );
}

function fromApiKeyRow(row: unknown, root: string): ApiKeyRecord {
  const parsed = ApiKeyRowSchema.safeParse(row);
  if (!parsed.success) {
    const id =
      row && typeof row === "object" && "id" in row
        ? String((row as { id: unknown }).id)
        : "unknown";
    throw new Error(
      `Invalid API key configuration for ${id} in ${dbPath(root)}: ${issueSummary(parsed.error)}. ` +
        `Repair or revoke the row before accepting API authentication.`,
    );
  }
  return {
    ...parsed.data,
    expiresAt: parsed.data.expiresAt ?? undefined,
    revokedAt: parsed.data.revokedAt ?? undefined,
  };
}

export function validateApiKey(
  config: LocalBaseConfig,
  presentedKey: string,
): boolean {
  if (!presentedKey) return false;
  const presentedHash = hashApiKey(presentedKey);
  const keys = loadApiKeys(config);
  for (const key of keys) {
    if (!isKeyActive(key.expiresAt, key.revokedAt)) continue;
    if (safeEqual(key.keyHash, presentedHash)) return true;
  }
  return false;
}
export function createApiKey(
  config: LocalBaseConfig,
  name: string,
  expiresDays?: number,
): { record: ApiKeyRecord; rawKey: string } {
  if (!name.trim()) throw new Error("API key name must not be empty.");
  if (
    expiresDays !== undefined &&
    (!Number.isInteger(expiresDays) || expiresDays <= 0)
  ) {
    throw new Error("API key expiry must be a positive whole number of days.");
  }
  const now = new Date().toISOString();
  const { key, prefix } = makeRawApiKey();
  const record: ApiKeyRecord = {
    id: `key_${crypto.randomUUID()}`,
    name,
    prefix,
    keyHash: hashApiKey(key),
    createdAt: now,
    lastRotatedAt: now,
    expiresAt:
      expiresDays !== undefined
        ? new Date(Date.now() + expiresDays * 86400_000).toISOString()
        : undefined,
  };
  withDatabase(config.root, (db) => {
    db.insert(apiKeysTable)
      .values({
        id: record.id,
        name: record.name,
        prefix: record.prefix,
        keyHash: record.keyHash,
        createdAt: record.createdAt,
        lastRotatedAt: record.lastRotatedAt,
        expiresAt: record.expiresAt,
        revokedAt: null,
      })
      .run();
  });
  return { record, rawKey: key };
}

export function revokeApiKey(
  config: LocalBaseConfig,
  id: string,
): ApiKeyRecord {
  return withDatabase(config.root, (db) => {
    const record = db
      .select()
      .from(apiKeysTable)
      .where(eq(apiKeysTable.id, id))
      .get();
    if (!record) {
      throw new Error(`API key not found: ${id}`);
    }
    const validated = fromApiKeyRow(record, config.root);
    const revokedAt = new Date().toISOString();
    db.update(apiKeysTable)
      .set({ revokedAt })
      .where(eq(apiKeysTable.id, id))
      .run();
    return {
      ...validated,
      revokedAt,
    };
  });
}

export function rotateApiKey(
  config: LocalBaseConfig,
  id: string,
): { record: ApiKeyRecord; rawKey: string } {
  return withDatabase(config.root, (db) => {
    const record = db
      .select()
      .from(apiKeysTable)
      .where(eq(apiKeysTable.id, id))
      .get();
    if (!record) {
      throw new Error(`API key not found: ${id}`);
    }
    const validated = fromApiKeyRow(record, config.root);
    const { key, prefix } = makeRawApiKey();
    const lastRotatedAt = new Date().toISOString();
    const keyHash = hashApiKey(key);
    db.update(apiKeysTable)
      .set({ prefix, keyHash, lastRotatedAt, revokedAt: null })
      .where(eq(apiKeysTable.id, id))
      .run();

    return {
      record: {
        id: validated.id,
        name: validated.name,
        prefix,
        keyHash,
        createdAt: validated.createdAt,
        lastRotatedAt,
        expiresAt: validated.expiresAt,
        revokedAt: undefined,
      },
      rawKey: key,
    };
  });
}

export type ParallelHardware = {
  memoryGb: number;
};

export type LlamaServerArgs = {
  args: string[];
  parallel: ParallelAllocation;
};

function logAutoParallel(
  parallel: ParallelAllocation,
  hardware?: ParallelHardware,
): void {
  const memoryGb = hardware?.memoryGb ?? 0;
  console.log(
    `🤖 Dynamic Concurrency: Calculated ${parallel.slots} parallel slots based on ${memoryGb} GB VRAM and context memory constraints. ${parallel.contextPerSlot} tokens per slot.`,
  );
}

/** Builds llama-server arguments so launch paths share parallel-slot policy. */
export function buildLlamaServerArgs(
  config: LocalBaseConfig,
  modelPath: string,
  host: string,
  port: number,
  ctxSize: number,
  hardware?: ParallelHardware,
): LlamaServerArgs {
  const modelRequirementGb = byId(config.activeLlmModel)?.minVramGb;
  const parallel = allocateParallelSlots({
    parallel: config.parallel,
    memoryGb: hardware?.memoryGb ?? 0,
    modelRequirementGb,
    ctxSize,
  });
  const args = [
    "-m",
    modelPath,
    "--host",
    host,
    "--port",
    String(port),
    "-c",
    String(ctxSize),
    "--parallel",
    String(parallel.slots),
    // Force --jinja to parse model's embedded tokenizer template correctly instead of standard fallback.
    "--jinja",
    // Expose the /v1/embeddings endpoint for local vector indexing/search in coding clients.
    "--embeddings",
  ];

  // Enable --flash-attn on Apple Silicon GPUs for up to 2x faster prompt prefill and reduced VRAM.
  if (process.platform === "darwin" && process.arch === "arm64") {
    args.push("--flash-attn", "auto");
  }

  return { args, parallel };
}

/**
 * Spawns the llama-server background subprocess with memory/attention optimizations.
 */
export async function startLlamaServerProcess(
  config: LocalBaseConfig,
  modelFile: string,
  host: string,
  port: number,
  ctxSize: number,
  hardware?: ParallelHardware,
): Promise<Bun.Subprocess> {
  const modelPath = join(config.llmModelsDir, modelFile);
  if (!(await Bun.file(modelPath).exists())) {
    throw new Error(`Model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "llama-server");
  const launch = buildLlamaServerArgs(
    config,
    modelPath,
    host,
    port,
    ctxSize,
    hardware,
  );
  if (launch.parallel.isAuto) {
    logAutoParallel(launch.parallel, hardware);
  }

  return Bun.spawn([binPath, ...launch.args], {
    stdout: "pipe",
    stderr: "pipe",
    stdin: "inherit",
  });
}

export async function startWhisperServerProcess(
  config: LocalBaseConfig,
  modelFile: string,
  host: string,
  port: number,
): Promise<Bun.Subprocess> {
  const modelPath = join(config.sttModelsDir, modelFile);
  if (!(await Bun.file(modelPath).exists())) {
    throw new Error(`STT model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "whisper-server");

  return Bun.spawn(
    [binPath, "--model", modelPath, "--host", host, "--port", String(port)],
    {
      stdout: "pipe",
      stderr: "pipe",
      stdin: "inherit",
    },
  );
}

export async function startSdServerProcess(
  config: LocalBaseConfig,
  modelFile: string,
  host: string,
  port: number,
): Promise<Bun.Subprocess> {
  const modelPath = join(config.imageModelsDir, modelFile);
  if (!(await Bun.file(modelPath).exists())) {
    throw new Error(`Model file not found: ${modelPath}`);
  }

  const binPath = await ensureBinary(config, "sd-server");
  const binDir = join(config.root, "bin");
  const args = [
    binPath,
    "-m",
    modelPath,
    "--listen-ip",
    host,
    "--listen-port",
    String(port),
  ];

  return Bun.spawn(args, {
    stdout: "pipe",
    stderr: "pipe",
    stdin: "inherit",
    cwd: binDir,
  });
}
