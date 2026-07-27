import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  createServiceDefinition,
  parseLaunchdDefinition,
  parseSystemdDefinition,
  resolveServiceInvocation,
  serviceIdentity,
  serviceMetadata,
  servicePlatform,
} from "./definitions";
import { canonicalRoot, canonicalRootHash } from "./ownership";
import { stopFixtureServices } from "../../test/service-manager-fixture";
import { ensureLocalBaseRootMarker } from "../../utils/root";
import { parseLaunchctlStatus } from "./manager";

const projectRoot = join(import.meta.dirname, "../../..");

test("parses only top-level launchd state from captured-style output", () => {
  expect(
    parseLaunchctlStatus(`gui/501/com.localbase.gateway = {
  state = spawn scheduled
  pid = 4321
  last exit code = 0
  coalitions = {
    resource coalition = {
      state = active
      pid = 99999
      last exit code = 42
    }
  }
}`),
  ).toEqual({
    state: "spawn scheduled",
    pid: 4321,
    lastExitCode: 0,
  });
});

test("parses the launchd never-exited sentinel as no exit code", () => {
  expect(
    parseLaunchctlStatus(`gui/501/com.localbase.gateway = {
  state = spawn scheduled
  last exit code = (never exited)
  coalitions = {
    resource coalition = {
      state = active
      last exit code = 42
    }
  }
}`),
  ).toEqual({ state: "spawn scheduled" });
  expect(() =>
    parseLaunchctlStatus(`gui/501/com.localbase.gateway = {
  state = exited
  last exit code = unknown
}`),
  ).toThrow("invalid service state");
});

type CliResult = { exitCode: number; stdout: string; stderr: string };

async function compile(entrypoint: string, outputPath: string): Promise<void> {
  const buildProcess = Bun.spawn(
    [
      process.execPath,
      "build",
      entrypoint,
      "--compile",
      "--no-compile-autoload-dotenv",
      "--no-compile-autoload-bunfig",
      "--asset-naming=[dir]/[name].[ext]",
      `--outfile=${outputPath}`,
    ],
    { cwd: projectRoot, stdout: "pipe", stderr: "pipe" },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    buildProcess.exited,
    new Response(buildProcess.stdout).text(),
    new Response(buildProcess.stderr).text(),
  ]);
  if (exitCode !== 0) {
    throw new Error(`Could not compile ${entrypoint}:\n${stdout}${stderr}`);
  }
}

async function runCli(
  executable: string,
  args: string[],
  environment: Record<string, string>,
): Promise<CliResult> {
  const cliProcess = Bun.spawn([executable, ...args], {
    cwd: projectRoot,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    env: { ...process.env, ...environment },
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    cliProcess.exited,
    new Response(cliProcess.stdout).text(),
    new Response(cliProcess.stderr).text(),
  ]);
  return { exitCode, stdout, stderr };
}

function jsonDocument(output: string): Record<string, unknown> {
  const lines = output.trim().split("\n").filter(Boolean);
  expect(lines).toHaveLength(1);
  return JSON.parse(lines[0]) as Record<string, unknown>;
}

function expectCliSuccess(result: CliResult): void {
  if (result.exitCode !== 0) {
    throw new Error(
      `CLI exited ${result.exitCode}.\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
    );
  }
}

async function ownerRecord(root: string) {
  return (await Bun.file(
    join(root, "runtime", "gateway.lock", "owner.json"),
  ).json()) as {
    instanceId: string;
    pid: number;
    rootHash: string;
    serviceId?: string;
  };
}

async function waitForFile(path: string): Promise<void> {
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    if (await Bun.file(path).exists()) return;
    await Bun.sleep(25);
  }
  throw new Error(`Timed out waiting for ${path}`);
}

async function waitForGatewayReady(
  executable: string,
  root: string,
  environment: Record<string, string>,
): Promise<Record<string, unknown>> {
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    const status = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment,
    );
    if (status.exitCode === 0) {
      const data = jsonDocument(status.stdout).data as Record<string, unknown>;
      if (
        (data.gateway as { state?: string }).state === "ready" &&
        (data.service as { state?: string }).state === "running"
      ) {
        return data;
      }
    }
    await Bun.sleep(25);
  }
  throw new Error("Managed fixture gateway did not become ready.");
}

async function verifyDefinition(
  command: string[],
): Promise<{ exitCode: number; output: string }> {
  const child = Bun.spawn(command, {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { exitCode, output: `${stdout}${stderr}` };
}

test("renders secure definitions and preserves supported path characters", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base definitions-"));
  const root = join(directory, `space ü "quote" \\slash $cash %percent`);
  mkdirSync(root);
  const invocation = {
    program: "/bin/true",
    arguments: ["--root", root, `argument\twith "quotes" \\ $ %`, "serve"],
  };

  try {
    const canonical = await canonicalRoot(root);
    const launchd = await createServiceDefinition(root, invocation, "darwin");
    expect(launchd.manifest.invocation.serviceToken).toBe(launchd.serviceToken);
    expect(parseLaunchdDefinition(launchd.contents)).toEqual({
      label: launchd.serviceId,
      program: invocation.program,
      programArguments: [invocation.program, ...invocation.arguments],
      workingDirectory: canonical,
      runAtLoad: true,
      keepAlive: true,
      exitTimeOut: 15,
      umask: 63,
      processType: "Interactive",
      environment: {
        LOCALBASE_SERVICE_ID: launchd.serviceId,
        LOCALBASE_SERVICE_TOKEN: launchd.serviceToken,
      },
      standardOutPath: `${canonical}/service/stdout.log`,
      standardErrorPath: `${canonical}/service/stderr.log`,
    });

    const systemd = await createServiceDefinition(root, invocation, "linux");
    expect(systemd.manifest.invocation.serviceToken).toBe(systemd.serviceToken);
    expect(parseSystemdDefinition(systemd.contents)).toEqual({
      description: "LocalBase gateway",
      type: "exec",
      execStart: [invocation.program, ...invocation.arguments],
      workingDirectory: canonical,
      restart: "on-failure",
      restartSec: "2s",
      killMode: "mixed",
      killSignal: "SIGTERM",
      timeoutStopSec: "15s",
      umask: "0077",
      environment: {
        LOCALBASE_SERVICE_ID: systemd.serviceId,
        LOCALBASE_SERVICE_TOKEN: systemd.serviceToken,
      },
      standardOutput: "journal",
      standardError: "journal",
      wantedBy: "default.target",
    });
    expect(systemd.contents).toContain("$$");
    expect(systemd.contents).toContain("%%");
    const workingDirectoryLine = systemd.contents
      .split("\n")
      .find((line) => line.startsWith("WorkingDirectory="));
    expect(workingDirectoryLine).toBe(
      `WorkingDirectory=${canonical
        .replaceAll("\\", "\\x5c")
        .replaceAll('"', "\\x22")
        .replaceAll(" ", "\\x20")
        .replaceAll("%", "%%")}`,
    );
    expect(workingDirectoryLine).toContain("ü");
    expect(workingDirectoryLine).toContain("$cash");
    expect(workingDirectoryLine).not.toContain('WorkingDirectory="');
    expect(() =>
      parseSystemdDefinition(systemd.contents.replace("\\x20", "\\q")),
    ).toThrow("Invalid systemd definition");

    await expect(
      createServiceDefinition(
        root,
        { program: "/bin/true", arguments: ["bad\u0007argument"] },
        "linux",
      ),
    ).rejects.toThrow("control characters");
    expect(await serviceIdentity(root)).not.toBe(
      await serviceIdentity(`${root}-other`),
    );
    expect((await serviceMetadata(root, "darwin")).definitionPath).not.toBe(
      (await serviceMetadata(`${root}-other`, "darwin")).definitionPath,
    );
    expect(() => servicePlatform("win32")).toThrow("supported on macOS");
    const sourceInvocation = await resolveServiceInvocation(root);
    expect(sourceInvocation.program).toBe(realpathSync(process.execPath));
    expect(sourceInvocation.arguments).toEqual([
      realpathSync(Bun.main),
      "--root",
      canonical,
      "serve",
    ]);

    if (process.platform === "darwin") {
      const plist = join(directory, "service.plist");
      await Bun.write(plist, launchd.contents);
      const lint = await verifyDefinition(["/usr/bin/plutil", "-lint", plist]);
      expect(lint.exitCode).toBe(0);
    }
    if (
      process.platform === "linux" &&
      (await Bun.file("/usr/bin/systemd-analyze").exists())
    ) {
      const unit = join(directory, "local-base.service");
      await Bun.write(unit, systemd.contents);
      const verify = await verifyDefinition([
        "/usr/bin/systemd-analyze",
        "verify",
        unit,
      ]);
      if (verify.exitCode !== 0) throw new Error(verify.output);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

describe.serial("compiled CLI service lifecycle", () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-service-cli-"));
  const executable = join(directory, "local-base-service-test");
  const home = join(directory, "home");
  const configHome = join(directory, "config");
  const runtimeHome = join(directory, "runtime");
  const darwinStatePath = join(directory, "darwin-state.json");
  const darwinCallsPath = join(directory, "darwin-calls.json");
  const linuxStatePath = join(directory, "linux-state.json");
  const linuxCallsPath = join(directory, "linux-calls.json");

  beforeAll(async () => {
    await compile("src/test/linux-service-cli.ts", executable);
  });

  afterAll(async () => {
    await stopFixtureServices(darwinStatePath);
    await stopFixtureServices(linuxStatePath);
    rmSync(directory, { recursive: true, force: true });
  });

  function environment(
    platform: "darwin" | "linux",
    extra: Record<string, string> = {},
  ) {
    return {
      HOME: home,
      XDG_CONFIG_HOME: configHome,
      XDG_RUNTIME_DIR: runtimeHome,
      LOCALBASE_TEST_PLATFORM: platform,
      LOCALBASE_TEST_DISABLE_CONTINUE_SYNC: "1",
      LOCALBASE_TEST_SERVICE_MANAGER_STATE:
        platform === "darwin" ? darwinStatePath : linuxStatePath,
      LOCALBASE_TEST_SERVICE_MANAGER_CALLS:
        platform === "darwin" ? darwinCallsPath : linuxCallsPath,
      ...extra,
    };
  }

  test("status is database-free and does not initialize an absent root", async () => {
    const root = join(directory, "absent-root");
    const canonical = await canonicalRoot(root);
    const result = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expect(result.exitCode).toBe(0);
    expect(jsonDocument(result.stdout).data).toMatchObject({
      service: {
        state: "not_installed",
        definitionInstalled: false,
        root: canonical,
      },
      gateway: { state: "not_ready" },
    });
    expect(await Bun.file(root).exists()).toBe(false);
  });

  test("starts one root idempotently and reloads a changed definition", async () => {
    const root = join(directory, "darwin-root");
    const first = await runCli(
      executable,
      ["--root", root, "start", "--json"],
      environment("darwin"),
    );
    expect(first.exitCode).toBe(0);
    const firstData = jsonDocument(first.stdout).data as {
      service: {
        definitionPath: string;
        serviceId: string;
        state: string;
        pid: number;
      };
      gateway: { state: string };
    };
    expect(["starting", "running"]).toContain(firstData.service.state);
    const ready = (await waitForGatewayReady(
      executable,
      root,
      environment("darwin"),
    )) as typeof firstData;
    expect(ready.service.state).toBe("running");
    expect(ready.gateway.state).toBe("ready");
    const firstOwner = await ownerRecord(root);
    const canonical = realpathSync(root);
    expect(
      parseLaunchdDefinition(
        await Bun.file(firstData.service.definitionPath).text(),
      ).programArguments,
    ).toEqual([realpathSync(executable), "--root", canonical, "serve"]);

    const repeated = await runCli(
      executable,
      ["--root", root, "start", "--json"],
      environment("darwin"),
    );
    expect(repeated.exitCode).toBe(0);
    expect((await ownerRecord(root)).instanceId).toBe(firstOwner.instanceId);

    await Bun.write(
      firstData.service.definitionPath,
      `${await Bun.file(firstData.service.definitionPath).text()}\n<!-- changed -->\n`,
    );
    const changed = await runCli(
      executable,
      ["--root", root, "start", "--json"],
      environment("darwin"),
    );
    expect(changed.exitCode).toBe(0);
    await waitForGatewayReady(executable, root, environment("darwin"));
    expect((await ownerRecord(root)).instanceId).not.toBe(
      firstOwner.instanceId,
    );

    const calls = (await Bun.file(darwinCallsPath).json()) as string[][];
    expect(calls.filter((args) => args[1] === "bootstrap")).toHaveLength(2);
    expect(
      calls.filter((args) => args[1] === "bootout").length,
    ).toBeGreaterThan(0);
  });

  test("status uses manager state even when the definition is missing", async () => {
    const root = join(directory, "missing-definition-root");
    const started = await runCli(
      executable,
      ["--root", root, "start", "--json"],
      environment("darwin"),
    );
    expect(started.exitCode).toBe(0);
    await waitForGatewayReady(executable, root, environment("darwin"));
    const definitionPath = (
      jsonDocument(started.stdout).data as {
        service: { definitionPath: string };
      }
    ).service.definitionPath;
    rmSync(definitionPath);

    const status = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expect(status.exitCode).toBe(0);
    expect(jsonDocument(status.stdout).data).toMatchObject({
      service: { state: "running", definitionInstalled: false },
      gateway: { state: "ready" },
    });

    const removed = await runCli(
      executable,
      ["--root", root, "uninstall", "--yes", "--json"],
      environment("darwin"),
    );
    expect(removed.exitCode).toBe(0);
    expect(await Bun.file(root).exists()).toBe(false);
  });

  test("stop disables persistence and reset stops before database mutation", async () => {
    const root = join(directory, "reset-root");
    expect(
      (
        await runCli(
          executable,
          ["--root", root, "start", "--json"],
          environment("darwin"),
        )
      ).exitCode,
    ).toBe(0);
    const definitionPath = (
      jsonDocument(
        (
          await runCli(
            executable,
            ["--root", root, "status", "--json"],
            environment("darwin"),
          )
        ).stdout,
      ).data as { service: { definitionPath: string } }
    ).service.definitionPath;

    const reset = await runCli(
      executable,
      ["--root", root, "reset", "--yes", "--json"],
      environment("darwin"),
    );
    expect(reset.exitCode).toBe(0);
    expect(jsonDocument(reset.stdout).data).toMatchObject({
      reset: true,
      root: realpathSync(root),
    });
    expect(await Bun.file(join(root, "runtime", "gateway.lock")).exists()).toBe(
      false,
    );
    expect(await Bun.file(definitionPath).exists()).toBe(true);

    const status = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expect(jsonDocument(status.stdout).data).toMatchObject({
      service: { state: "stopped", definitionInstalled: true },
    });
    const calls = (await Bun.file(darwinCallsPath).json()) as string[][];
    expect(calls.some((args) => args[1] === "disable")).toBe(true);

    const restarted = await runCli(
      executable,
      ["--root", root, "start", "--json"],
      environment("darwin"),
    );
    expectCliSuccess(restarted);
    const stopped = await runCli(
      executable,
      ["--root", root, "stop", "--json"],
      environment("darwin"),
    );
    expect(stopped.exitCode).toBe(0);
    expect(jsonDocument(stopped.stdout).data).toMatchObject({
      service: { state: "stopped", definitionInstalled: true },
    });
  });

  test("serializes reset and uninstall behind a pending managed start", async () => {
    const resetRoot = join(directory, "start-reset-handoff-root");
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", resetRoot, "start", "--json"],
        environment("darwin"),
      ),
    );
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", resetRoot, "reset", "--yes", "--json"],
        environment("darwin"),
      ),
    );
    expect(
      (
        jsonDocument(
          (
            await runCli(
              executable,
              ["--root", resetRoot, "status", "--json"],
              environment("darwin"),
            )
          ).stdout,
        ).data as { service: { state: string } }
      ).service.state,
    ).toBe("stopped");

    const uninstallRoot = join(directory, "start-uninstall-handoff-root");
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", uninstallRoot, "start", "--json"],
        environment("darwin"),
      ),
    );
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", uninstallRoot, "uninstall", "--yes", "--json"],
        environment("darwin"),
      ),
    );
    expect(existsSync(uninstallRoot)).toBe(false);
  });

  test("refuses foreground and ambiguous live owners without signaling them", async () => {
    const foregroundRoot = join(directory, "foreground-root");
    const foreground = Bun.spawn(
      [executable, "--root", foregroundRoot, "serve"],
      {
        stdin: "ignore",
        stdout: "ignore",
        stderr: "ignore",
        env: {
          ...process.env,
          ...environment("darwin", {
            LOCALBASE_TEST_FOREGROUND_GATEWAY: "1",
          }),
        },
      },
    );
    await waitForFile(
      join(foregroundRoot, "runtime", "gateway.lock", "owner.json"),
    );
    const start = await runCli(
      executable,
      ["--root", foregroundRoot, "start", "--json"],
      environment("darwin"),
    );
    expect(start.exitCode).toBe(1);
    expect(jsonDocument(start.stdout)).toMatchObject({
      ok: false,
      error: { code: "operational_error" },
    });
    const reset = await runCli(
      executable,
      ["--root", foregroundRoot, "reset", "--yes", "--json"],
      environment("darwin"),
    );
    expect(reset.exitCode).toBe(1);
    expect(foreground.exitCode).toBeNull();
    foreground.kill("SIGTERM");
    await foreground.exited;

    const ambiguousRoot = join(directory, "ambiguous-root");
    const canonical = await canonicalRoot(ambiguousRoot);
    const lock = join(ambiguousRoot, "runtime", "gateway.lock");
    mkdirSync(lock, { recursive: true });
    writeFileSync(
      join(lock, "owner.json"),
      JSON.stringify({
        version: 1,
        instanceId: crypto.randomUUID(),
        root: canonical,
        rootHash: canonicalRootHash(canonical),
        pid: process.pid,
        startedAt: new Date().toISOString(),
        host: "127.0.0.1",
        port: 65_534,
      }),
    );
    const ambiguous = await runCli(
      executable,
      ["--root", ambiguousRoot, "start", "--json"],
      environment("darwin"),
    );
    expect(ambiguous.exitCode).toBe(1);
    expect(process.pid).toBeGreaterThan(0);
  });

  test("safely reclaims a definitely-dead owner and serializes concurrent starts", async () => {
    const root = join(directory, "stale-root");
    const exited = Bun.spawn([process.execPath, "-e", ""], {
      stdout: "ignore",
      stderr: "ignore",
    });
    await exited.exited;
    const canonical = await canonicalRoot(root);
    ensureLocalBaseRootMarker(canonical);
    const lock = join(root, "runtime", "gateway.lock");
    mkdirSync(lock, { recursive: true });
    writeFileSync(
      join(lock, "owner.json"),
      JSON.stringify({
        version: 1,
        instanceId: crypto.randomUUID(),
        root: canonical,
        rootHash: canonicalRootHash(canonical),
        pid: exited.pid,
        startedAt: new Date().toISOString(),
        host: "127.0.0.1",
        port: 65_533,
      }),
    );

    const [first, second] = await Promise.all([
      runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin"),
      ),
      runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin"),
      ),
    ]);
    expectCliSuccess(first);
    expectCliSuccess(second);
    const data = [first, second].map(
      (result) =>
        (
          jsonDocument(result.stdout).data as {
            service: { serviceId: string };
          }
        ).service.serviceId,
    );
    expect(new Set(data).size).toBe(1);

    const [startRace, stopRace] = await Promise.all([
      runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin"),
      ),
      runCli(
        executable,
        ["--root", root, "stop", "--json"],
        environment("darwin"),
      ),
    ]);
    expectCliSuccess(startRace);
    expectCliSuccess(stopRace);
    expect(jsonDocument(stopRace.stdout).data).toMatchObject({
      service: { state: "stopped" },
      gateway: { state: "not_ready" },
    });
    const status = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expectCliSuccess(status);
    const finalState = (
      jsonDocument(status.stdout).data as {
        service: { state: string };
      }
    ).service.state;
    expect(["running", "stopped"]).toContain(finalState);
  });

  test("reports unavailable and timed-out user managers", async () => {
    const unavailable = await runCli(
      executable,
      ["--root", join(directory, "unavailable-root"), "status", "--json"],
      environment("darwin", {
        LOCALBASE_TEST_SERVICE_MANAGER_UNAVAILABLE: "launchctl",
      }),
    );
    expect(unavailable.exitCode).toBe(1);
    expect(jsonDocument(unavailable.stdout)).toMatchObject({
      ok: false,
      error: { code: "operational_error" },
    });

    const timeout = await runCli(
      executable,
      ["--root", join(directory, "timeout-root"), "status", "--json"],
      environment("darwin", {
        LOCALBASE_TEST_SERVICE_MANAGER_TIMEOUT: "print",
        LOCALBASE_TEST_SERVICE_COMMAND_TIMEOUT_MS: "100",
      }),
    );
    expect(timeout.exitCode).toBe(1);
    expect(`${timeout.stdout}${timeout.stderr}`).toContain("timed out");

    const malformed = await runCli(
      executable,
      ["--root", join(directory, "malformed-root"), "status", "--json"],
      environment("linux", {
        LOCALBASE_TEST_SERVICE_MANAGER_MALFORMED: "show",
      }),
    );
    expect(malformed.exitCode).toBe(1);
    expect(`${malformed.stdout}${malformed.stderr}`).toContain(
      "malformed service state",
    );
  });

  test("disables a deterministic missing systemd unit", async () => {
    const root = join(directory, "missing-systemd-unit-root");
    const stopped = await runCli(
      executable,
      ["--root", root, "stop", "--json"],
      environment("linux", { LOCALBASE_TEST_SYSTEMD_DISABLE_MISSING: "1" }),
    );
    expectCliSuccess(stopped);
    const calls = (await Bun.file(linuxCallsPath).json()) as string[][];
    expect(
      calls.some((args) => args[0] === "systemctl" && args[2] === "disable"),
    ).toBe(true);
  });

  test("accepts transient launchd startup states and rejects immediate failure", async () => {
    for (const transition of ["waiting", "scheduled", "running"] as const) {
      const root = join(directory, `launchd-${transition}-root`);
      const started = await runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin", {
          LOCALBASE_TEST_LAUNCHD_START_TRANSITION: transition,
        }),
      );
      expectCliSuccess(started);
      expect(jsonDocument(started.stdout).data).toMatchObject({
        service: {
          state: "starting",
          managerState:
            transition === "scheduled" ? "spawn scheduled" : transition,
        },
        gateway: { state: "not_ready" },
      });
    }

    const failed = await runCli(
      executable,
      [
        "--root",
        join(directory, "launchd-immediate-failure-root"),
        "start",
        "--json",
      ],
      environment("darwin", {
        LOCALBASE_TEST_LAUNCHD_START_TRANSITION: "failed",
      }),
    );
    expect(failed.exitCode).toBe(1);
    expect(failed.stdout).toContain(
      "LocalBase service manager reported failed after start.",
    );
  });

  test("repairs a loaded launchd job that exited with failure", async () => {
    const root = join(directory, "failed-launchd-root");
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin"),
      ),
    );
    const ready = (await waitForGatewayReady(
      executable,
      root,
      environment("darwin"),
    )) as { service: { serviceId: string } };
    await stopFixtureServices(darwinStatePath);
    const state = (await Bun.file(darwinStatePath).json()) as {
      services: Record<
        string,
        { loaded: boolean; activeState: string; pid?: number }
      >;
    };
    const service = state.services[ready.service.serviceId];
    expect(service).toBeDefined();
    state.services[ready.service.serviceId] = {
      ...service!,
      loaded: true,
      activeState: "scheduled",
      pid: undefined,
    };
    await Bun.write(darwinStatePath, JSON.stringify(state));
    const scheduled = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expectCliSuccess(scheduled);
    expect(jsonDocument(scheduled.stdout).data).toMatchObject({
      service: { state: "starting", managerState: "spawn scheduled" },
    });

    state.services[ready.service.serviceId] = {
      ...service!,
      loaded: true,
      activeState: "failed",
      pid: undefined,
    };
    await Bun.write(darwinStatePath, JSON.stringify(state));

    const failed = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expectCliSuccess(failed);
    expect(jsonDocument(failed.stdout).data).toMatchObject({
      service: { state: "failed" },
      gateway: { state: "not_ready" },
    });

    expectCliSuccess(
      await runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin"),
      ),
    );
    expect(
      (
        (await waitForGatewayReady(
          executable,
          root,
          environment("darwin"),
        )) as { service: { state: string } }
      ).service.state,
    ).toBe("running");
  });

  test("rejects a managed label whose token-bound owner does not match", async () => {
    const root = join(directory, "identity-mismatch-root");
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("darwin"),
      ),
    );
    await waitForGatewayReady(executable, root, environment("darwin"));
    const ownerPath = join(root, "runtime", "gateway.lock", "owner.json");
    const original = await Bun.file(ownerPath).text();
    const owner = JSON.parse(original) as Record<string, unknown>;
    await Bun.write(
      ownerPath,
      JSON.stringify({ ...owner, serviceToken: crypto.randomUUID() }),
    );

    const status = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("darwin"),
    );
    expectCliSuccess(status);
    expect(jsonDocument(status.stdout).data).toMatchObject({
      service: { state: "unknown" },
    });
    const stop = await runCli(
      executable,
      ["--root", root, "stop", "--json"],
      environment("darwin"),
    );
    expect(stop.exitCode).toBe(1);

    await Bun.write(ownerPath, original);
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", root, "stop", "--json"],
        environment("darwin"),
      ),
    );
  });

  test("does not associate a reused systemd PID with the managed gateway", async () => {
    const root = join(directory, "systemd-pid-mismatch-root");
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", root, "start", "--json"],
        environment("linux"),
      ),
    );
    await waitForGatewayReady(executable, root, environment("linux"));
    const original = await Bun.file(linuxStatePath).text();
    const state = JSON.parse(original) as {
      services: Record<string, { root: string; pid?: number }>;
    };
    const service = Object.values(state.services).find(
      (candidate) => candidate.root === realpathSync(root),
    );
    if (!service) throw new Error("Missing systemd fixture state.");
    service.pid = process.pid;
    await Bun.write(linuxStatePath, JSON.stringify(state));

    const status = await runCli(
      executable,
      ["--root", root, "status", "--json"],
      environment("linux"),
    );
    expectCliSuccess(status);
    expect(jsonDocument(status.stdout).data).toMatchObject({
      service: { state: "unknown" },
    });

    await Bun.write(linuxStatePath, original);
    expectCliSuccess(
      await runCli(
        executable,
        ["--root", root, "stop", "--json"],
        environment("linux"),
      ),
    );
  });

  test("runs the systemd user lifecycle with strict status parsing", async () => {
    const root = join(directory, "linux-root");
    const started = await runCli(
      executable,
      ["--root", root, "start", "--json"],
      environment("linux"),
    );
    expect(started.exitCode).toBe(0);
    const startedData = jsonDocument(started.stdout).data as {
      service: {
        definitionPath: string;
        serviceId: string;
        state: string;
      };
      gateway: { state: string };
    };
    expect(["starting", "running"]).toContain(startedData.service.state);
    await waitForGatewayReady(executable, root, environment("linux"));
    const unit = parseSystemdDefinition(
      await Bun.file(startedData.service.definitionPath).text(),
    );
    expect(unit.execStart).toEqual([
      realpathSync(executable),
      "--root",
      realpathSync(root),
      "serve",
    ]);

    const firstOwner = await ownerRecord(root);
    const restarted = await runCli(
      executable,
      ["--root", root, "restart", "--json"],
      environment("linux"),
    );
    expect(restarted.exitCode).toBe(0);
    await waitForGatewayReady(executable, root, environment("linux"));
    expect((await ownerRecord(root)).instanceId).not.toBe(
      firstOwner.instanceId,
    );

    const stopped = await runCli(
      executable,
      ["--root", root, "stop", "--json"],
      environment("linux"),
    );
    expect(stopped.exitCode).toBe(0);
    expect(jsonDocument(stopped.stdout).data).toMatchObject({
      service: { state: "stopped", manager: "systemd-user" },
    });

    const removed = await runCli(
      executable,
      ["--root", root, "uninstall", "--yes", "--json"],
      environment("linux"),
    );
    expect(removed.exitCode).toBe(0);
    expect(await Bun.file(root).exists()).toBe(false);
    const calls = (await Bun.file(linuxCallsPath).json()) as string[][];
    expect(calls.some((args) => args[2] === "enable")).toBe(true);
    expect(calls.some((args) => args[2] === "disable")).toBe(true);
    expect(calls.some((args) => args[2] === "show")).toBe(true);
  });
});
