import { chmod, mkdir, rename, rm } from "node:fs/promises";
import { basename, dirname } from "node:path";
import { z } from "zod";
import {
  createServiceDefinition,
  parseLaunchdDefinition,
  parseSystemdDefinition,
  resolveServiceInvocation,
  serviceDefinitionDirectory,
  serviceInstanceTokenSchema,
  serviceLogDirectory,
  serviceManifestPath,
  serviceManifestSchema,
  serviceMetadata,
  type ServiceDefinition,
  type ServiceManifest,
  type ServiceMetadata,
} from "./definitions";
import { ensureLocalBaseRootMarker } from "../../utils/root";
import {
  canonicalRootHash,
  clearStaleGatewayLeaseAtRoot,
  gatewayHealthUrl,
  getGatewayInstanceStateAtRoot,
  waitForGatewayStoppedAtRoot,
  withServiceStartHandoff,
  withRootOperation,
  type GatewayInstanceState,
} from "./ownership";

const MANAGER_TIMEOUT_MS = 10_000;
const MAX_MANAGER_OUTPUT_BYTES = 64 * 1024;

export const managerCommandResultSchema = z
  .object({
    exitCode: z.number().int().min(-255).max(255),
    stdout: z.string(),
    stderr: z.string(),
  })
  .strict();

export type ManagerCommandResult = z.infer<typeof managerCommandResultSchema>;
export type ServiceManagerCommandRunner = (
  args: readonly string[],
) => Promise<ManagerCommandResult>;

export const serviceStateSchema = z.enum([
  "foreground",
  "running",
  "starting",
  "stopping",
  "stopped",
  "failed",
  "not_installed",
  "unknown",
]);

export const serviceStatusSchema = z
  .object({
    manager: z.enum(["launchd", "systemd-user"]),
    serviceId: z.string().min(1),
    root: z.string().min(1),
    definitionPath: z.string().min(1),
    definitionInstalled: z.boolean(),
    state: serviceStateSchema,
    managerState: z.string().min(1),
    pid: z.number().int().positive().optional(),
    baseUrl: z.string().url().optional(),
  })
  .strict();

export const gatewayReadinessSchema = z.discriminatedUnion("state", [
  z.object({ state: z.literal("ready"), url: z.string().url() }).strict(),
  z
    .object({
      state: z.literal("not_ready"),
      url: z.string().url().optional(),
      detail: z.string().min(1),
    })
    .strict(),
]);

export const serviceInspectionSchema = z
  .object({
    service: serviceStatusSchema,
    gateway: gatewayReadinessSchema,
  })
  .strict();

export type ServiceStatus = z.infer<typeof serviceStatusSchema>;
export type ServiceInspection = z.infer<typeof serviceInspectionSchema>;

const systemdStatusSchema = z
  .object({
    LoadState: z.enum(["loaded", "not-found", "masked", "error"]),
    ActiveState: z.enum([
      "active",
      "inactive",
      "activating",
      "deactivating",
      "reloading",
      "failed",
    ]),
    SubState: z.string().min(1),
    MainPID: z.string().regex(/^\d+$/).transform(Number),
    ExecMainCode: z.string().regex(/^\d+$/).transform(Number),
    ExecMainStatus: z.string().regex(/^\d+$/).transform(Number),
  })
  .strict();

const launchdStatusSchema = z
  .object({
    state: z.string().min(1),
    pid: z.coerce.number().int().positive().optional(),
    lastExitCode: z.coerce.number().int().optional(),
  })
  .strict();

type ManagerObservation =
  | {
      manager: "launchd";
      loaded: boolean;
      active: boolean;
      failed: boolean;
      state: string;
      pid?: number;
    }
  | {
      manager: "systemd-user";
      loaded: boolean;
      active: boolean;
      stopping: boolean;
      failed: boolean;
      state: string;
      pid?: number;
    };

export class ServiceManagerError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "ServiceManagerError";
  }
}

export function parseLaunchctlStatus(output: string): {
  state: string;
  pid?: number;
  lastExitCode?: number;
} {
  let depth = 0;
  const fields = new Map<string, string>();
  for (const line of output.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (depth === 1) {
      const match = /^([^=]+?)\s*=\s*(.*?)\s*$/.exec(trimmed);
      if (match && !match[2]?.endsWith("{")) {
        const key = match[1]?.trim();
        if (key === "state" || key === "pid" || key === "last exit code") {
          if (fields.has(key)) {
            throw new ServiceManagerError(
              `launchctl returned duplicate top-level ${key}.`,
            );
          }
          fields.set(key, match[2] ?? "");
        }
      }
    }
    depth += (line.match(/{/g) ?? []).length;
    depth -= (line.match(/}/g) ?? []).length;
    if (depth < 0) {
      throw new ServiceManagerError(
        "launchctl returned malformed service state.",
      );
    }
  }
  if (depth !== 0) {
    throw new ServiceManagerError(
      "launchctl returned malformed service state.",
    );
  }
  const parsed = launchdStatusSchema.safeParse({
    state: fields.get("state"),
    ...(fields.has("pid") ? { pid: fields.get("pid") } : {}),
    ...(fields.has("last exit code") &&
    fields.get("last exit code") !== "(never exited)"
      ? { lastExitCode: fields.get("last exit code") }
      : {}),
  });
  if (!parsed.success) {
    throw new ServiceManagerError(
      `launchctl returned invalid service state: ${parsed.error.issues[0]?.message ?? "malformed output"}.`,
    );
  }
  return parsed.data;
}

async function boundedText(
  stream: ReadableStream<Uint8Array>,
  maximumBytes: number,
): Promise<string> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let remaining = maximumBytes;
  let output = "";
  let truncated = false;
  while (true) {
    const result = await reader.read();
    if (result.done) break;
    if (remaining > 0) {
      const available = remaining;
      const kept = result.value.subarray(0, available);
      output += decoder.decode(kept, { stream: true });
      remaining -= kept.byteLength;
      if (result.value.byteLength > available) truncated = true;
    } else {
      truncated = true;
    }
  }
  output += decoder.decode();
  return truncated ? `${output}\n[output truncated]` : output;
}

async function productionCommandRunner(
  args: readonly string[],
): Promise<ManagerCommandResult> {
  let child: Bun.Subprocess;
  try {
    child = Bun.spawn([...args], {
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    });
  } catch (error) {
    throw new ServiceManagerError(
      `Could not start ${args[0] ?? "the service manager"}. Use foreground "local-base serve" if the user service manager is unavailable.`,
      { cause: error },
    );
  }
  if (
    !(child.stdout instanceof ReadableStream) ||
    !(child.stderr instanceof ReadableStream)
  ) {
    child.kill("SIGTERM");
    throw new ServiceManagerError(
      `Could not capture ${args[0] ?? "service manager"} output.`,
    );
  }

  const stdout = boundedText(child.stdout, MAX_MANAGER_OUTPUT_BYTES);
  const stderr = boundedText(child.stderr, MAX_MANAGER_OUTPUT_BYTES);
  const outcome = await Promise.race([
    child.exited.then((exitCode) => ({ kind: "exit" as const, exitCode })),
    Bun.sleep(MANAGER_TIMEOUT_MS).then(() => ({ kind: "timeout" as const })),
  ]);
  if (outcome.kind === "timeout") {
    child.kill("SIGTERM");
    await Promise.race([child.exited, Bun.sleep(250)]);
    if (child.exitCode === null) child.kill("SIGKILL");
    await child.exited;
    await Promise.all([stdout, stderr]);
    throw new ServiceManagerError(
      `${args[0] ?? "Service manager"} timed out after ${MANAGER_TIMEOUT_MS}ms. Use foreground "local-base serve" until the user service manager is available.`,
    );
  }
  return managerCommandResultSchema.parse({
    exitCode: outcome.exitCode,
    stdout: await stdout,
    stderr: await stderr,
  });
}

let commandRunner: ServiceManagerCommandRunner = productionCommandRunner;
let commandTimeoutMs = MANAGER_TIMEOUT_MS;

export function setServiceManagerCommandRunnerForTests(
  runner?: ServiceManagerCommandRunner,
  timeoutMs = MANAGER_TIMEOUT_MS,
): void {
  commandRunner = runner ?? productionCommandRunner;
  commandTimeoutMs = runner ? timeoutMs : MANAGER_TIMEOUT_MS;
}

function managerExecutable(metadata: ServiceMetadata): string {
  return metadata.manager === "launchd"
    ? "/bin/launchctl"
    : "/usr/bin/systemctl";
}

async function runManagerCommand(
  metadata: ServiceMetadata,
  args: string[],
): Promise<ManagerCommandResult> {
  const command = [managerExecutable(metadata), ...args];
  const outcome = await Promise.race([
    commandRunner(command).then((result) => ({
      kind: "result" as const,
      result,
    })),
    Bun.sleep(commandTimeoutMs).then(() => ({ kind: "timeout" as const })),
  ]);
  if (outcome.kind === "timeout") {
    throw new ServiceManagerError(
      `${basename(command[0])} timed out after ${commandTimeoutMs}ms. Use foreground "local-base serve" until the user service manager is available.`,
    );
  }
  return managerCommandResultSchema.parse(outcome.result);
}

function commandFailure(
  args: readonly string[],
  result: ManagerCommandResult,
): ServiceManagerError {
  const detail =
    result.stderr.trim() ||
    result.stdout.trim() ||
    `exit code ${result.exitCode}`;
  return new ServiceManagerError(
    `${basename(args[0] ?? "service manager")} could not manage the LocalBase service: ${detail}.`,
  );
}

async function assertManagerCommand(
  metadata: ServiceMetadata,
  args: string[],
): Promise<void> {
  const fullArgs = [managerExecutable(metadata), ...args];
  const result = await runManagerCommand(metadata, args);
  if (result.exitCode !== 0) throw commandFailure(fullArgs, result);
}

function currentUserId(): number {
  const uid =
    typeof process.getuid === "function" ? process.getuid() : undefined;
  const parsed = z.number().int().nonnegative().safeParse(uid);
  if (!parsed.success) {
    throw new ServiceManagerError(
      "launchd user services require a POSIX user ID. Run LocalBase from a signed-in macOS user session.",
    );
  }
  return parsed.data;
}

function launchdDomain(): string {
  return `gui/${currentUserId()}`;
}

function launchdTarget(metadata: ServiceMetadata): string {
  return `${launchdDomain()}/${metadata.serviceId}`;
}

function parseSystemdStatus(output: string) {
  const record: Record<string, string> = {};
  for (const line of output.split(/\r?\n/)) {
    if (!line) continue;
    const separator = line.indexOf("=");
    if (separator <= 0) {
      throw new ServiceManagerError(
        "systemctl returned malformed service state.",
      );
    }
    const key = line.slice(0, separator);
    if (key in record) {
      throw new ServiceManagerError(
        `systemctl returned duplicate ${key} service state.`,
      );
    }
    record[key] = line.slice(separator + 1);
  }
  const parsed = systemdStatusSchema.safeParse(record);
  if (!parsed.success) {
    throw new ServiceManagerError(
      `systemctl returned invalid service state: ${parsed.error.issues[0]?.message ?? "malformed output"}.`,
    );
  }
  return parsed.data;
}

async function inspectManager(
  metadata: ServiceMetadata,
): Promise<ManagerObservation> {
  if (metadata.manager === "launchd") {
    const domain = await runManagerCommand(metadata, [
      "print",
      launchdDomain(),
    ]);
    if (domain.exitCode !== 0) {
      throw new ServiceManagerError(
        'The launchd GUI user domain is unavailable. Sign in to a macOS user session or use foreground "local-base serve".',
      );
    }
    const result = await runManagerCommand(metadata, [
      "print",
      launchdTarget(metadata),
    ]);
    if (result.exitCode !== 0) {
      return {
        manager: "launchd",
        loaded: false,
        active: false,
        failed: false,
        state: "unloaded",
      };
    }
    const { state, pid, lastExitCode } = parseLaunchctlStatus(result.stdout);
    const active =
      (state === "running" && pid !== undefined && pid > 0) ||
      state === "spawn scheduled";
    return {
      manager: "launchd",
      loaded: true,
      active,
      failed: !active && lastExitCode !== undefined && lastExitCode !== 0,
      state,
      ...(pid && pid > 0 ? { pid } : {}),
    };
  }

  const result = await runManagerCommand(metadata, [
    "--user",
    "show",
    metadata.unitName,
    "--property=LoadState,ActiveState,SubState,MainPID,ExecMainCode,ExecMainStatus",
    "--no-pager",
  ]);
  if (result.exitCode !== 0) {
    throw new ServiceManagerError(
      'The systemd user manager is unavailable. Ensure the user bus is running or use foreground "local-base serve".',
    );
  }
  const status = parseSystemdStatus(result.stdout);
  const active = ["active", "activating", "reloading"].includes(
    status.ActiveState,
  );
  return {
    manager: "systemd-user",
    loaded: status.LoadState === "loaded",
    active,
    stopping: status.ActiveState === "deactivating",
    failed: status.ActiveState === "failed" || status.LoadState === "error",
    state: `${status.LoadState}/${status.ActiveState}/${status.SubState}`,
    ...(status.MainPID > 0 ? { pid: status.MainPID } : {}),
  };
}

function managerIsActive(observation: ManagerObservation): boolean {
  return observation.active;
}

function managerIsStopping(observation: ManagerObservation): boolean {
  return observation.manager === "systemd-user" && observation.stopping;
}

function managerHasFailed(observation: ManagerObservation): boolean {
  return observation.failed;
}

async function readManifest(
  root: string,
): Promise<
  | { state: "missing" }
  | { state: "valid"; manifest: ServiceManifest }
  | { state: "invalid" }
> {
  const file = Bun.file(serviceManifestPath(root));
  if (!(await file.exists())) return { state: "missing" };
  try {
    const parsed = serviceManifestSchema.safeParse(await file.json());
    return parsed.success
      ? { state: "valid", manifest: parsed.data }
      : { state: "invalid" };
  } catch {
    return { state: "invalid" };
  }
}

async function definitionFingerprint(
  path: string,
): Promise<string | undefined> {
  const file = Bun.file(path);
  if (!(await file.exists())) return undefined;
  return new Bun.CryptoHasher("sha256")
    .update(await file.arrayBuffer())
    .digest("hex");
}

async function atomicWrite(
  path: string,
  contents: string,
  mode = 0o600,
): Promise<void> {
  const directory = dirname(path);
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  try {
    await Bun.write(temporary, contents);
    await chmod(temporary, mode);
    await rename(temporary, path);
  } finally {
    await rm(temporary, { force: true });
  }
}

async function writeDefinition(
  definition: ServiceDefinition,
  root: string,
): Promise<void> {
  if (definition.manager === "launchd") {
    parseLaunchdDefinition(definition.contents);
  } else {
    parseSystemdDefinition(definition.contents);
  }
  await mkdir(serviceLogDirectory(root), { recursive: true, mode: 0o700 });
  await chmod(serviceLogDirectory(root), 0o700);
  await mkdir(serviceDefinitionDirectory(definition), {
    recursive: true,
    mode: 0o700,
  });
  await atomicWrite(definition.definitionPath, definition.contents);
  await atomicWrite(
    serviceManifestPath(root),
    JSON.stringify(definition.manifest),
  );
}

function readinessFromOwner(
  owner: GatewayInstanceState,
): z.infer<typeof gatewayReadinessSchema> {
  if (owner.state === "active") {
    return { state: "ready", url: gatewayHealthUrl(owner.instance) };
  }
  if (owner.state === "unknown" && owner.instance !== undefined) {
    return {
      state: "not_ready",
      url: gatewayHealthUrl(owner.instance),
      detail: owner.detail,
    };
  }
  if (owner.state === "stale") {
    return {
      state: "not_ready",
      url: gatewayHealthUrl(owner.instance),
      detail: "gateway owner process is no longer running",
    };
  }
  return {
    state: "not_ready",
    detail:
      owner.state === "missing" ? "no active LocalBase instance" : owner.detail,
  };
}

function ownerServiceId(owner: GatewayInstanceState): string | undefined {
  return "instance" in owner ? owner.instance?.serviceId : undefined;
}

function statusFromObservations(
  root: string,
  metadata: ServiceMetadata,
  definitionInstalled: boolean,
  installedFingerprint: string | undefined,
  manifestState: Awaited<ReturnType<typeof readManifest>>,
  manager: ManagerObservation,
  owner: GatewayInstanceState,
  acceptedStart = false,
): ServiceStatus {
  const managerState = manager.state;
  const activeInstance = owner.state === "active" ? owner.instance : undefined;
  const recordedInstance =
    owner.state === "active" ||
    owner.state === "stale" ||
    (owner.state === "unknown" && owner.instance)
      ? owner.instance
      : undefined;
  const tokenMatches =
    recordedInstance !== undefined &&
    manifestState.state === "valid" &&
    recordedInstance.serviceId === metadata.serviceId &&
    recordedInstance.serviceToken === manifestState.manifest.serviceToken;
  const systemdPidMatches =
    manager.manager !== "systemd-user" ||
    (recordedInstance !== undefined && manager.pid === recordedInstance.pid);
  const ownedByService = tokenMatches && systemdPidMatches;
  const foregroundOwner =
    recordedInstance !== undefined && recordedInstance.serviceId === undefined;
  const managedIdentityMismatch =
    recordedInstance?.serviceId === metadata.serviceId && !ownedByService;
  const manifestIssue =
    manifestState.state === "invalid" ||
    (manifestState.state === "valid" &&
      (manifestState.manifest.root !== root ||
        manifestState.manifest.rootHash !== canonicalRootHash(root) ||
        manifestState.manifest.manager !== metadata.manager ||
        manifestState.manifest.serviceId !== metadata.serviceId ||
        manifestState.manifest.definitionPath !== metadata.definitionPath ||
        (definitionInstalled &&
          installedFingerprint !==
            manifestState.manifest.definitionFingerprint))) ||
    (manifestState.state === "missing" &&
      (definitionInstalled || manager.loaded || ownedByService));

  let state: z.infer<typeof serviceStateSchema>;
  if (owner.state === "invalid") {
    state = "unknown";
  } else if (managedIdentityMismatch) {
    state = "unknown";
  } else if (foregroundOwner) {
    state = owner.state === "active" ? "foreground" : "unknown";
  } else if (recordedInstance && !ownedByService) {
    state = "unknown";
  } else if (manifestIssue) {
    state = "unknown";
  } else if (managerHasFailed(manager)) {
    state = "failed";
  } else if (owner.state === "stale") {
    state = "failed";
  } else if (managerIsStopping(manager)) {
    state = "stopping";
  } else if (activeInstance && ownedByService && managerIsActive(manager)) {
    state = "running";
  } else if (
    managerIsActive(manager) &&
    (owner.state === "missing" ||
      (owner.state === "unknown" &&
        (ownedByService || owner.instance === undefined)))
  ) {
    state = "starting";
  } else if (activeInstance && ownedByService && !managerIsActive(manager)) {
    state = "unknown";
  } else if (
    acceptedStart &&
    manager.manager === "launchd" &&
    manager.loaded &&
    (manager.state === "waiting" || manager.state === "spawn scheduled") &&
    (owner.state === "missing" ||
      (owner.state === "unknown" && owner.instance === undefined))
  ) {
    state = "starting";
  } else if (
    !definitionInstalled &&
    manifestState.state === "missing" &&
    !manager.loaded &&
    owner.state === "missing"
  ) {
    state = "not_installed";
  } else {
    state = "stopped";
  }

  return serviceStatusSchema.parse({
    manager: metadata.manager,
    serviceId: metadata.serviceId,
    root,
    definitionPath: metadata.definitionPath,
    definitionInstalled,
    state,
    managerState:
      owner.state === "invalid"
        ? `${managerState}/owner-invalid`
        : managedIdentityMismatch
          ? `${managerState}/managed-identity-mismatch`
          : owner.state === "unknown"
            ? `${managerState}/owner-unverified`
            : manifestIssue
              ? `${managerState}/manifest-mismatch`
              : managerState,
    ...(activeInstance ? { pid: activeInstance.pid } : {}),
    ...(recordedInstance
      ? {
          baseUrl: gatewayHealthUrl(recordedInstance).replace(/\/health$/, ""),
        }
      : {}),
  });
}

async function inspectServiceAtRoot(
  root: string,
  acceptedStart = false,
): Promise<ServiceInspection> {
  const metadata = await serviceMetadata(root);
  const definitionInstalled = await Bun.file(metadata.definitionPath).exists();
  const [manager, owner, manifest, installedFingerprint] = await Promise.all([
    inspectManager(metadata),
    getGatewayInstanceStateAtRoot(root),
    readManifest(root),
    definitionInstalled
      ? definitionFingerprint(metadata.definitionPath)
      : Promise.resolve(undefined),
  ]);
  return serviceInspectionSchema.parse({
    service: statusFromObservations(
      root,
      metadata,
      definitionInstalled,
      installedFingerprint,
      manifest,
      manager,
      owner,
      acceptedStart,
    ),
    gateway: readinessFromOwner(owner),
  });
}

export async function getServiceInspection(
  root: string,
): Promise<ServiceInspection> {
  return await withRootOperation(root, "status", inspectServiceAtRoot);
}

export async function getServiceStatus(root: string): Promise<ServiceStatus> {
  return (await getServiceInspection(root)).service;
}

function assertControllableOwner(
  owner: GatewayInstanceState,
  metadata: ServiceMetadata,
  serviceToken: string | undefined,
  operation: string,
): void {
  if (owner.state === "invalid") {
    throw new ServiceManagerError(
      `Cannot ${operation} LocalBase for ${owner.root}: ${owner.detail}.`,
    );
  }
  if (owner.state === "unknown" && !owner.instance) {
    throw new ServiceManagerError(
      `Cannot ${operation} LocalBase for ${owner.root}: ${owner.detail}.`,
    );
  }
  const instance = "instance" in owner ? owner.instance : undefined;
  const serviceId = instance?.serviceId;
  if (
    (owner.state === "active" || owner.state === "unknown") &&
    serviceId === metadata.serviceId &&
    instance?.serviceToken !== serviceToken
  ) {
    throw new ServiceManagerError(
      `Cannot ${operation} LocalBase for ${owner.root}: the managed gateway identity does not match this service definition.`,
    );
  }
  if (
    (owner.state === "active" || owner.state === "unknown") &&
    serviceId !== metadata.serviceId
  ) {
    const ownership =
      serviceId === undefined
        ? "a foreground gateway"
        : `another managed gateway (${serviceId})`;
    throw new ServiceManagerError(
      `Cannot ${operation} LocalBase for ${owner.root}: ${ownership} owns the root. Stop it explicitly before managing the user service.`,
    );
  }
}

async function enableManager(metadata: ServiceMetadata): Promise<void> {
  if (metadata.manager === "launchd") {
    await assertManagerCommand(metadata, ["enable", launchdTarget(metadata)]);
  } else {
    await assertManagerCommand(metadata, [
      "--user",
      "enable",
      metadata.unitName,
    ]);
  }
}

async function disableManager(metadata: ServiceMetadata): Promise<void> {
  if (metadata.manager === "launchd") {
    await assertManagerCommand(metadata, ["disable", launchdTarget(metadata)]);
  } else {
    const args = ["--user", "disable", metadata.unitName];
    const result = await runManagerCommand(metadata, args);
    if (result.exitCode === 0) return;
    const detail = `${result.stdout}\n${result.stderr}`.toLowerCase();
    if (detail.includes("does not exist") || detail.includes("not found")) {
      return;
    }
    throw commandFailure([managerExecutable(metadata), ...args], result);
  }
}

async function startManager(definition: ServiceDefinition): Promise<void> {
  if (definition.manager === "launchd") {
    await enableManager(definition);
    await assertManagerCommand(definition, [
      "bootstrap",
      launchdDomain(),
      definition.definitionPath,
    ]);
  } else {
    await assertManagerCommand(definition, ["--user", "daemon-reload"]);
    await enableManager(definition);
    await assertManagerCommand(definition, [
      "--user",
      "start",
      definition.unitName,
    ]);
  }
}

async function stopManager(
  metadata: ServiceMetadata,
  disable: boolean,
): Promise<void> {
  const before = await inspectManager(metadata);
  if (metadata.manager === "launchd") {
    if (before.loaded) {
      await assertManagerCommand(metadata, [
        "bootout",
        launchdTarget(metadata),
      ]);
    }
  } else if (before.loaded || managerIsActive(before)) {
    await assertManagerCommand(metadata, ["--user", "stop", metadata.unitName]);
  }
  if (disable) await disableManager(metadata);

  const deadline = Date.now() + MANAGER_TIMEOUT_MS;
  while (Date.now() < deadline) {
    const current = await inspectManager(metadata);
    if (!managerIsActive(current) && !managerIsStopping(current)) return;
    await Bun.sleep(50);
  }
  throw new ServiceManagerError(
    `${metadata.manager} did not stop ${metadata.serviceId} within ${MANAGER_TIMEOUT_MS}ms.`,
  );
}

async function stopManagedInstanceAtRoot(
  root: string,
  metadata: ServiceMetadata,
  disable: boolean,
): Promise<void> {
  const manifest = await readManifest(root);
  const serviceToken =
    manifest.state === "valid" &&
    manifest.manifest.root === root &&
    manifest.manifest.serviceId === metadata.serviceId
      ? manifest.manifest.serviceToken
      : undefined;
  let owner = await getGatewayInstanceStateAtRoot(root);
  assertControllableOwner(owner, metadata, serviceToken, "stop");
  if (owner.state === "stale") {
    await clearStaleGatewayLeaseAtRoot(root);
    owner = await getGatewayInstanceStateAtRoot(root);
  }

  const manager = await inspectManager(metadata);
  if (
    (owner.state === "active" || owner.state === "unknown") &&
    ownerServiceId(owner) === metadata.serviceId &&
    !managerIsActive(manager) &&
    !managerIsStopping(manager)
  ) {
    throw new ServiceManagerError(
      `Cannot stop LocalBase for ${root}: the recorded managed gateway is not controlled by the current ${metadata.manager} instance. No process was signaled.`,
    );
  }

  await stopManager(metadata, disable);
  await waitForGatewayStoppedAtRoot(root);
}

async function definitionMatches(
  root: string,
  desired: ServiceDefinition,
): Promise<boolean> {
  const manifest = await readManifest(root);
  if (
    manifest.state !== "valid" ||
    manifest.manifest.definitionFingerprint !== desired.fingerprint ||
    manifest.manifest.serviceId !== desired.serviceId ||
    manifest.manifest.root !== root
  ) {
    return false;
  }
  return (
    (await definitionFingerprint(desired.definitionPath)) ===
    desired.fingerprint
  );
}

async function startServiceAtRoot(
  root: string,
  restart: boolean,
  handoff: (serviceToken: string) => Promise<void>,
): Promise<ServiceInspection> {
  ensureLocalBaseRootMarker(root);
  const previousManifest = await readManifest(root);
  const currentServiceToken =
    previousManifest.state === "valid" &&
    previousManifest.manifest.root === root
      ? previousManifest.manifest.serviceToken
      : undefined;
  const serviceToken = serviceInstanceTokenSchema.parse(
    !restart && currentServiceToken ? currentServiceToken : crypto.randomUUID(),
  );
  const definition = await createServiceDefinition(
    root,
    await resolveServiceInvocation(root),
    undefined,
    serviceToken,
  );
  let owner = await getGatewayInstanceStateAtRoot(root);
  assertControllableOwner(
    owner,
    definition,
    currentServiceToken ?? definition.serviceToken,
    restart ? "restart" : "start",
  );
  if (owner.state === "stale") {
    await clearStaleGatewayLeaseAtRoot(root);
    owner = await getGatewayInstanceStateAtRoot(root);
  }
  const manager = await inspectManager(definition);
  const unchanged = await definitionMatches(root, definition);
  const managedOwner =
    ownerServiceId(owner) === definition.serviceId &&
    (owner.state === "active" || owner.state === "unknown");

  if (
    !restart &&
    unchanged &&
    managerIsActive(manager) &&
    (managedOwner || owner.state === "missing")
  ) {
    await enableManager(definition);
    return await inspectServiceAtRoot(root);
  }

  if (
    manager.loaded ||
    managerIsActive(manager) ||
    managerIsStopping(manager) ||
    managedOwner
  ) {
    await stopManagedInstanceAtRoot(root, definition, true);
  }
  await writeDefinition(definition, root);
  await startManager(definition);
  await handoff(definition.serviceToken);
  return await inspectServiceAtRoot(root, true);
}

export async function startService(root: string): Promise<ServiceInspection> {
  return await withServiceStartHandoff(
    root,
    async (canonical, handoff) =>
      await startServiceAtRoot(canonical, false, handoff),
  );
}

export async function restartService(root: string): Promise<ServiceInspection> {
  return await withServiceStartHandoff(
    root,
    async (canonical, handoff) =>
      await startServiceAtRoot(canonical, true, handoff),
  );
}

async function stopServiceAtRoot(root: string): Promise<ServiceInspection> {
  const metadata = await serviceMetadata(root);
  await stopManagedInstanceAtRoot(root, metadata, true);
  return await inspectServiceAtRoot(root);
}

export async function stopService(root: string): Promise<ServiceInspection> {
  return await withRootOperation(root, "stop", stopServiceAtRoot);
}

async function removeServiceAtRoot(root: string): Promise<ServiceInspection> {
  const metadata = await serviceMetadata(root);
  await stopManagedInstanceAtRoot(root, metadata, true);
  await rm(metadata.definitionPath, { force: true });
  await rm(serviceManifestPath(root), { force: true });
  if (metadata.manager === "systemd-user") {
    await assertManagerCommand(metadata, ["--user", "daemon-reload"]);
  }
  return await inspectServiceAtRoot(root);
}

export async function removeService(root: string): Promise<ServiceInspection> {
  return await withRootOperation(root, "remove", removeServiceAtRoot);
}

export async function withServiceRootOperation<T>(
  root: string,
  operation: string,
  work: (canonicalRoot: string) => Promise<T>,
): Promise<T> {
  return await withRootOperation(root, operation, work);
}

export async function removeServiceWithinOperation(
  canonicalRoot: string,
): Promise<ServiceInspection> {
  return await removeServiceAtRoot(canonicalRoot);
}

export async function stopServiceWithinOperation(
  canonicalRoot: string,
): Promise<ServiceInspection> {
  return await stopServiceAtRoot(canonicalRoot);
}
