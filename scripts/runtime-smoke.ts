import { mkdir, rm } from "node:fs/promises";
import { z } from "zod";
import { unzipSync } from "fflate";
import { gatewayHealthSchema } from "../src/domains/runtime/health";
import { openAIErrorResponseSchema } from "../src/domains/runtime/openai-error";
import { diagnosticsManifestSchema } from "../src/domains/diagnostics/commands/diagnostics";
import { serviceLifecycleResultSchema } from "../src/domains/app/commands/results";
import { commandSuccessSchema } from "../src/domains/app/commands/output";

type CommandResult = {
  exitCode: number;
  stdout: string;
  stderr: string;
};

export type SmokeTarget =
  "macos-arm64" | "linux-x64" | "macos-x64" | "linux-arm64";

function hostSmokeTarget(): SmokeTarget {
  if (process.platform === "darwin" && process.arch === "arm64") {
    return "macos-arm64";
  }
  if (process.platform === "darwin" && process.arch === "x64") {
    return "macos-x64";
  }
  if (process.platform === "linux" && process.arch === "x64") {
    return "linux-x64";
  }
  if (process.platform === "linux" && process.arch === "arm64") {
    return "linux-arm64";
  }
  throw new Error(
    `Runtime smoke is unsupported on ${process.platform}/${process.arch}.`,
  );
}

export function resolveSmokeTarget(
  value = process.env.LOCALBASE_SMOKE_TARGET,
): SmokeTarget {
  if (value === undefined) return hostSmokeTarget();
  if (
    value === "macos-arm64" ||
    value === "linux-x64" ||
    value === "macos-x64" ||
    value === "linux-arm64"
  ) {
    return value;
  }
  throw new Error(
    `LOCALBASE_SMOKE_TARGET must be one of macos-arm64, linux-x64, macos-x64, or linux-arm64; received ${value ?? "<unset>"}.`,
  );
}

function isManagedTarget(target: SmokeTarget): boolean {
  return target === "macos-arm64" || target === "linux-x64";
}

type RunningGateway = {
  process: Bun.Subprocess;
  stdout: Promise<string>;
  stderr: Promise<string>;
  baseUrl: string;
  sttPort: number;
};

const SMOKE_TIMEOUT_MS = 60_000;
const STARTUP_TIMEOUT_MS = 20_000;
const STT_MODEL_ID = "whisper-tiny-en-q8_0";
const STT_MODEL_FILE = "ggml-tiny.en-q8_0.bin";
const CLI_ONLY_LLM_MODEL_FILE = "runtime-smoke-llm.gguf";
const transcriptionResponseSchema = z
  .object({ text: z.string() })
  .passthrough();
const temporaryStartupErrorSchema = z
  .object({
    error: z
      .object({
        message: z
          .string()
          .regex(/^STT service is currently restarting or unavailable\./),
        type: z.literal("api_error"),
        param: z.null(),
        code: z.literal("service_unavailable"),
      })
      .passthrough(),
  })
  .passthrough();
const runtimeReceiptSchema = z
  .object({ runtimes: z.record(z.string(), z.unknown()) })
  .passthrough();

function cliCommand(): string[] {
  const binary = process.env.LOCALBASE_SMOKE_CLI;
  if (!binary?.startsWith("/")) {
    throw new Error(
      "LOCALBASE_SMOKE_CLI must be the absolute path to the extracted CLI.",
    );
  }
  return [binary];
}

export function buildConfigureArgs(
  root: string,
  target: SmokeTarget = "linux-x64",
): string[] {
  const args = [
    "--root",
    root,
    "--non-interactive",
    "configure",
    "--defaults",
    "--no-create-key",
  ];
  if (isManagedTarget(target)) {
    args.splice(
      -1,
      0,
      "--stt-models",
      STT_MODEL_ID,
      "--active-stt",
      STT_MODEL_ID,
    );
  } else {
    args.splice(-1, 0, "--stt-models", "", "--image-models", "");
  }
  return args;
}

export function buildServeArgs(
  root: string,
  gatewayPort: number,
  sttPort: number,
  target: SmokeTarget = "linux-x64",
): string[] {
  return [
    "--root",
    root,
    "--non-interactive",
    "serve",
    "--host",
    "127.0.0.1",
    "--port",
    String(gatewayPort),
    ...(isManagedTarget(target) ? ["--no-llm"] : []),
    ...(isManagedTarget(target)
      ? ["--stt", "--stt-host", "127.0.0.1", "--stt-port", String(sttPort)]
      : ["--no-stt", "--llm-model-file", CLI_ONLY_LLM_MODEL_FILE]),
    "--no-image",
    "--no-auth",
    "--bypass-memory-check",
  ];
}

export function buildUninstallArgs(root: string): string[] {
  return ["--root", root, "--non-interactive", "uninstall", "--yes"];
}

export function buildDiagnosticsArgs(root: string, output: string): string[] {
  return [
    "--root",
    root,
    "--non-interactive",
    "diagnostics",
    "--output",
    output,
    "--json",
  ];
}

function commandEnvironment(root: string): Record<string, string> {
  return {
    ...process.env,
    HOME: `${root}.home`,
    TMPDIR: `${root}.tmp`,
    XDG_CONFIG_HOME: `${root}.config`,
    LOCALBASE_ROOT: root,
  } as Record<string, string>;
}

async function outputOf(
  stream: ReadableStream<Uint8Array> | number | undefined,
): Promise<string> {
  if (!stream || typeof stream === "number") return "";
  return await new Response(stream).text();
}

async function runCli(args: string[], root: string): Promise<CommandResult> {
  const process = Bun.spawn([...cliCommand(), ...args], {
    env: commandEnvironment(root),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    outputOf(process.stdout),
    outputOf(process.stderr),
    process.exited,
  ]);
  return { exitCode, stdout, stderr };
}

async function prepareCliOnlyLlmFixture(root: string): Promise<void> {
  await Bun.write(`${root}/models/llm/${CLI_ONLY_LLM_MODEL_FILE}`, "smoke");
}

function describe(result: CommandResult): string {
  return `exit ${result.exitCode}\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`;
}

function stripAnsi(value: string): string {
  return value.replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "");
}

async function expectCli(args: string[], root: string): Promise<CommandResult> {
  const result = await runCli(args, root);
  if (result.exitCode !== 0) {
    throw new Error(
      `local-base ${args.join(" ")} failed:\n${describe(result)}`,
    );
  }
  return result;
}

function reservePort(): number {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const bytes = new Uint32Array(1);
    crypto.getRandomValues(bytes);
    const port = 20_000 + (bytes[0] % 40_000);
    try {
      const server = Bun.serve({
        hostname: "127.0.0.1",
        port,
        fetch: () => new Response("reserved"),
      });
      server.stop(true);
      return port;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EADDRINUSE") throw error;
    }
  }
  throw new Error("Could not reserve a local port for the runtime smoke test.");
}

async function waitForHealthyGateway(
  gateway: Bun.Subprocess,
  baseUrl: string,
  target: SmokeTarget,
): Promise<void> {
  const deadline = Date.now() + STARTUP_TIMEOUT_MS;
  let lastError = "no response";
  while (Date.now() < deadline) {
    if (gateway.exitCode !== null) {
      throw new Error(
        `Gateway exited during startup with code ${gateway.exitCode}.`,
      );
    }
    try {
      const response = await fetch(`${baseUrl}/health`, {
        signal: AbortSignal.timeout(500),
      });
      const body = gatewayHealthSchema.safeParse(await response.json());
      const ready =
        body.success &&
        (isManagedTarget(target)
          ? body.data.modalities.stt.configured
          : body.data.modalities.llm.configured &&
            !body.data.modalities.stt.configured &&
            !body.data.modalities.image.configured);
      if (response.ok && ready) {
        return;
      }
      lastError = `unexpected health response: HTTP ${response.status}`;
    } catch (error) {
      lastError = error instanceof Error ? error.message : String(error);
    }
    await Bun.sleep(100);
  }
  throw new Error(`Gateway did not become ready: ${lastError}`);
}

async function startGateway(
  root: string,
  target: SmokeTarget,
): Promise<RunningGateway> {
  const gatewayPort = reservePort();
  const sttPort = isManagedTarget(target) ? reservePort() : 0;
  const process = Bun.spawn(
    [...cliCommand(), ...buildServeArgs(root, gatewayPort, sttPort, target)],
    { env: commandEnvironment(root), stdout: "pipe", stderr: "pipe" },
  );
  const stdout = outputOf(process.stdout);
  const stderr = outputOf(process.stderr);
  try {
    const baseUrl = `http://127.0.0.1:${gatewayPort}`;
    await waitForHealthyGateway(process, baseUrl, target);
    return { process, stdout, stderr, baseUrl, sttPort };
  } catch (error) {
    if (process.exitCode === null) process.kill(9);
    const [out, err] = await Promise.all([stdout, stderr]);
    throw new Error(`${error}\nstdout:\n${out}\nstderr:\n${err}`, {
      cause: error,
    });
  }
}

function silentWav(): Uint8Array {
  const sampleRate = 16_000;
  const samples = sampleRate / 10;
  const bytesPerSample = 2;
  const dataBytes = samples * bytesPerSample;
  const wav = new Uint8Array(44 + dataBytes);
  const view = new DataView(wav.buffer);
  const writeText = (offset: number, value: string) => {
    for (let index = 0; index < value.length; index += 1) {
      wav[offset + index] = value.charCodeAt(index);
    }
  };

  writeText(0, "RIFF");
  view.setUint32(4, 36 + dataBytes, true);
  writeText(8, "WAVEfmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * bytesPerSample, true);
  view.setUint16(32, bytesPerSample, true);
  view.setUint16(34, 16, true);
  writeText(36, "data");
  view.setUint32(40, dataBytes, true);
  return wav;
}

function describeBody(payload: unknown): string {
  return JSON.stringify(payload);
}

function describeValidation(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "body"}: ${issue.message}`)
    .join("; ");
}

async function readJsonBody(response: Response): Promise<unknown> {
  const text = await response.text();
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new Error(
      `Gateway returned invalid JSON for HTTP ${response.status}: ${text || "<empty body>"}`,
      { cause: error },
    );
  }
}

function retryAfterMilliseconds(response: Response): number {
  const value = response.headers.get("retry-after");
  if (value === null) return 0;
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds < 0) {
    throw new Error(`Gateway returned an invalid Retry-After header: ${value}`);
  }
  return seconds * 1_000;
}

function wait(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

export async function transcribe(baseUrl: string): Promise<void> {
  const deadline = Date.now() + SMOKE_TIMEOUT_MS;
  let attempts = 0;

  while (Date.now() < deadline) {
    attempts += 1;
    const body = new FormData();
    body.append(
      "file",
      new Blob([silentWav()], { type: "audio/wav" }),
      "silence.wav",
    );
    body.append("model", STT_MODEL_ID);
    const response = await fetch(`${baseUrl}/v1/audio/transcriptions`, {
      method: "POST",
      body,
      signal: AbortSignal.timeout(Math.max(1, deadline - Date.now())),
    });
    const payload = await readJsonBody(response).catch((error) => {
      throw new Error(`Transcription attempt ${attempts} failed: ${error}`, {
        cause: error,
      });
    });

    if (response.ok) {
      const transcription = transcriptionResponseSchema.safeParse(payload);
      if (transcription.success) return;
      throw new Error(
        `Expected a successful Whisper transcription, received HTTP ${response.status} with an invalid response body: ${describeValidation(transcription.error)} (${describeBody(payload)})`,
      );
    }

    if (
      response.status === 503 &&
      temporaryStartupErrorSchema.safeParse(payload).success
    ) {
      const delay = retryAfterMilliseconds(response);
      const remaining = deadline - Date.now();
      if (delay >= remaining) break;
      // The gateway supplies the retry window; the smoke test does not invent a delay.
      if (delay > 0) await wait(delay);
      continue;
    }

    const errorResponse = openAIErrorResponseSchema.safeParse(payload);
    if (!errorResponse.success) {
      throw new Error(
        `Transcription failed with HTTP ${response.status} and an invalid error body: ${describeValidation(errorResponse.error)} (${describeBody(payload)})`,
      );
    }
    throw new Error(
      `Transcription failed with HTTP ${response.status}, code ${errorResponse.data.error.code ?? "<none>"}: ${errorResponse.data.error.message}`,
    );
  }

  throw new Error(
    `Whisper did not become available within ${SMOKE_TIMEOUT_MS}ms after ${attempts} startup attempts.`,
  );
}

async function stopGateway(gateway: RunningGateway): Promise<void> {
  if (gateway.process.exitCode === null) gateway.process.kill(15);
  try {
    await Promise.race([
      gateway.process.exited,
      Bun.sleep(STARTUP_TIMEOUT_MS).then(() => {
        throw new Error("Gateway did not exit after SIGTERM.");
      }),
    ]);
  } catch (error) {
    if (gateway.process.exitCode === null) gateway.process.kill(9);
    await gateway.process.exited;
    throw error;
  }
  const [stdout, stderr] = await Promise.all([gateway.stdout, gateway.stderr]);
  if (gateway.process.exitCode !== 0) {
    throw new Error(
      `Gateway exited with code ${gateway.process.exitCode}.\nstdout:\n${stdout}\nstderr:\n${stderr}`,
    );
  }
}

async function waitForClosedPort(port: number): Promise<void> {
  const deadline = Date.now() + STARTUP_TIMEOUT_MS;
  while (Date.now() < deadline) {
    try {
      await fetch(`http://127.0.0.1:${port}/health`, {
        signal: AbortSignal.timeout(500),
      });
    } catch {
      return;
    }
    await Bun.sleep(100);
  }
  throw new Error(
    `Whisper runtime remained reachable on port ${port} after shutdown.`,
  );
}

async function verifyInstalledArtifacts(root: string): Promise<void> {
  const binary = Bun.file(`${root}/bin/whisper-server`);
  const receipt = Bun.file(`${root}/bin/.managed-binaries.json`);
  const model = Bun.file(`${root}/models/stt/${STT_MODEL_FILE}`);
  if (!(await binary.exists()) || !(await model.exists())) {
    throw new Error(
      "LocalBase did not install the pinned Whisper runtime and model.",
    );
  }
  const installed = runtimeReceiptSchema.safeParse(await receipt.json());
  if (
    !installed.success ||
    !Object.hasOwn(installed.data.runtimes, "whisper-server")
  ) {
    throw new Error(
      "LocalBase did not record the verified Whisper runtime receipt.",
    );
  }
}

async function verifyCliOnlyArtifacts(root: string): Promise<void> {
  if (
    (await Bun.file(`${root}/bin/whisper-server`).exists()) ||
    (await Bun.file(`${root}/bin/.managed-binaries.json`).exists())
  ) {
    throw new Error(
      "A CLI-only target attempted to install a managed runtime.",
    );
  }
}

async function verifyDiagnosticsArchive(path: string): Promise<void> {
  const archive = unzipSync(await Bun.file(path).bytes());
  const names = Object.keys(archive);
  if (names.join(",") !== "manifest.json,logs/events.jsonl") {
    throw new Error(`Diagnostics archive entries were unexpected: ${names}`);
  }
  const manifest = diagnosticsManifestSchema.safeParse(
    JSON.parse(new TextDecoder().decode(archive["manifest.json"]!)),
  );
  if (!manifest.success) {
    throw new Error("Diagnostics archive manifest failed schema validation.");
  }
}

function verifyStatusResult(result: CommandResult): void {
  let payload: unknown;
  try {
    payload = JSON.parse(result.stdout);
  } catch (error) {
    throw new Error("Status did not return valid JSON.", { cause: error });
  }
  const envelope = commandSuccessSchema.safeParse(payload);
  if (
    !envelope.success ||
    !serviceLifecycleResultSchema.safeParse(envelope.data.data).success
  ) {
    throw new Error("Status returned an invalid lifecycle response.");
  }
}

export async function runRuntimeSmoke(): Promise<void> {
  const target = resolveSmokeTarget();
  const root = `${process.env.RUNNER_TEMP ?? "/tmp"}/localbase-runtime-smoke-${crypto.randomUUID()}`;
  try {
    await Promise.all([
      mkdir(`${root}.home`, { recursive: true }),
      mkdir(`${root}.tmp`, { recursive: true }),
      mkdir(`${root}.config`, { recursive: true }),
    ]);
    const help = await expectCli(["--help"], root);
    if (!stripAnsi(help.stdout).includes("USAGE local-base ")) {
      throw new Error("CLI help did not produce the expected usage banner.");
    }

    await expectCli(buildConfigureArgs(root, target), root);
    if (!(await Bun.file(`${root}/local-base.db`).exists())) {
      throw new Error("The compiled CLI did not create its migrated database.");
    }
    if (!isManagedTarget(target)) await prepareCliOnlyLlmFixture(root);
    verifyStatusResult(
      await expectCli(["--root", root, "status", "--json"], root),
    );
    await expectCli(
      buildDiagnosticsArgs(root, `${root}/diagnostics-configured.zip`),
      root,
    );
    await verifyDiagnosticsArchive(`${root}/diagnostics-configured.zip`);

    const running = await startGateway(root, target);
    try {
      verifyStatusResult(
        await expectCli(["--root", root, "status", "--json"], root),
      );
      await expectCli(
        buildDiagnosticsArgs(root, `${root}/diagnostics-running.zip`),
        root,
      );
      await verifyDiagnosticsArchive(`${root}/diagnostics-running.zip`);
      if (isManagedTarget(target)) {
        await transcribe(running.baseUrl);
        await verifyInstalledArtifacts(root);
      } else {
        await verifyCliOnlyArtifacts(root);
      }
      await stopGateway(running);
      if (isManagedTarget(target)) await waitForClosedPort(running.sttPort);
    } finally {
      if (running.process.exitCode === null) await stopGateway(running);
    }

    await expectCli(
      buildDiagnosticsArgs(root, `${root}/diagnostics-stopped.zip`),
      root,
    );
    await verifyDiagnosticsArchive(`${root}/diagnostics-stopped.zip`);
    await expectCli(buildUninstallArgs(root), root);
    if (await Bun.file(root).exists()) {
      throw new Error(
        "LocalBase uninstall did not remove the smoke-test root.",
      );
    }
  } finally {
    await Promise.all(
      [root, `${root}.home`, `${root}.tmp`, `${root}.config`].map((path) =>
        rm(path, { recursive: true, force: true }),
      ),
    );
  }
}
