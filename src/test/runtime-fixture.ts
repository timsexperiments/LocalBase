import { appendFileSync } from "node:fs";

declare const __LOCALBASE_TEST_LAUNCHES_PATH__: string | undefined;
declare const __LOCALBASE_TEST_EXIT_ON_START__: boolean | undefined;

export async function compileRuntimeFixture(
  outputPath: string,
  argsPath?: string,
  launchesPath?: string,
  exitOnStart = false,
): Promise<void> {
  const define: Record<string, string> = {};
  if (argsPath) {
    define["process.env.LOCALBASE_TEST_ARGS_PATH"] = JSON.stringify(argsPath);
  }
  if (launchesPath) {
    define.__LOCALBASE_TEST_LAUNCHES_PATH__ = JSON.stringify(launchesPath);
  }
  if (exitOnStart) define.__LOCALBASE_TEST_EXIT_ON_START__ = "true";
  const result = await Bun.build({
    entrypoints: [import.meta.path],
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
  if (pidPath) await Bun.write(pidPath, `${process.pid}\n`);
  if (parentPidPath) await Bun.write(parentPidPath, `${process.ppid}\n`);
  if (
    typeof __LOCALBASE_TEST_EXIT_ON_START__ === "boolean" &&
    __LOCALBASE_TEST_EXIT_ON_START__
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
