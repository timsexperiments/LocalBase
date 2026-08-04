import { mkdir, rename, rm } from "node:fs/promises";
import { basename, dirname, join } from "node:path";
import { z } from "zod";
import {
  parseLaunchdDefinition,
  parseSystemdDefinition,
} from "../domains/service/definitions";
import {
  acquireGatewayLeaseForServe,
  canonicalRoot,
  gatewayHealthUrl,
  gatewayInstanceSchema,
} from "../domains/service/ownership";
import {
  gatewayHealthSchema,
  gatewayIdentitySchema,
} from "../domains/runtime/health";
import { LOCALBASE_VERSION } from "../version";
import {
  managerCommandResultSchema,
  type ManagerCommandResult,
  type ServiceManagerCommandRunner,
} from "../domains/service/manager";

const fixtureServiceSchema = z
  .object({
    manager: z.enum(["launchd", "systemd-user"]),
    root: z.string().min(1),
    serviceId: z.string().min(1),
    unitName: z.string().min(1),
    definitionPath: z.string().min(1),
    serviceToken: z.uuid(),
    enabled: z.boolean(),
    loaded: z.boolean(),
    activeState: z.enum([
      "inactive",
      "active",
      "failed",
      "scheduled",
      "stopped",
      "waiting",
      "xpcproxy",
    ]),
    pid: z.number().int().positive().optional(),
    lastExitCode: z.number().int().optional(),
  })
  .strict();

const fixtureStateSchema = z
  .object({
    version: z.literal(1),
    services: z.record(z.string(), fixtureServiceSchema),
  })
  .strict();

const callsSchema = z.array(z.array(z.string()));
type FixtureState = z.infer<typeof fixtureStateSchema>;
type FixtureService = z.infer<typeof fixtureServiceSchema>;

function requiredEnvironment(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function readJson<T>(path: string, schema: z.ZodType<T>, fallback: T) {
  const file = Bun.file(path);
  if (!(await file.exists())) return fallback;
  return schema.parse(await file.json());
}

async function writeJson(path: string, value: unknown): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  try {
    await Bun.write(temporary, JSON.stringify(value));
    await rename(temporary, path);
  } finally {
    await rm(temporary, { force: true });
  }
}

function success(stdout = ""): ManagerCommandResult {
  return managerCommandResultSchema.parse({
    exitCode: 0,
    stdout,
    stderr: "",
  });
}

function failure(stderr: string): ManagerCommandResult {
  return managerCommandResultSchema.parse({
    exitCode: 1,
    stdout: "",
    stderr,
  });
}

function reservePort(): number {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const random = new Uint32Array(1);
    crypto.getRandomValues(random);
    const port = 20_000 + (random[0] % 40_000);
    try {
      const reservation = Bun.serve({
        hostname: "127.0.0.1",
        port,
        fetch: () => new Response("reserved"),
      });
      reservation.stop(true);
      return port;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EADDRINUSE") throw error;
    }
  }
  throw new Error("Could not reserve a fixture gateway port.");
}

async function fixtureOwner(service: FixtureService) {
  let value: unknown;
  try {
    value = JSON.parse(
      await Bun.file(
        join(service.root, "runtime", "gateway.lock", "owner.json"),
      ).text(),
    );
  } catch {
    return undefined;
  }
  const parsed = gatewayInstanceSchema.safeParse(value);
  if (
    !parsed.success ||
    parsed.data.serviceId !== service.serviceId ||
    parsed.data.serviceToken !== service.serviceToken
  ) {
    return undefined;
  }
  return parsed.data;
}

async function stopFixtureProcess(service: FixtureService): Promise<void> {
  const owner = await fixtureOwner(service);
  if (!owner) return;
  const response = await fetch(
    `${gatewayHealthUrl(owner).replace(/\/health$/, "")}/__fixture/stop`,
    {
      method: "POST",
      headers: { "x-localbase-service-token": service.serviceToken },
      signal: AbortSignal.timeout(1_000),
    },
  );
  if (!response.ok) {
    throw new Error("Fixture gateway rejected authenticated shutdown.");
  }
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    if (!(await fixtureOwner(service))) return;
    await Bun.sleep(25);
  }
  throw new Error("Fixture gateway did not release ownership after shutdown.");
}

export async function stopFixtureServices(statePath: string): Promise<void> {
  const state = await readJson<FixtureState>(statePath, fixtureStateSchema, {
    version: 1,
    services: {},
  });
  for (const service of Object.values(state.services)) {
    await stopFixtureProcess(service);
  }
}

async function startFixtureProcess(
  service: FixtureService,
  invocation: string[],
): Promise<FixtureService> {
  await stopFixtureProcess(service);
  const child = Bun.spawn(invocation, {
    stdin: "ignore",
    stdout: "ignore",
    stderr: "pipe",
    env: {
      ...process.env,
      LOCALBASE_SERVICE_ID: service.serviceId,
      LOCALBASE_SERVICE_TOKEN: service.serviceToken,
      LOCALBASE_TEST_MANAGED_GATEWAY: "1",
    },
  });
  await Bun.sleep(10);
  if (child.exitCode !== null) {
    throw new Error(
      `Managed fixture gateway exited during startup: ${await new Response(child.stderr).text()}`,
    );
  }
  return {
    ...service,
    loaded: true,
    activeState: "active",
    pid: child.pid,
  };
}

async function launchdService(
  definitionPath: string,
): Promise<{ service: FixtureService; invocation: string[] }> {
  const definition = parseLaunchdDefinition(
    await Bun.file(definitionPath).text(),
  );
  return {
    service: fixtureServiceSchema.parse({
      manager: "launchd",
      root: definition.workingDirectory,
      serviceId: definition.label,
      serviceToken: definition.environment.LOCALBASE_SERVICE_TOKEN,
      unitName: definition.label,
      definitionPath,
      enabled: true,
      loaded: false,
      activeState: "inactive",
    }),
    invocation: definition.programArguments,
  };
}

async function systemdService(
  unitName: string,
): Promise<{ service: FixtureService; invocation: string[] }> {
  const definitionPath = join(
    requiredEnvironment("XDG_CONFIG_HOME"),
    "systemd",
    "user",
    unitName,
  );
  const definition = parseSystemdDefinition(
    await Bun.file(definitionPath).text(),
  );
  const serviceId = definition.environment.LOCALBASE_SERVICE_ID;
  return {
    service: fixtureServiceSchema.parse({
      manager: "systemd-user",
      root: definition.workingDirectory,
      serviceId,
      serviceToken: definition.environment.LOCALBASE_SERVICE_TOKEN,
      unitName,
      definitionPath,
      enabled: false,
      loaded: true,
      activeState: "inactive",
    }),
    invocation: definition.execStart,
  };
}

function targetServiceId(target: string): string {
  return target.split("/").at(-1) ?? "";
}

function systemdOutput(service: FixtureService | undefined): string {
  if (!service) {
    return [
      "LoadState=not-found",
      "ActiveState=inactive",
      "SubState=dead",
      "MainPID=0",
      "ExecMainCode=0",
      "ExecMainStatus=0",
      "NRestarts=0",
      "",
    ].join("\n");
  }
  return [
    `LoadState=${service.loaded ? "loaded" : "not-found"}`,
    `ActiveState=${service.activeState}`,
    `SubState=${service.activeState === "active" ? "running" : "dead"}`,
    `MainPID=${service.pid ?? 0}`,
    "ExecMainCode=0",
    `ExecMainStatus=${service.activeState === "failed" ? 1 : 0}`,
    "NRestarts=0",
    "",
  ].join("\n");
}

export function createServiceManagerFixtureRunner(): ServiceManagerCommandRunner {
  return async (command) => {
    const statePath = requiredEnvironment(
      "LOCALBASE_TEST_SERVICE_MANAGER_STATE",
    );
    const callsPath = requiredEnvironment(
      "LOCALBASE_TEST_SERVICE_MANAGER_CALLS",
    );
    const args = [...command];
    const executable = basename(args.shift() ?? "");
    const actionArgs = executable === "systemctl" ? args.slice(1) : args;
    const action = actionArgs[0] ?? "";

    const calls = await readJson(callsPath, callsSchema, []);
    calls.push([executable, ...args]);
    await writeJson(callsPath, calls);

    if (process.env.LOCALBASE_TEST_SERVICE_MANAGER_UNAVAILABLE === executable) {
      return failure(`${executable} unavailable`);
    }
    if (process.env.LOCALBASE_TEST_SERVICE_MANAGER_FAIL === action) {
      return failure(`fixture failure for ${action}`);
    }
    if (process.env.LOCALBASE_TEST_SERVICE_MANAGER_MALFORMED === action) {
      return success("malformed manager response\n");
    }
    if (process.env.LOCALBASE_TEST_SERVICE_MANAGER_TIMEOUT === action) {
      return await new Promise<ManagerCommandResult>(() => {});
    }

    const state = await readJson<FixtureState>(statePath, fixtureStateSchema, {
      version: 1,
      services: {},
    });

    if (executable === "launchctl") {
      if (action === "print") {
        const target = actionArgs[1] ?? "";
        if (target.split("/").length === 2) return success();
        const service = state.services[targetServiceId(target)];
        if (!service?.loaded) return failure("service not loaded");
        const launchState =
          service.activeState === "active"
            ? "running"
            : service.activeState === "failed"
              ? "exited"
              : service.activeState === "scheduled"
                ? "spawn scheduled"
                : service.activeState === "stopped"
                  ? "stopped"
                  : service.activeState === "xpcproxy"
                    ? "xpcproxy"
                    : "waiting";
        return success(
          [
            `${target} = {`,
            `\tstate = ${launchState}`,
            ...(service.pid ? [`\tpid = ${service.pid}`] : []),
            `\tlast exit code = ${
              service.lastExitCode !== undefined
                ? service.lastExitCode
                : service.activeState === "scheduled"
                  ? "(never exited)"
                  : service.activeState === "failed"
                    ? 1
                    : 0
            }`,
            "\tcoalitions = {",
            "\t\tresource coalition = {",
            "\t\t\tstate = active",
            "\t\t\tpid = 99999",
            "\t\t\tlast exit code = 42",
            "\t\t}",
            "\t}",
            "}",
            "",
          ].join("\n"),
        );
      }
      if (action === "print-disabled") {
        const disabled = Object.values(state.services)
          .filter((service) => service.manager === "launchd")
          .map(
            (service) =>
              `\t"${service.serviceId}" => ${service.enabled ? "enabled" : "disabled"}`,
          );
        return success(["{", ...disabled, "}", ""].join("\n"));
      }
      if (action === "enable" || action === "disable") {
        const serviceId = targetServiceId(actionArgs[1] ?? "");
        const service = state.services[serviceId];
        if (service) {
          state.services[serviceId] = {
            ...service,
            enabled: action === "enable",
          };
          await writeJson(statePath, state);
        }
        return success();
      }
      if (action === "bootstrap") {
        const definitionPath = actionArgs[2];
        if (!definitionPath) return failure("missing definition");
        const parsed = await launchdService(definitionPath);
        const existing = state.services[parsed.service.serviceId];
        if (existing?.loaded) return failure("already loaded");
        const transition = z
          .enum([
            "stopped",
            "waiting",
            "scheduled",
            "xpcproxy",
            "running",
            "failed",
          ])
          .optional()
          .parse(process.env.LOCALBASE_TEST_LAUNCHD_START_TRANSITION);
        const lastExitCode = z.coerce
          .number()
          .int()
          .optional()
          .parse(process.env.LOCALBASE_TEST_LAUNCHD_LAST_EXIT_CODE);
        state.services[parsed.service.serviceId] = transition
          ? {
              ...parsed.service,
              enabled: true,
              loaded: true,
              activeState:
                transition === "scheduled"
                  ? "scheduled"
                  : transition === "running"
                    ? "active"
                    : transition,
              pid:
                transition === "running" || transition === "xpcproxy"
                  ? process.pid
                  : undefined,
              ...(lastExitCode !== undefined ? { lastExitCode } : {}),
            }
          : await startFixtureProcess(
              { ...parsed.service, enabled: true },
              parsed.invocation,
            );
        await writeJson(statePath, state);
        return success();
      }
      if (action === "bootout") {
        const serviceId = targetServiceId(actionArgs[1] ?? "");
        const service = state.services[serviceId];
        if (!service?.loaded) return failure("service not loaded");
        await stopFixtureProcess(service);
        state.services[serviceId] = {
          ...service,
          loaded: false,
          activeState: "inactive",
          pid: undefined,
        };
        await writeJson(statePath, state);
        return success();
      }
      return failure(`unsupported launchctl action ${action}`);
    }

    if (executable !== "systemctl" || args[0] !== "--user") {
      return failure("unsupported manager command");
    }
    if (action === "show") {
      return success(systemdOutput(state.services[actionArgs[1] ?? ""]));
    }
    if (action === "is-enabled") {
      const service = state.services[actionArgs[1] ?? ""];
      return success(service?.enabled ? "enabled\n" : "disabled\n");
    }
    if (action === "daemon-reload") {
      for (const [key, service] of Object.entries(state.services)) {
        if (
          service.manager === "systemd-user" &&
          !(await Bun.file(service.definitionPath).exists())
        ) {
          state.services[key] = { ...service, loaded: false };
        }
      }
      await writeJson(statePath, state);
      return success();
    }
    if (action === "enable") {
      const unitName = actionArgs[1];
      if (!unitName) return failure("missing unit");
      const existing = state.services[unitName];
      const parsed = existing ? undefined : await systemdService(unitName);
      state.services[unitName] = {
        ...(existing ?? parsed?.service),
        enabled: true,
      } as FixtureService;
      await writeJson(statePath, state);
      return success();
    }
    if (action === "disable") {
      const unitName = actionArgs[1];
      const service = unitName ? state.services[unitName] : undefined;
      if (!unitName) return failure("missing unit");
      if (!service) {
        return process.env.LOCALBASE_TEST_SYSTEMD_DISABLE_MISSING === "1"
          ? failure(`Unit file ${unitName} does not exist.`)
          : success();
      }
      state.services[unitName] = { ...service, enabled: false };
      await writeJson(statePath, state);
      return success();
    }
    if (action === "start" || action === "restart") {
      const unitName = actionArgs[1];
      if (!unitName) return failure("missing unit");
      const parsed = await systemdService(unitName);
      const existing = state.services[unitName];
      state.services[unitName] = await startFixtureProcess(
        {
          ...parsed.service,
          enabled: existing?.enabled ?? true,
        },
        parsed.invocation,
      );
      await writeJson(statePath, state);
      return success();
    }
    if (action === "stop") {
      const unitName = actionArgs[1];
      const service = unitName ? state.services[unitName] : undefined;
      if (!unitName || !service) return success();
      await stopFixtureProcess(service);
      state.services[unitName] = {
        ...service,
        activeState: "inactive",
        pid: undefined,
      };
      await writeJson(statePath, state);
      return success();
    }
    return failure(`unsupported systemctl action ${action}`);
  };
}

function positionalValue(args: string[], option: string): string | undefined {
  const index = args.indexOf(option);
  return index === -1 ? undefined : args[index + 1];
}

export async function runManagedGatewayFixture(
  args: string[],
): Promise<number> {
  const root = positionalValue(args, "--root");
  const serviceId = process.env.LOCALBASE_SERVICE_ID;
  const serviceToken = process.env.LOCALBASE_SERVICE_TOKEN;
  const foreground = process.env.LOCALBASE_TEST_FOREGROUND_GATEWAY === "1";
  if (
    !root ||
    (!foreground && (!serviceId || !serviceToken)) ||
    (serviceId !== undefined) !== (serviceToken !== undefined)
  ) {
    return 2;
  }
  const canonical = await canonicalRoot(root);
  let lease:
    Awaited<ReturnType<typeof acquireGatewayLeaseForServe>> | undefined;
  let shutdown!: () => void;
  const stopped = new Promise<void>((resolve) => {
    shutdown = resolve;
  });
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: reservePort(),
    fetch: (request) => {
      const pathname = new URL(request.url).pathname;
      const token = request.headers.get("x-localbase-service-token");
      if (pathname === "/__fixture/stop") {
        if (
          !lease?.instance.serviceToken ||
          token !== lease.instance.serviceToken
        ) {
          return new Response("not found", { status: 404 });
        }
        setTimeout(shutdown, 25);
        return new Response(null, { status: 204 });
      }
      if (pathname === "/_localbase/instance") {
        if (
          request.headers.get("x-localbase-instance-token") !==
          lease?.instance.instanceToken
        ) {
          return new Response(null, { status: 404 });
        }
        return Response.json(
          gatewayIdentitySchema.parse({
            instanceId: lease.instance.instanceId,
            rootHash: lease.instance.rootHash,
          }),
        );
      }
      if (pathname === "/health" && lease) {
        return Response.json(
          gatewayHealthSchema.parse({
            status: "ok",
            version: LOCALBASE_VERSION,
            uptimeSeconds: 0,
            configurationRevision: 0,
            modalities: {
              llm: { configured: true, state: "idle" },
              stt: { configured: false, state: "disabled" },
              image: { configured: false, state: "disabled" },
            },
          }),
        );
      }
      return new Response(null, { status: 404 });
    },
  });
  const port = server.port;
  if (port === undefined) return 1;
  lease = await acquireGatewayLeaseForServe(canonical, {
    host: "127.0.0.1",
    port,
    ...(serviceId ? { serviceId, serviceToken } : {}),
  });

  process.once("SIGTERM", shutdown);
  process.once("SIGINT", shutdown);
  await stopped;
  server.stop(true);
  await lease.release();
  return 0;
}
