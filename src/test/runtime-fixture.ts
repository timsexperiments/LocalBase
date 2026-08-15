import { appendFileSync } from "node:fs";
import { join } from "node:path";

const runtimeFixtureEntrypoint = join(
  import.meta.dirname,
  "runtime-fixture.ts",
);

declare const __LOCALBASE_TEST_LAUNCHES_PATH__: string | undefined;
declare const __LOCALBASE_TEST_EXIT_ON_START__: boolean | undefined;
declare const __LOCALBASE_TEST_FAILURE_MARKER_PATH__: string | undefined;
declare const __LOCALBASE_TEST_LAUNCH_REPORT_URL__: string | undefined;
export async function compileRuntimeFixture(
  outputPath: string,
  argsPath?: string,
  launchesPath?: string,
  exitOnStart = false,
  failureMarkerPath?: string,
  launchReportUrl?: string,
): Promise<void> {
  const define: Record<string, string> = {};
  if (argsPath) {
    define["process.env.LOCALBASE_TEST_ARGS_PATH"] = JSON.stringify(argsPath);
  }
  if (launchesPath) {
    define.__LOCALBASE_TEST_LAUNCHES_PATH__ = JSON.stringify(launchesPath);
  }
  if (exitOnStart) define.__LOCALBASE_TEST_EXIT_ON_START__ = "true";
  if (failureMarkerPath) {
    define.__LOCALBASE_TEST_FAILURE_MARKER_PATH__ =
      JSON.stringify(failureMarkerPath);
  }
  if (launchReportUrl) {
    define.__LOCALBASE_TEST_LAUNCH_REPORT_URL__ =
      JSON.stringify(launchReportUrl);
  }
  const result = await Bun.build({
    entrypoints: [runtimeFixtureEntrypoint],
    target: "bun",
    compile: { outfile: outputPath },
    define: Object.keys(define).length > 0 ? define : undefined,
  });
  if (!result.success) {
    throw new Error(
      `Could not compile runtime fixture: ${result.logs.map((log) => log.message).join("\n")}`,
    );
  }
}

async function runRuntimeFixture(): Promise<void> {
  const args = Bun.argv.slice(2);
  const argsPath = process.env.LOCALBASE_TEST_ARGS_PATH;
  const launchesPath =
    typeof __LOCALBASE_TEST_LAUNCHES_PATH__ === "string"
      ? __LOCALBASE_TEST_LAUNCHES_PATH__
      : process.env.LOCALBASE_TEST_LAUNCHES_PATH;
  const supplementaryPath = process.env.LOCALBASE_TEST_SUPPLEMENTARY_PATH;
  const pidPath = process.env.LOCALBASE_TEST_PID_PATH;
  const parentPidPath = process.env.LOCALBASE_TEST_PARENT_PID_PATH;
  const ignoreSigterm = process.env.LOCALBASE_TEST_IGNORE_SIGTERM === "1";

  if (supplementaryPath && !(await Bun.file(supplementaryPath).exists())) {
    process.exit(41);
  }

  if (argsPath) await Bun.write(argsPath, `${args.join("\n")}\n`);
  if (launchesPath) {
    appendFileSync(launchesPath, `${JSON.stringify(args)}\n`);
  }
  if (typeof __LOCALBASE_TEST_LAUNCH_REPORT_URL__ === "string") {
    const response = await fetch(__LOCALBASE_TEST_LAUNCH_REPORT_URL__, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(args),
    });
    if (!response.ok) throw new Error("Could not report runtime startup.");
  }
  if (pidPath) await Bun.write(pidPath, `${process.pid}\n`);
  if (parentPidPath) await Bun.write(parentPidPath, `${process.ppid}\n`);
  if (
    typeof __LOCALBASE_TEST_EXIT_ON_START__ === "boolean" &&
    __LOCALBASE_TEST_EXIT_ON_START__
  ) {
    process.exit(1);
  }
  if (
    typeof __LOCALBASE_TEST_FAILURE_MARKER_PATH__ === "string" &&
    (await Bun.file(__LOCALBASE_TEST_FAILURE_MARKER_PATH__).exists())
  ) {
    process.exit(1);
  }
  const keepAlive = setInterval(() => {}, 60_000);
  process.on("SIGTERM", () => {
    if (ignoreSigterm) return;
    clearInterval(keepAlive);
    process.exit(0);
  });
}

if (import.meta.main) await runRuntimeFixture();
