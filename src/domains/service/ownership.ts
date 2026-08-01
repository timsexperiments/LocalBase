import { homedir } from "node:os";
import { chmod, lstat, mkdir, rename, rm } from "node:fs/promises";
import { isAbsolute, join, resolve } from "node:path";
import { z } from "zod";
import {
  canonicalLocalBaseRoot as canonicalRoot,
  canonicalLocalBaseRootSchema,
} from "../../utils/root";
import {
  gatewayHealthSchema,
  gatewayIdentitySchema,
  type GatewayHealth,
} from "../runtime/health";

export { canonicalLocalBaseRoot as canonicalRoot } from "../../utils/root";

const canonicalRootSchema = canonicalLocalBaseRootSchema;

const rootHashSchema = z.string().regex(/^[a-f0-9]{64}$/);
const serviceIdSchema = z
  .string()
  .regex(/^com\.localbase\.gateway\.[a-f0-9]{64}$/);

export const gatewayInstanceSchema = z
  .object({
    version: z.literal(2),
    instanceId: z.uuid(),
    root: canonicalRootSchema,
    rootHash: rootHashSchema,
    pid: z.number().int().positive(),
    startedAt: z.iso.datetime({ offset: true }),
    host: z.string().min(1).max(253),
    port: z.number().int().min(1).max(65_535),
    instanceToken: z.uuid(),
    serviceId: serviceIdSchema.optional(),
    serviceToken: z.uuid().optional(),
  })
  .strict()
  .refine(
    (instance) =>
      (instance.serviceId === undefined) ===
      (instance.serviceToken === undefined),
    "service identity requires both serviceId and serviceToken",
  );

const operationOwnerSchema = z
  .object({
    version: z.literal(1),
    operationId: z.uuid(),
    operation: z.string().min(1).max(64),
    root: canonicalRootSchema,
    rootHash: rootHashSchema,
    pid: z.number().int().positive(),
    startedAt: z.iso.datetime({ offset: true }),
    handoffToken: z.uuid().optional(),
    handoffExpiresAt: z.iso.datetime({ offset: true }).optional(),
  })
  .strict()
  .refine(
    (owner) =>
      (owner.handoffToken === undefined) ===
      (owner.handoffExpiresAt === undefined),
    "operation handoff requires both token and expiration",
  );

export type GatewayInstance = z.infer<typeof gatewayInstanceSchema>;

export type GatewayInstanceState =
  | { state: "missing"; root: string }
  | { state: "active"; root: string; instance: GatewayInstance }
  | { state: "stale"; root: string; instance: GatewayInstance }
  | {
      state: "unknown";
      root: string;
      instance?: GatewayInstance;
      detail: string;
    }
  | { state: "invalid"; root: string; detail: string };

export class RootOwnershipError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "RootOwnershipError";
  }
}

type RootPaths = {
  runtimeDir: string;
  gatewayLockDir: string;
  gatewayOwnerPath: string;
};

type OperationPaths = {
  coordinationDir: string;
  operationLockDir: string;
  operationOwnerPath: string;
  mutationLockDir: string;
};

type ProcessPresence = "present" | "absent" | "unknown";

export function canonicalRootHash(root: string): string {
  canonicalRootSchema.parse(root);
  return new Bun.CryptoHasher("sha256").update(root).digest("hex");
}

function rootPaths(root: string): RootPaths {
  const runtimeDir = join(root, "runtime");
  const gatewayLockDir = join(runtimeDir, "gateway.lock");
  return {
    runtimeDir,
    gatewayLockDir,
    gatewayOwnerPath: join(gatewayLockDir, "owner.json"),
  };
}

function defaultCoordinationDirectory(): string {
  const runtimeHome = process.env.XDG_RUNTIME_DIR;
  if (runtimeHome && isAbsolute(runtimeHome)) {
    return resolve(runtimeHome, "local-base");
  }
  const userHome = resolve(process.env.HOME || homedir());
  return process.platform === "darwin"
    ? join(userHome, "Library", "Application Support", "LocalBase", "runtime")
    : join(userHome, ".local", "state", "local-base", "runtime");
}

function operationPaths(
  root: string,
  coordinationDirectory = defaultCoordinationDirectory(),
): OperationPaths {
  const coordinationDir = resolve(coordinationDirectory);
  canonicalRootSchema.parse(coordinationDir);
  const operationLockDir = join(
    coordinationDir,
    `${canonicalRootHash(root)}.operation.lock`,
  );
  return {
    coordinationDir,
    operationLockDir,
    operationOwnerPath: join(operationLockDir, "owner.json"),
    mutationLockDir: `${operationLockDir}.mutation`,
  };
}

function processPresence(pid: number): ProcessPresence {
  try {
    process.kill(pid, 0);
    return "present";
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ESRCH") return "absent";
    if (code === "EPERM") return "unknown";
    throw error;
  }
}

async function fileJson(path: string): Promise<unknown | undefined> {
  try {
    const file = Bun.file(path);
    if (!(await file.exists())) return undefined;
    return JSON.parse(await file.text()) as unknown;
  } catch {
    return undefined;
  }
}

async function writeJsonAtomically(
  path: string,
  value: unknown,
): Promise<void> {
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  try {
    await Bun.write(temporary, JSON.stringify(value));
    await chmod(temporary, 0o600);
    await rename(temporary, path);
  } finally {
    await rm(temporary, { force: true });
  }
}

async function directoryExists(path: string): Promise<boolean> {
  try {
    const info = await lstat(path);
    return info.isDirectory() && !info.isSymbolicLink();
  } catch {
    return false;
  }
}

async function ensurePrivateDirectory(path: string): Promise<void> {
  await mkdir(path, { recursive: true, mode: 0o700 });
  const info = await lstat(path);
  if (!info.isDirectory() || info.isSymbolicLink()) {
    throw new RootOwnershipError(
      `Service coordination path is not a private directory: ${path}.`,
    );
  }
  const uid =
    typeof process.getuid === "function" ? process.getuid() : undefined;
  if (uid !== undefined && info.uid !== uid) {
    throw new RootOwnershipError(
      `Service coordination path is not owned by the current user: ${path}.`,
    );
  }
  if ((info.mode & 0o077) !== 0) await chmod(path, 0o700);
}

function gatewayHost(host: string): string {
  if (host === "0.0.0.0") return "127.0.0.1";
  if (host === "::" || host === "[::]") return "[::1]";
  return host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
}

export function gatewayHealthUrl(instance: GatewayInstance): string {
  return `http://${gatewayHost(instance.host)}:${instance.port}/health`;
}

function gatewayIdentityUrl(instance: GatewayInstance): string {
  return `http://${gatewayHost(instance.host)}:${instance.port}/_localbase/instance`;
}

async function readGatewayInstanceAtRoot(
  root: string,
): Promise<
  | { state: "missing"; root: string }
  | { state: "record"; root: string; instance: GatewayInstance }
  | { state: "unknown"; root: string; detail: string }
  | { state: "invalid"; root: string; detail: string }
> {
  const canonical = canonicalRootSchema.parse(root);
  const paths = rootPaths(canonical);
  if (!(await directoryExists(paths.gatewayLockDir))) {
    return { state: "missing", root: canonical };
  }
  const ownerFile = Bun.file(paths.gatewayOwnerPath);
  if (!(await ownerFile.exists())) {
    return {
      state: "unknown",
      root: canonical,
      detail: "gateway ownership is being initialized",
    };
  }
  const raw = await fileJson(paths.gatewayOwnerPath);
  const parsed = gatewayInstanceSchema.safeParse(raw);
  if (!parsed.success) {
    return {
      state: "invalid",
      root: canonical,
      detail: "gateway owner metadata is invalid",
    };
  }
  const instance = parsed.data;
  if (
    instance.root !== canonical ||
    instance.rootHash !== canonicalRootHash(canonical)
  ) {
    return {
      state: "invalid",
      root: canonical,
      detail: "gateway owner metadata targets another root",
    };
  }
  return { state: "record", root: canonical, instance };
}

async function healthMatches(instance: GatewayInstance): Promise<boolean> {
  try {
    const response = await fetch(gatewayIdentityUrl(instance), {
      headers: { "x-localbase-instance-token": instance.instanceToken },
      signal: AbortSignal.timeout(750),
    });
    if (!response.ok) return false;
    const parsed = gatewayIdentitySchema.safeParse(await response.json());
    return (
      parsed.success &&
      parsed.data.instanceId === instance.instanceId &&
      parsed.data.rootHash === instance.rootHash
    );
  } catch {
    return false;
  }
}

export async function readGatewayHealth(
  instance: GatewayInstance,
): Promise<GatewayHealth | undefined> {
  try {
    const response = await fetch(gatewayHealthUrl(instance), {
      signal: AbortSignal.timeout(750),
    });
    if (!response.ok) return undefined;
    const parsed = gatewayHealthSchema.safeParse(await response.json());
    return parsed.success ? parsed.data : undefined;
  } catch {
    return undefined;
  }
}

export async function getGatewayInstanceState(
  root: string,
): Promise<GatewayInstanceState> {
  const canonical = await canonicalRoot(root);
  return await getGatewayInstanceStateAtRoot(canonical);
}

export async function getGatewayInstanceStateAtRoot(
  root: string,
): Promise<GatewayInstanceState> {
  const canonical = canonicalRootSchema.parse(root);
  const record = await readGatewayInstanceAtRoot(canonical);
  if (record.state !== "record") return record;

  const presence = processPresence(record.instance.pid);
  if (presence === "absent") {
    return { state: "stale", root: canonical, instance: record.instance };
  }
  if (presence === "unknown") {
    return {
      state: "unknown",
      root: canonical,
      instance: record.instance,
      detail: "gateway process identity cannot be verified",
    };
  }
  if (!(await healthMatches(record.instance))) {
    return {
      state: "unknown",
      root: canonical,
      instance: record.instance,
      detail:
        "gateway process is present but did not prove its instance identity",
    };
  }

  const current = await readGatewayInstanceAtRoot(canonical);
  if (
    current.state !== "record" ||
    current.instance.instanceId !== record.instance.instanceId
  ) {
    return {
      state: "unknown",
      root: canonical,
      detail: "gateway ownership changed during the health probe",
    };
  }
  return { state: "active", root: canonical, instance: current.instance };
}

async function removeStaleGatewayLease(
  root: string,
  expected: GatewayInstance,
): Promise<boolean> {
  const current = await readGatewayInstanceAtRoot(root);
  if (
    current.state !== "record" ||
    current.instance.instanceId !== expected.instanceId ||
    processPresence(current.instance.pid) !== "absent"
  ) {
    return false;
  }
  await rm(rootPaths(root).gatewayLockDir, { recursive: true, force: true });
  return true;
}

export async function clearStaleGatewayLeaseAtRoot(
  root: string,
): Promise<boolean> {
  const state = await getGatewayInstanceStateAtRoot(root);
  return state.state === "stale"
    ? await removeStaleGatewayLease(root, state.instance)
    : false;
}

export type GatewayLease = {
  root: string;
  instance: GatewayInstance;
  release(): Promise<void>;
};

export async function acquireGatewayLease(
  root: string,
  endpoint: {
    host: string;
    port: number;
    serviceId?: string;
    serviceToken?: string;
  },
): Promise<GatewayLease> {
  const canonical = await canonicalRoot(root);
  const parsedEndpoint = z
    .object({
      host: z.string().min(1).max(253),
      port: z.number().int().min(1).max(65_535),
      serviceId: serviceIdSchema.optional(),
      serviceToken: z.uuid().optional(),
    })
    .strict()
    .refine(
      (value) =>
        (value.serviceId === undefined) === (value.serviceToken === undefined),
      "service identity requires both serviceId and serviceToken",
    )
    .parse(endpoint);
  const paths = rootPaths(canonical);
  await ensurePrivateDirectory(paths.runtimeDir);

  for (let attempt = 0; attempt < 3; attempt += 1) {
    try {
      await mkdir(paths.gatewayLockDir, { mode: 0o700 });
      const instance = gatewayInstanceSchema.parse({
        version: 2,
        instanceId: crypto.randomUUID(),
        root: canonical,
        rootHash: canonicalRootHash(canonical),
        pid: process.pid,
        startedAt: new Date().toISOString(),
        host: parsedEndpoint.host,
        port: parsedEndpoint.port,
        instanceToken: crypto.randomUUID(),
        ...(parsedEndpoint.serviceId
          ? {
              serviceId: parsedEndpoint.serviceId,
              serviceToken: parsedEndpoint.serviceToken,
            }
          : {}),
      });
      await writeJsonAtomically(paths.gatewayOwnerPath, instance);
      return {
        root: canonical,
        instance,
        release: async () => {
          const current = await readGatewayInstanceAtRoot(canonical);
          if (
            current.state === "record" &&
            current.instance.instanceId === instance.instanceId
          ) {
            await rm(paths.gatewayLockDir, { recursive: true, force: true });
          }
        },
      };
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== "EEXIST" && code !== "ENOTEMPTY") throw error;
      const existing = await getGatewayInstanceStateAtRoot(canonical);
      if (existing.state === "stale") {
        if (await removeStaleGatewayLease(canonical, existing.instance)) {
          continue;
        }
      }
      if (existing.state === "unknown" && !existing.instance && attempt < 2) {
        await Bun.sleep(25);
        continue;
      }
      const detail =
        existing.state === "active"
          ? `instance ${existing.instance.instanceId} is active`
          : existing.state === "stale"
            ? "the stale lease changed before it could be reclaimed"
            : existing.state === "missing"
              ? "ownership changed during acquisition"
              : existing.detail;
      throw new RootOwnershipError(
        `A LocalBase gateway already owns ${canonical}, or its identity cannot be proven (${detail}). Stop the existing gateway before starting another one.`,
      );
    }
  }
  throw new RootOwnershipError(
    `Could not acquire LocalBase gateway ownership for ${canonical}.`,
  );
}

export type OperationLease = {
  operationId: string;
  handoff(serviceToken: string): Promise<void>;
  release(): Promise<void>;
};

const SERVICE_START_HANDOFF_MS = 10_000;

const mutationOwnerSchema = z
  .object({
    version: z.literal(1),
    token: z.uuid(),
    pid: z.number().int().positive(),
  })
  .strict();

async function acquireOperationMutation(
  paths: OperationPaths,
): Promise<() => Promise<void>> {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    const token = crypto.randomUUID();
    const candidate = `${paths.mutationLockDir}.${token}.candidate`;
    await mkdir(candidate, { mode: 0o700 });
    await writeJsonAtomically(
      join(candidate, "owner.json"),
      mutationOwnerSchema.parse({ version: 1, token, pid: process.pid }),
    );
    try {
      await rename(candidate, paths.mutationLockDir);
      return async () => {
        const owner = mutationOwnerSchema.safeParse(
          await fileJson(join(paths.mutationLockDir, "owner.json")),
        );
        if (owner.success && owner.data.token === token) {
          await rm(paths.mutationLockDir, { recursive: true, force: true });
        }
      };
    } catch (error) {
      await rm(candidate, { recursive: true, force: true });
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== "EEXIST" && code !== "ENOTEMPTY") throw error;
    }

    const owner = mutationOwnerSchema.safeParse(
      await fileJson(join(paths.mutationLockDir, "owner.json")),
    );
    if (owner.success && processPresence(owner.data.pid) === "absent") {
      const abandoned = `${paths.mutationLockDir}.${owner.data.token}.abandoned`;
      try {
        await rename(paths.mutationLockDir, abandoned);
        const moved = mutationOwnerSchema.safeParse(
          await fileJson(join(abandoned, "owner.json")),
        );
        if (moved.success && moved.data.token === owner.data.token) {
          await rm(abandoned, { recursive: true, force: true });
          continue;
        }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      }
    }
    await Bun.sleep(10);
  }
  throw new RootOwnershipError(
    `Timed out serializing LocalBase operation ownership for ${paths.operationLockDir}.`,
  );
}

async function withOperationMutation<T>(
  paths: OperationPaths,
  work: () => Promise<T>,
): Promise<T> {
  const release = await acquireOperationMutation(paths);
  try {
    return await work();
  } finally {
    await release();
  }
}

async function removeAbandonedOperationLeaseLocked(
  paths: OperationPaths,
  operationId: string,
): Promise<boolean> {
  const raw = await fileJson(paths.operationOwnerPath);
  const current = operationOwnerSchema.safeParse(raw);
  if (!current.success || current.data.operationId !== operationId) {
    return false;
  }
  if (current.data.handoffExpiresAt) {
    if (Date.parse(current.data.handoffExpiresAt) > Date.now()) return false;
  } else if (processPresence(current.data.pid) !== "absent") {
    return false;
  }
  const abandoned = `${paths.operationLockDir}.${operationId}.abandoned`;
  try {
    await rename(paths.operationLockDir, abandoned);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
  const moved = operationOwnerSchema.safeParse(
    await fileJson(join(abandoned, "owner.json")),
  );
  if (!moved.success || moved.data.operationId !== operationId) {
    throw new RootOwnershipError(
      `Operation ownership changed while reclaiming ${paths.operationLockDir}.`,
    );
  }
  await rm(abandoned, { recursive: true, force: true });
  return true;
}

async function acquireOperationLease(
  root: string,
  operation: string,
  coordinationDirectory?: string,
): Promise<OperationLease> {
  const canonical = canonicalRootSchema.parse(root);
  const paths = operationPaths(canonical, coordinationDirectory);
  await ensurePrivateDirectory(paths.coordinationDir);
  const deadline = Date.now() + 10_000;
  let invalidOwnerSince: number | undefined;

  while (Date.now() < deadline) {
    try {
      await mkdir(paths.operationLockDir, { mode: 0o700 });
      const owner = operationOwnerSchema.parse({
        version: 1,
        operationId: crypto.randomUUID(),
        operation,
        root: canonical,
        rootHash: canonicalRootHash(canonical),
        pid: process.pid,
        startedAt: new Date().toISOString(),
      });
      await writeJsonAtomically(paths.operationOwnerPath, owner);
      let handedOff = false;
      return {
        operationId: owner.operationId,
        handoff: async (serviceToken) => {
          const token = z.uuid().parse(serviceToken);
          await withOperationMutation(paths, async () => {
            const current = operationOwnerSchema.safeParse(
              await fileJson(paths.operationOwnerPath),
            );
            if (
              !current.success ||
              current.data.operationId !== owner.operationId ||
              current.data.handoffToken !== undefined
            ) {
              throw new RootOwnershipError(
                `Cannot hand off LocalBase operation ownership for ${canonical}.`,
              );
            }
            await writeJsonAtomically(
              paths.operationOwnerPath,
              operationOwnerSchema.parse({
                ...current.data,
                operation: "service-start-handoff",
                handoffToken: token,
                handoffExpiresAt: new Date(
                  Date.now() + SERVICE_START_HANDOFF_MS,
                ).toISOString(),
              }),
            );
            handedOff = true;
          });
        },
        release: async () => {
          if (handedOff) return;
          await withOperationMutation(paths, async () => {
            const current = operationOwnerSchema.safeParse(
              await fileJson(paths.operationOwnerPath),
            );
            if (
              current.success &&
              current.data.operationId === owner.operationId
            ) {
              await rm(paths.operationLockDir, {
                recursive: true,
                force: true,
              });
            }
          });
        },
      };
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      const reclaimed = await withOperationMutation(paths, async () => {
        const owner = operationOwnerSchema.safeParse(
          await fileJson(paths.operationOwnerPath),
        );
        return (
          owner.success &&
          owner.data.root === canonical &&
          owner.data.rootHash === canonicalRootHash(canonical) &&
          (await removeAbandonedOperationLeaseLocked(
            paths,
            owner.data.operationId,
          ))
        );
      });
      if (reclaimed) {
        continue;
      }
      const owner = operationOwnerSchema.safeParse(
        await fileJson(paths.operationOwnerPath),
      );
      if (
        !owner.success &&
        !(await Bun.file(paths.operationOwnerPath).exists())
      ) {
        invalidOwnerSince = undefined;
        await Bun.sleep(25);
        continue;
      }
      if (!owner.success) {
        invalidOwnerSince ??= Date.now();
        if (Date.now() - invalidOwnerSince >= 250) {
          throw new RootOwnershipError(
            `Cannot coordinate LocalBase operations for ${canonical}: operation metadata is invalid.`,
          );
        }
        await Bun.sleep(25);
        continue;
      }
      invalidOwnerSince = undefined;
      await Bun.sleep(50);
    }
  }
  throw new RootOwnershipError(
    `Timed out waiting to ${operation} LocalBase root ${canonical}; another LocalBase operation is still active or its owner cannot be verified.`,
  );
}

export async function withRootOperation<T>(
  root: string,
  operation: string,
  work: (canonicalRoot: string) => Promise<T>,
  options: { coordinationDirectory?: string } = {},
): Promise<T> {
  const canonical = await canonicalRoot(root);
  const lease = await acquireOperationLease(
    canonical,
    operation,
    options.coordinationDirectory,
  );
  try {
    return await work(canonical);
  } finally {
    await lease.release();
  }
}

export async function withServiceStartHandoff<T>(
  root: string,
  work: (
    canonicalRoot: string,
    handoff: (serviceToken: string) => Promise<void>,
  ) => Promise<T>,
  options: { coordinationDirectory?: string } = {},
): Promise<T> {
  const canonical = await canonicalRoot(root);
  const lease = await acquireOperationLease(
    canonical,
    "service-start",
    options.coordinationDirectory,
  );
  try {
    return await work(canonical, lease.handoff);
  } finally {
    await lease.release();
  }
}

async function claimServiceStartHandoff(
  root: string,
  serviceToken: string,
  coordinationDirectory?: string,
): Promise<"missing" | "waiting" | OperationLease> {
  const paths = operationPaths(root, coordinationDirectory);
  if (!(await directoryExists(paths.operationLockDir))) return "missing";
  return await withOperationMutation(paths, async () => {
    if (!(await directoryExists(paths.operationLockDir))) return "missing";
    const owner = operationOwnerSchema.safeParse(
      await fileJson(paths.operationOwnerPath),
    );
    if (!owner.success) return "waiting";
    if (
      owner.data.root !== root ||
      owner.data.rootHash !== canonicalRootHash(root) ||
      !owner.data.handoffToken ||
      !owner.data.handoffExpiresAt
    ) {
      return "waiting";
    }
    if (Date.parse(owner.data.handoffExpiresAt) <= Date.now()) {
      await removeAbandonedOperationLeaseLocked(paths, owner.data.operationId);
      return "missing";
    }
    if (owner.data.handoffToken !== serviceToken) return "waiting";

    const claimed = operationOwnerSchema.parse({
      version: 1,
      operationId: crypto.randomUUID(),
      operation: "serve-initialize",
      root,
      rootHash: canonicalRootHash(root),
      pid: process.pid,
      startedAt: new Date().toISOString(),
    });
    await writeJsonAtomically(paths.operationOwnerPath, claimed);
    return {
      operationId: claimed.operationId,
      handoff: async () => {
        throw new RootOwnershipError(
          `Cannot hand off claimed LocalBase operation ownership for ${root}.`,
        );
      },
      release: async () => {
        await withOperationMutation(paths, async () => {
          const current = operationOwnerSchema.safeParse(
            await fileJson(paths.operationOwnerPath),
          );
          if (
            current.success &&
            current.data.operationId === claimed.operationId
          ) {
            await rm(paths.operationLockDir, {
              recursive: true,
              force: true,
            });
          }
        });
      },
    };
  });
}

export async function acquireServeInitializationLease(
  root: string,
  serviceToken?: string,
  options: { coordinationDirectory?: string } = {},
): Promise<OperationLease> {
  const canonical = await canonicalRoot(root);
  if (!serviceToken) {
    return await acquireOperationLease(
      canonical,
      "serve-initialize",
      options.coordinationDirectory,
    );
  }
  const token = z.uuid().parse(serviceToken);
  const deadline = Date.now() + SERVICE_START_HANDOFF_MS;
  while (Date.now() < deadline) {
    const claimed = await claimServiceStartHandoff(
      canonical,
      token,
      options.coordinationDirectory,
    );
    if (claimed !== "missing" && claimed !== "waiting") return claimed;
    if (claimed === "missing") {
      return await acquireOperationLease(
        canonical,
        "serve-initialize",
        options.coordinationDirectory,
      );
    }
    await Bun.sleep(25);
  }
  throw new RootOwnershipError(
    `Timed out waiting for LocalBase service start handoff for ${canonical}.`,
  );
}

export async function acquireGatewayLeaseForServe(
  root: string,
  endpoint: {
    host: string;
    port: number;
    serviceId?: string;
    serviceToken?: string;
  },
  options: { coordinationDirectory?: string } = {},
): Promise<GatewayLease> {
  const canonical = await canonicalRoot(root);
  const initialization = await acquireServeInitializationLease(
    canonical,
    endpoint.serviceToken,
    options,
  );
  try {
    const lease = await acquireGatewayLease(canonical, endpoint);
    await initialization.release();
    return lease;
  } catch (error) {
    await initialization.release();
    throw error;
  }
}

export async function waitForGatewayStoppedAtRoot(
  root: string,
  timeoutMs = 10_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const state = await getGatewayInstanceStateAtRoot(root);
    if (state.state === "missing") return;
    if (
      state.state === "stale" &&
      (await removeStaleGatewayLease(root, state.instance))
    ) {
      return;
    }
    if (state.state === "invalid") {
      throw new RootOwnershipError(
        `Gateway ownership became invalid while stopping ${root}: ${state.detail}.`,
      );
    }
    await Bun.sleep(50);
  }
  throw new RootOwnershipError(
    `LocalBase gateway for ${root} did not release ownership within ${timeoutMs}ms. No process was signaled because its identity could not be proven.`,
  );
}
