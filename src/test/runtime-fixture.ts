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
declare const __LOCALBASE_TEST_HTTP_BACKEND__: boolean | undefined;
declare const __LOCALBASE_TEST_FIRST_EVENT_REPORT_URL__: string | undefined;
export async function compileRuntimeFixture(
  outputPath: string,
  argsPath?: string,
  launchesPath?: string,
  exitOnStart = false,
  failureMarkerPath?: string,
  launchReportUrl?: string,
  httpBackend = false,
  firstEventReportUrl?: string,
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
  define.__LOCALBASE_TEST_HTTP_BACKEND__ = httpBackend ? "true" : "false";
  if (firstEventReportUrl) {
    define.__LOCALBASE_TEST_FIRST_EVENT_REPORT_URL__ =
      JSON.stringify(firstEventReportUrl);
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

function runtimePort(args: string[]): number {
  const index = args.indexOf("--port");
  const port = index === -1 ? NaN : Number(args[index + 1]);
  if (!Number.isSafeInteger(port) || port < 1 || port > 65_535) {
    throw new Error("Runtime fixture requires a valid --port argument.");
  }
  return port;
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
  if (__LOCALBASE_TEST_HTTP_BACKEND__) {
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: runtimePort(args),
      async fetch(request) {
        const path = new URL(request.url).pathname;
        if (path === "/health") return new Response(null, { status: 200 });
        if (path !== "/v1/chat/completions")
          return new Response(null, { status: 404 });
        const stream = new ReadableStream<Uint8Array>({
          async start(controller) {
            if (typeof __LOCALBASE_TEST_FIRST_EVENT_REPORT_URL__ === "string") {
              const response = await fetch(
                __LOCALBASE_TEST_FIRST_EVENT_REPORT_URL__,
                { method: "POST" },
              );
              if (!response.ok) {
                throw new Error("Could not report the first stream event.");
              }
            }
            controller.enqueue(
              new TextEncoder().encode(
                'data: {"id":"fixture","object":"chat.completion.chunk","created":0,"model":"fixture","choices":[{"index":0,"delta":{"role":"assistant","content":"waiting"},"finish_reason":null}]}\n\n',
              ),
            );
          },
        });
        return new Response(stream, {
          headers: { "content-type": "text/event-stream" },
        });
      },
    });
    await new Promise<void>((resolve) => process.once("SIGTERM", resolve));
    server.stop(true);
    return;
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
