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
declare const __LOCALBASE_TEST_CAPABILITY__: string | undefined;

type RuntimeFixtureCapability =
  "localbase-sd-gpu-pci-v1" | "localbase-whisper-gpu-pci-v1";

export type RuntimeFixtureOptions = {
  argsPath?: string;
  capability?: RuntimeFixtureCapability;
  environmentPath?: string;
  exitOnStart?: boolean;
  failureMarkerPath?: string;
  firstEventReportUrl?: string;
  httpBackend?: boolean;
  launchReportUrl?: string;
  launchesPath?: string;
};

export async function compileRuntimeFixture(
  outputPath: string,
  options: RuntimeFixtureOptions = {},
): Promise<void> {
  const define: Record<string, string> = {};
  if (options.argsPath) {
    define["process.env.LOCALBASE_TEST_ARGS_PATH"] = JSON.stringify(
      options.argsPath,
    );
  }
  if (options.launchesPath) {
    define.__LOCALBASE_TEST_LAUNCHES_PATH__ = JSON.stringify(
      options.launchesPath,
    );
  }
  if (options.exitOnStart) define.__LOCALBASE_TEST_EXIT_ON_START__ = "true";
  if (options.failureMarkerPath) {
    define.__LOCALBASE_TEST_FAILURE_MARKER_PATH__ = JSON.stringify(
      options.failureMarkerPath,
    );
  }
  if (options.launchReportUrl) {
    define.__LOCALBASE_TEST_LAUNCH_REPORT_URL__ = JSON.stringify(
      options.launchReportUrl,
    );
  }
  define.__LOCALBASE_TEST_HTTP_BACKEND__ = options.httpBackend
    ? "true"
    : "false";
  if (options.firstEventReportUrl) {
    define.__LOCALBASE_TEST_FIRST_EVENT_REPORT_URL__ = JSON.stringify(
      options.firstEventReportUrl,
    );
  }
  if (options.environmentPath) {
    define["process.env.LOCALBASE_TEST_ENVIRONMENT_PATH"] = JSON.stringify(
      options.environmentPath,
    );
  }
  if (options.capability) {
    define.__LOCALBASE_TEST_CAPABILITY__ = JSON.stringify(options.capability);
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
  if (args[0] === "--localbase-capabilities") {
    if (typeof __LOCALBASE_TEST_CAPABILITY__ !== "string") {
      process.exit(2);
    }
    console.log(__LOCALBASE_TEST_CAPABILITY__);
    return;
  }
  const argsPath = process.env.LOCALBASE_TEST_ARGS_PATH;
  const environmentPath = process.env.LOCALBASE_TEST_ENVIRONMENT_PATH;
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

  if (environmentPath) {
    await Bun.write(environmentPath, process.env.LD_LIBRARY_PATH ?? "");
  }
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
    const stopped = new Promise<void>((resolve) =>
      process.once("SIGTERM", resolve),
    );
    if (argsPath) await Bun.write(argsPath, `${args.join("\n")}\n`);
    await stopped;
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
  if (argsPath) await Bun.write(argsPath, `${args.join("\n")}\n`);
}

if (import.meta.main) await runRuntimeFixture();
