import {
  defineCommand,
  type ArgDef,
  type ArgsDef,
  type CommandDef,
} from "citty";
import { z } from "zod";
import type { AppContext, MinimalAppContext } from "../../../context";
import { byId, type ModelKind } from "../../../catalog";
import {
  catalogInputSchema,
  configureInputSchema,
  diagnosticsInputSchema,
  doctorInputSchema,
  initInputSchema,
  installedInputSchema,
  installInputSchema,
  keyIdInputSchema,
  keysCreateInputSchema,
  keysListInputSchema,
  logsInputSchema,
  recommendInputSchema,
  resetInputSchema,
  serviceInputSchema,
  serveInputSchema,
  uninstallInputSchema,
  type CatalogInput,
  type ConfigureInput,
  type DiagnosticsInput,
  type DoctorInput,
  type GlobalOptions,
  type InitInput,
  type InstalledInput,
  type InstallInput,
  type KeyIdInput,
  type KeysCreateInput,
  type KeysListInput,
  type LogsInput,
  type RecommendInput,
  type ResetInput,
  type ServiceInput,
  type ServeInput,
  type UninstallInput,
} from "./inputs";
import type { CommandOutput, CommandResult } from "./output";
import { CliInputError } from "./errors";
import {
  catalogResultSchema,
  configureResultSchema,
  diagnosticsResultSchema,
  doctorResultSchema,
  initResultSchema,
  installedResultSchema,
  installResultSchema,
  keyRevocationResultSchema,
  keySecretResultSchema,
  keysListResultSchema,
  logsResultSchema,
  recommendResultSchema,
  resetResultSchema,
  serviceLifecycleResultSchema,
  serveResultSchema,
  uninstallResultSchema,
} from "./results";

export { CliInputError } from "./errors";

export type CommandExecution = {
  global: GlobalOptions;
  output: CommandOutput;
};

type Positionals = {
  minimum?: number;
  maximum?: number;
};

export type CittyCommand = CommandDef<any>;

type CommandBase<Input> = {
  path: readonly string[];
  description: string;
  examples?: readonly string[];
  args?: ArgsDef;
  positionals?: Positionals;
  requiresDatabase?: boolean;
  readOnlyConfiguration?: boolean;
  initializeUnderOperationLock?: boolean;
  longRunning?: boolean;
  streaming?(input: Input): boolean;
  resultSchema: z.ZodType;
  citty: CittyCommand;
  parse(input: Record<string, unknown>, positionals: string[]): Input;
  validate?(input: Input, global: GlobalOptions): Input;
};

type FullCommand<Input> = CommandBase<Input> & {
  minimalContext?: false;
  run(
    input: Input,
    context: AppContext,
    execution: CommandExecution,
  ): Promise<CommandResult> | CommandResult;
};

type MinimalCommand<Input> = CommandBase<Input> & {
  minimalContext: true;
  run(
    input: Input,
    context: MinimalAppContext,
    execution: CommandExecution,
  ): Promise<CommandResult> | CommandResult;
};

type LocalCommand<Input> = FullCommand<Input> | MinimalCommand<Input>;

export type Command = LocalCommand<unknown>;

export const globalArgs = {
  root: {
    type: "string",
    valueHint: "path",
    description: "LocalBase data directory",
  },
  "non-interactive": {
    type: "boolean",
    description: "Never open interactive prompts",
  },
  json: {
    type: "boolean",
    description: "Emit machine-readable JSON",
  },
} satisfies ArgsDef;

const modelKindArg = {
  type: "enum",
  options: ["llm", "stt", "image"],
  description: "Filter by model kind",
} satisfies ArgDef;

const noPromptBoolean = (description: string, negativeDescription: string) =>
  ({ type: "boolean", description, negativeDescription }) as const;

function assertCatalogModels(
  modelIds: string[] | undefined,
  kind: ModelKind,
): void {
  const invalid = modelIds?.filter((modelId) => byId(modelId)?.kind !== kind);
  if (invalid?.length) {
    throw new CliInputError(`Invalid ${kind} model ids: ${invalid.join(", ")}`);
  }
}

function command<Input>(
  definition: Omit<FullCommand<Input>, "citty">,
): FullCommand<Input>;
function command<Input>(
  definition: Omit<MinimalCommand<Input>, "citty">,
): MinimalCommand<Input>;
function command<Input>(
  definition:
    Omit<FullCommand<Input>, "citty"> | Omit<MinimalCommand<Input>, "citty">,
): LocalCommand<Input> {
  return {
    ...definition,
    citty: defineCommand({
      meta: {
        name: definition.path.at(-1),
        description: definition.description,
      },
      args: { ...globalArgs, ...definition.args },
    }),
  };
}

export const configureCommand = command<ConfigureInput>({
  path: ["configure"],
  description: "Configure models, ports, settings, and API keys",
  examples: [
    "local-base configure --all",
    "local-base --non-interactive configure --defaults --parallel auto",
  ],
  args: {
    all: { type: "boolean", description: "Prompt for every setting" },
    defaults: {
      type: "boolean",
      description: "Use saved or default settings without prompting",
    },
    config: {
      type: "string",
      valueHint: "file",
      description: "Load TOML configuration overrides",
    },
    host: { type: "string", valueHint: "host", description: "LLM host" },
    port: { type: "string", valueHint: "port", description: "LLM port" },
    "ctx-size": {
      type: "string",
      valueHint: "tokens",
      description: "LLM context limit ceiling",
    },
    parallel: {
      type: "string",
      valueHint: "auto|1-4",
      description: "Parallel request slots",
    },
    "otel-endpoint": {
      type: "string",
      valueHint: "url",
      description: "OTLP/HTTP base endpoint; empty disables export",
    },
    "otel-headers": {
      type: "string",
      valueHint: "key=value,...",
      description: "OTLP exporter headers",
    },
    "otel-sample-ratio": {
      type: "string",
      valueHint: "0-100",
      description: "Percentage of root traces to sample",
    },
    "stt-host": {
      type: "string",
      valueHint: "host",
      description: "STT host",
    },
    "stt-port": {
      type: "string",
      valueHint: "port",
      description: "STT port",
    },
    "llm-models": {
      type: "string",
      valueHint: "id,...",
      description: "Selected LLM model IDs",
    },
    "stt-models": {
      type: "string",
      valueHint: "id,...",
      description: "Selected STT model IDs; use an empty value to disable",
    },
    "image-models": {
      type: "string",
      valueHint: "id,...",
      description: "Selected image model IDs; use an empty value to disable",
    },
    "active-llm": {
      type: "string",
      valueHint: "id",
      description: "Active LLM model ID",
    },
    "active-stt": {
      type: "string",
      valueHint: "id",
      description: "Active STT model ID",
    },
    "active-image": {
      type: "string",
      valueHint: "id",
      description: "Active image model ID",
    },
    "hf-token": {
      type: "string",
      valueHint: "token",
      description: "Hugging Face token for gated downloads",
    },
    "create-key": noPromptBoolean(
      "Create an initial API key",
      "Do not create an initial API key",
    ),
  },
  parse: (input) =>
    configureInputSchema.parse({ ...input, configPath: input.config }),
  validate: (input, global) => {
    if (global.nonInteractive && input.all) {
      throw new CliInputError("--all cannot be used with --non-interactive");
    }
    assertCatalogModels(input.llmModels, "llm");
    assertCatalogModels(input.sttModels, "stt");
    assertCatalogModels(input.imageModels, "image");
    return input;
  },
  resultSchema: configureResultSchema,
  run: async (input, context, execution) => {
    const { runConfigure } = await import("../../config/commands/configure");
    return await runConfigure(input, context, execution);
  },
});

const initCommand = command<InitInput>({
  path: ["init"],
  description: "Initialize the LocalBase data directory",
  parse: (input) => initInputSchema.parse(input),
  resultSchema: initResultSchema,
  run: async (input, context, execution) => {
    const { runInit } = await import("../../config/commands/init");
    return runInit(input, context, execution);
  },
});

const doctorCommand = command<DoctorInput>({
  path: ["doctor"],
  description: "Run a system health check and print configuration details",
  requiresDatabase: false,
  readOnlyConfiguration: true,
  parse: (input) => doctorInputSchema.parse(input),
  resultSchema: doctorResultSchema,
  run: async (input, context, execution) => {
    const { runDoctor } = await import("../../system/commands/doctor");
    return runDoctor(input, context, execution);
  },
});

const catalogCommand = command<CatalogInput>({
  path: ["models", "catalog"],
  description: "List all supported models",
  requiresDatabase: false,
  args: { kind: modelKindArg },
  parse: (input) => catalogInputSchema.parse(input),
  resultSchema: catalogResultSchema,
  run: async (input, context, execution) => {
    const { runCatalog } = await import("../../models/commands/catalog");
    return runCatalog(input, context, execution);
  },
});

const recommendCommand = command<RecommendInput>({
  path: ["models", "recommend"],
  description: "Recommend models for available VRAM",
  requiresDatabase: false,
  args: {
    kind: modelKindArg,
    vram: {
      type: "string",
      valueHint: "GB",
      description: "Target VRAM in GB",
    },
  },
  parse: (input) => recommendInputSchema.parse(input),
  resultSchema: recommendResultSchema,
  run: async (input, context, execution) => {
    const { runRecommend } = await import("../../models/commands/recommend");
    return runRecommend(input, context, execution);
  },
});

const listCommand = command<InstalledInput>({
  path: ["models", "list"],
  description: "List installed models",
  args: { kind: modelKindArg },
  parse: (input) => installedInputSchema.parse(input),
  resultSchema: installedResultSchema,
  run: async (input, context, execution) => {
    const { runInstalled } = await import("../../models/commands/installed");
    return await runInstalled(input, context, execution);
  },
});

const installCommand = command<InstallInput>({
  path: ["models", "install"],
  description: "Download and install a model",
  examples: [
    "local-base models install qwen2.5-coder-7b-instruct-q4_k_m",
    "local-base --non-interactive models install --all",
  ],
  args: {
    modelId: {
      type: "positional",
      required: false,
      description: "Catalog model ID",
    },
    all: { type: "boolean", description: "Install all selected models" },
  },
  positionals: { maximum: 1 },
  parse: (input) => installInputSchema.parse(input),
  validate: (input) => {
    if (input.modelId && !byId(input.modelId)) {
      throw new CliInputError(`Unknown model id: ${input.modelId}`);
    }
    return input;
  },
  resultSchema: installResultSchema,
  run: async (input, context, execution) => {
    const { runInstall } = await import("../../models/commands/install");
    return await runInstall(input, context, execution);
  },
});

const serveCommand = command<ServeInput>({
  path: ["serve"],
  description: "Start the unified LocalBase API gateway",
  args: {
    host: { type: "string", valueHint: "host", description: "Gateway host" },
    port: { type: "string", valueHint: "port", description: "Gateway port" },
    llm: noPromptBoolean("Enable the LLM service", "Disable the LLM service"),
    stt: noPromptBoolean("Enable the STT service", "Disable the STT service"),
    image: noPromptBoolean(
      "Enable image generation",
      "Disable image generation",
    ),
    "llm-host": {
      type: "string",
      valueHint: "host",
      description: "llama-server host",
    },
    "llm-port": {
      type: "string",
      valueHint: "port",
      description: "llama-server port",
    },
    "stt-host": {
      type: "string",
      valueHint: "host",
      description: "whisper-server host",
    },
    "stt-port": {
      type: "string",
      valueHint: "port",
      description: "whisper-server port",
    },
    "image-host": {
      type: "string",
      valueHint: "host",
      description: "sd-server host",
    },
    "image-port": {
      type: "string",
      valueHint: "port",
      description: "sd-server port",
    },
    "ctx-size": {
      type: "string",
      valueHint: "tokens",
      description: "LLM context limit",
    },
    "stt-path": {
      type: "string",
      valueHint: "path",
      description: "Whisper endpoint path",
    },
    "llm-model-file": {
      type: "string",
      valueHint: "file",
      description: "LLM model filename override",
    },
    "stt-model-file": {
      type: "string",
      valueHint: "file",
      description: "STT model filename override",
    },
    "image-model-file": {
      type: "string",
      valueHint: "file",
      description: "Image model filename override",
    },
    auth: noPromptBoolean(
      "Enable API key authentication",
      "Disable API key authentication",
    ),
    "auth-mode": {
      type: "enum",
      options: ["bearer", "x-api-key", "either"],
      description: "Authentication header mode",
    },
    "bypass-memory-check": {
      type: "boolean",
      description: "Start despite model memory warnings",
    },
  },
  parse: (input) => serveInputSchema.parse(input),
  resultSchema: serveResultSchema,
  longRunning: true,
  initializeUnderOperationLock: true,
  run: async (input, context, execution) => {
    const { runServe } = await import("../../runtime/commands/serve");
    return await runServe(input, context, execution);
  },
});

const startCommand = command<ServiceInput>({
  path: ["start"],
  description: "Install, enable, and start the LocalBase user service",
  requiresDatabase: false,
  parse: (input) => serviceInputSchema.parse(input),
  resultSchema: serviceLifecycleResultSchema,
  run: async (input, context, execution) => {
    const { runStart } = await import("../../service/commands/lifecycle");
    return await runStart(input, context, execution);
  },
});

const stopCommand = command<ServiceInput>({
  path: ["stop"],
  description: "Stop and disable the LocalBase user service",
  requiresDatabase: false,
  parse: (input) => serviceInputSchema.parse(input),
  resultSchema: serviceLifecycleResultSchema,
  run: async (input, context, execution) => {
    const { runStop } = await import("../../service/commands/lifecycle");
    return await runStop(input, context, execution);
  },
});

const restartCommand = command<ServiceInput>({
  path: ["restart"],
  description: "Refresh, enable, and restart the LocalBase user service",
  requiresDatabase: false,
  parse: (input) => serviceInputSchema.parse(input),
  resultSchema: serviceLifecycleResultSchema,
  run: async (input, context, execution) => {
    const { runRestart } = await import("../../service/commands/lifecycle");
    return await runRestart(input, context, execution);
  },
});

const statusCommand = command<ServiceInput>({
  path: ["status"],
  description: "Show LocalBase service and gateway readiness",
  requiresDatabase: false,
  parse: (input) => serviceInputSchema.parse(input),
  resultSchema: serviceLifecycleResultSchema,
  run: async (input, context, execution) => {
    const { runStatus } = await import("../../service/commands/lifecycle");
    return await runStatus(input, context, execution);
  },
});

const logsCommand = command<LogsInput>({
  path: ["logs"],
  description: "Read structured LocalBase operational logs",
  examples: [
    "local-base logs --level error",
    "local-base logs --follow --runtime llm",
  ],
  args: {
    follow: { type: "boolean", description: "Continue streaming new events" },
    limit: {
      type: "string",
      valueHint: "count",
      description: "Maximum snapshot events (default 200, max 5000)",
    },
    since: {
      type: "string",
      valueHint: "ISO-8601",
      description: "Include events at or after this timestamp",
    },
    level: {
      type: "enum",
      options: ["debug", "info", "warn", "error"],
      description: "Filter by severity",
    },
    runtime: {
      type: "enum",
      options: ["gateway", "llm", "stt", "image", "service", "cli"],
      description: "Filter by runtime",
    },
    "request-id": {
      type: "string",
      valueHint: "id",
      description: "Filter by request ID",
    },
  },
  requiresDatabase: false,
  minimalContext: true,
  parse: (input) => logsInputSchema.parse(input),
  streaming: (input) => input.follow,
  resultSchema: logsResultSchema,
  run: async (input, context, execution) => {
    const { runLogs } = await import("../../observability/commands/logs");
    return await runLogs(input, context, execution);
  },
});

const diagnosticsCommand = command<DiagnosticsInput>({
  path: ["diagnostics"],
  description: "Create a redacted LocalBase diagnostics bundle",
  examples: [
    "local-base diagnostics",
    "local-base diagnostics --output report.zip",
  ],
  args: {
    output: {
      type: "string",
      valueHint: "path.zip",
      description: "Output ZIP path",
    },
  },
  requiresDatabase: false,
  minimalContext: true,
  parse: (input) => diagnosticsInputSchema.parse(input),
  resultSchema: diagnosticsResultSchema,
  run: async (input, context, execution) => {
    const { runDiagnostics } =
      await import("../../diagnostics/commands/diagnostics");
    return await runDiagnostics(input, context, execution);
  },
});

const keysListCommand = command<KeysListInput>({
  path: ["keys", "list"],
  description: "List API keys",
  parse: (input) => keysListInputSchema.parse(input),
  resultSchema: keysListResultSchema,
  run: async (input, context, execution) => {
    const { runKeysList } = await import("../../auth/commands/keys");
    return runKeysList(input, context, execution);
  },
});

const keysCreateCommand = command<KeysCreateInput>({
  path: ["keys", "create"],
  description: "Create an API key",
  args: {
    name: { type: "string", valueHint: "label", description: "Key label" },
    "expires-days": {
      type: "string",
      valueHint: "days",
      description: "Expiration period",
    },
  },
  parse: (input) => keysCreateInputSchema.parse(input),
  resultSchema: keySecretResultSchema,
  run: async (input, context, execution) => {
    const { runKeysCreate } = await import("../../auth/commands/keys");
    return runKeysCreate(input, context, execution);
  },
});

const keysRevokeCommand = command<KeyIdInput>({
  path: ["keys", "revoke"],
  description: "Revoke an API key",
  args: {
    keyId: { type: "positional", description: "API key ID", required: true },
  },
  positionals: { minimum: 1, maximum: 1 },
  parse: (input) => keyIdInputSchema.parse(input),
  resultSchema: keyRevocationResultSchema,
  run: async (input, context, execution) => {
    const { runKeysRevoke } = await import("../../auth/commands/keys");
    return runKeysRevoke(input, context, execution);
  },
});

const keysRotateCommand = command<KeyIdInput>({
  path: ["keys", "rotate"],
  description: "Rotate an API key",
  args: {
    keyId: { type: "positional", description: "API key ID", required: true },
  },
  positionals: { minimum: 1, maximum: 1 },
  parse: (input) => keyIdInputSchema.parse(input),
  resultSchema: keySecretResultSchema,
  run: async (input, context, execution) => {
    const { runKeysRotate } = await import("../../auth/commands/keys");
    return runKeysRotate(input, context, execution);
  },
});

const resetCommand = command<ResetInput>({
  path: ["reset"],
  description: "Reset the LocalBase configuration database",
  args: {
    yes: { type: "boolean", alias: "y", description: "Confirm reset" },
  },
  requiresDatabase: false,
  parse: (input) => resetInputSchema.parse(input),
  resultSchema: resetResultSchema,
  run: async (input, context, execution) => {
    const { runReset } = await import("../../maintenance/commands/reset");
    return await runReset(input, context, execution);
  },
});

const uninstallCommand = command<UninstallInput>({
  path: ["uninstall"],
  description: "Remove all LocalBase-managed data",
  args: {
    yes: { type: "boolean", alias: "y", description: "Confirm uninstall" },
  },
  requiresDatabase: false,
  parse: (input) => uninstallInputSchema.parse(input),
  resultSchema: uninstallResultSchema,
  run: async (input, context, execution) => {
    const { runUninstall } =
      await import("../../maintenance/commands/uninstall");
    return runUninstall(input, context, execution);
  },
});

export const commands = [
  configureCommand,
  initCommand,
  doctorCommand,
  catalogCommand,
  recommendCommand,
  listCommand,
  installCommand,
  serveCommand,
  startCommand,
  stopCommand,
  restartCommand,
  statusCommand,
  logsCommand,
  diagnosticsCommand,
  keysListCommand,
  keysCreateCommand,
  keysRevokeCommand,
  keysRotateCommand,
  resetCommand,
  uninstallCommand,
] as const satisfies readonly Command[];

export const modelsCommand = defineCommand({
  meta: { name: "local-base models", description: "Browse and install models" },
  args: globalArgs,
  subCommands: {
    catalog: catalogCommand.citty,
    recommend: recommendCommand.citty,
    list: listCommand.citty,
    install: installCommand.citty,
  },
});

export const keysCommand = defineCommand({
  meta: { name: "local-base keys", description: "Manage API keys" },
  args: globalArgs,
  subCommands: {
    list: keysListCommand.citty,
    create: keysCreateCommand.citty,
    revoke: keysRevokeCommand.citty,
    rotate: keysRotateCommand.citty,
  },
});

export const rootCommand = defineCommand({
  meta: {
    name: "local-base",
    version: "0.1.0",
    description: "Local AI installer, manager, and OpenAI-compatible gateway",
  },
  args: globalArgs,
  subCommands: {
    init: initCommand.citty,
    configure: configureCommand.citty,
    doctor: doctorCommand.citty,
    models: modelsCommand,
    serve: serveCommand.citty,
    start: startCommand.citty,
    stop: stopCommand.citty,
    restart: restartCommand.citty,
    status: statusCommand.citty,
    logs: logsCommand.citty,
    diagnostics: diagnosticsCommand.citty,
    keys: keysCommand,
    reset: resetCommand.citty,
    uninstall: uninstallCommand.citty,
  },
});

export function groupForPath(path: string[]): CittyCommand | undefined {
  if (path.length !== 1) return undefined;
  if (path[0] === "models") return modelsCommand;
  if (path[0] === "keys") return keysCommand;
  return undefined;
}
