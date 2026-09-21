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
  accessCloudflareInputSchema,
  accessDisableInputSchema,
  accessOidcInputSchema,
  accessPolicyApplyInputSchema,
  accessPolicyClearInputSchema,
  accessPolicyShowInputSchema,
  accessPolicyTestInputSchema,
  accessShowInputSchema,
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
  keysScopesInputSchema,
  logsInputSchema,
  recommendInputSchema,
  resetInputSchema,
  serviceInputSchema,
  serveInputSchema,
  uninstallInputSchema,
  type AccessCloudflareInput,
  type AccessDisableInput,
  type AccessOidcInput,
  type AccessPolicyApplyInput,
  type AccessPolicyClearInput,
  type AccessPolicyShowInput,
  type AccessPolicyTestInput,
  type AccessShowInput,
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
  type KeysScopesInput,
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
  accessConfigureResultSchema,
  accessDisableResultSchema,
  accessPolicyApplyResultSchema,
  accessPolicyClearResultSchema,
  accessPolicyShowResultSchema,
  accessPolicyTestResultSchema,
  accessShowResultSchema,
  catalogResultSchema,
  configureResultSchema,
  diagnosticsResultSchema,
  doctorResultSchema,
  initResultSchema,
  installedResultSchema,
  installResultSchema,
  keyMetadataResultSchema,
  keySecretResultSchema,
  keysListResultSchema,
  logsResultSchema,
  recommendResultSchema,
  resetResultSchema,
  serviceLifecycleResultSchema,
  serveResultSchema,
  uninstallResultSchema,
} from "./results";

import {
  configurationPlanSchema,
  desiredConfigurationSchema,
} from "../../config/declarative";
import { configApplyResultSchema } from "../../config/apply";
import {
  configFileInputSchema,
  configPlanInputSchema,
  configApplyInputSchema,
  runConfigValidate,
  runConfigPlan,
  runConfigApply,
  runConfigShow,
} from "../../config/commands/config";

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
  inputErrorExitCode?: number;
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
  options: ["llm", "stt", "tts", "image", "video"],
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
    "tts-models": {
      type: "string",
      valueHint: "id,...",
      description: "Selected TTS model IDs; use an empty value to disable",
    },
    "image-models": {
      type: "string",
      valueHint: "id,...",
      description: "Selected image model IDs; use an empty value to disable",
    },
    "video-models": {
      type: "string",
      valueHint: "id,...",
      description: "Selected video model IDs; use an empty value to disable",
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
    "active-tts": {
      type: "string",
      valueHint: "id",
      description: "Active TTS model ID",
    },
    "active-image": {
      type: "string",
      valueHint: "id",
      description: "Active image model ID",
    },
    "active-video": {
      type: "string",
      valueHint: "id",
      description: "Active video model ID",
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
    assertCatalogModels(input.ttsModels, "tts");
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
    tts: noPromptBoolean(
      "Enable speech generation",
      "Disable speech generation",
    ),
    image: noPromptBoolean(
      "Enable image generation",
      "Disable image generation",
    ),
    video: noPromptBoolean(
      "Enable video generation runtime",
      "Disable video generation runtime",
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
    "video-host": {
      type: "string",
      valueHint: "host",
      description: "video sd-server host",
    },
    "video-port": {
      type: "string",
      valueHint: "port",
      description: "video sd-server port",
    },
    "ctx-size": {
      type: "string",
      valueHint: "tokens",
      description: "LLM context limit",
    },
    "inference-queue-capacity": {
      type: "string",
      valueHint: "requests",
      description: "Maximum waiting requests per inference type",
    },
    "inference-queue-timeout-ms": {
      type: "string",
      valueHint: "milliseconds",
      description: "Maximum inference queue wait",
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
    "tts-model-file": {
      type: "string",
      valueHint: "file",
      description: "TTS backbone model filename override",
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
      options: ["gateway", "llm", "stt", "tts", "image", "service", "cli"],
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
    scopes: {
      type: "string",
      valueHint: "permission,...",
      description:
        "Comma-separated permissions; defaults to inference and models:read. Empty grants none",
    },
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
  resultSchema: keyMetadataResultSchema,
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

const keysScopesCommand = command<KeysScopesInput>({
  path: ["keys", "scopes"],
  description: "Replace an API key's scopes",
  args: {
    keyId: { type: "positional", description: "API key ID", required: true },
    scopes: {
      type: "string",
      valueHint: "permission,...",
      description: "Complete comma-separated permission set; empty grants none",
      required: true,
    },
  },
  positionals: { minimum: 1, maximum: 1 },
  parse: (input) => keysScopesInputSchema.parse(input),
  resultSchema: keyMetadataResultSchema,
  run: async (input, context, execution) => {
    const { runKeysScopes } = await import("../../auth/commands/keys");
    return runKeysScopes(input, context, execution);
  },
});

const accessShowCommand = command<AccessShowInput>({
  path: ["access", "show"],
  description: "Show browser identity provider configuration",
  parse: (input) => accessShowInputSchema.parse(input),
  resultSchema: accessShowResultSchema,
  run: async (input, context, execution) => {
    const { runAccessShow } = await import("../../auth/commands/access");
    return await runAccessShow(input, context, execution);
  },
});

const accessCloudflareCommand = command<AccessCloudflareInput>({
  path: ["access", "cloudflare"],
  description: "Configure Cloudflare Access browser authentication",
  args: {
    "team-domain": {
      type: "string",
      valueHint: "team.cloudflareaccess.com",
      description: "Cloudflare Access team domain",
      required: true,
    },
    audience: {
      type: "string",
      valueHint: "AUD tag",
      description: "Cloudflare Access application audience",
      required: true,
    },
    origin: {
      type: "string",
      valueHint: "https://localbase.example.com",
      description: "Exact public UI origin",
      required: true,
    },
    permissions: {
      type: "string",
      valueHint: "permission,...",
      description:
        "Browser permissions; defaults to inference and model management",
    },
  },
  parse: (input) => accessCloudflareInputSchema.parse(input),
  resultSchema: accessConfigureResultSchema,
  run: async (input, context, execution) => {
    const { runAccessCloudflare } = await import("../../auth/commands/access");
    return await runAccessCloudflare(input, context, execution);
  },
});

const accessOidcCommand = command<AccessOidcInput>({
  path: ["access", "oidc"],
  description: "Configure OpenID Connect browser authentication",
  args: {
    issuer: {
      type: "string",
      valueHint: "https://identity.example.com",
      description: "Exact OpenID Connect issuer",
      required: true,
    },
    "client-id": {
      type: "string",
      valueHint: "CLIENT_ID",
      description: "OpenID Connect client ID",
      required: true,
    },
    "client-secret-env": {
      type: "string",
      valueHint: "ENVIRONMENT_VARIABLE",
      description: "Environment variable containing the client secret",
    },
    "public-client": {
      type: "boolean",
      description: "Configure an OpenID Connect public client",
    },
    origin: {
      type: "string",
      valueHint: "https://localbase.example.com",
      description: "Exact public UI origin",
      required: true,
    },
    permissions: {
      type: "string",
      valueHint: "permission,...",
      description:
        "Browser permissions; defaults to inference and model management",
    },
  },
  parse: (input) => accessOidcInputSchema.parse(input),
  resultSchema: accessConfigureResultSchema,
  run: async (input, context, execution) => {
    const { runAccessOidc } = await import("../../auth/commands/access");
    return await runAccessOidc(input, context, execution);
  },
});

const accessDisableCommand = command<AccessDisableInput>({
  path: ["access", "disable"],
  description: "Disable browser authentication",
  parse: (input) => accessDisableInputSchema.parse(input),
  resultSchema: accessDisableResultSchema,
  run: async (input, context, execution) => {
    const { runAccessDisable } = await import("../../auth/commands/access");
    return await runAccessDisable(input, context, execution);
  },
});

const accessPolicyShowCommand = command<AccessPolicyShowInput>({
  path: ["access", "policy", "show"],
  description: "Show the browser authorization policy",
  parse: (input) => accessPolicyShowInputSchema.parse(input),
  resultSchema: accessPolicyShowResultSchema,
  run: async (input, context, execution) => {
    const { runAccessPolicyShow } = await import("../../auth/commands/access");
    return await runAccessPolicyShow(input, context, execution);
  },
});

const accessPolicyApplyCommand = command<AccessPolicyApplyInput>({
  path: ["access", "policy", "apply"],
  description: "Atomically apply a browser authorization policy",
  args: {
    file: {
      type: "string",
      valueHint: "policy.json",
      description: "Complete policy JSON file",
      required: true,
    },
  },
  parse: (input) => accessPolicyApplyInputSchema.parse(input),
  resultSchema: accessPolicyApplyResultSchema,
  run: async (input, context, execution) => {
    const { runAccessPolicyApply } = await import("../../auth/commands/access");
    return await runAccessPolicyApply(input, context, execution);
  },
});

const accessPolicyTestCommand = command<AccessPolicyTestInput>({
  path: ["access", "policy", "test"],
  description: "Evaluate a browser identity against the local policy",
  args: {
    issuer: {
      type: "string",
      valueHint: "https://identity.example.com",
      description: "Verified identity issuer",
      required: true,
    },
    subject: {
      type: "string",
      valueHint: "subject",
      description: "Exact verified subject",
      required: true,
    },
    email: {
      type: "string",
      valueHint: "person@example.com",
      description: "Verified email address",
    },
  },
  parse: (input) => accessPolicyTestInputSchema.parse(input),
  resultSchema: accessPolicyTestResultSchema,
  run: async (input, context, execution) => {
    const { runAccessPolicyTest } = await import("../../auth/commands/access");
    return await runAccessPolicyTest(input, context, execution);
  },
});

const accessPolicyClearCommand = command<AccessPolicyClearInput>({
  path: ["access", "policy", "clear"],
  description: "Clear the local policy and use provider-wide permissions",
  parse: (input) => accessPolicyClearInputSchema.parse(input),
  resultSchema: accessPolicyClearResultSchema,
  run: async (input, context, execution) => {
    const { runAccessPolicyClear } = await import("../../auth/commands/access");
    return await runAccessPolicyClear(input, context, execution);
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

const configFileArg = {
  type: "string",
  valueHint: "path|-",
  description: "Versioned TOML file, or - for stdin",
} satisfies ArgDef;
const configValidateCommand = command({
  path: ["config", "validate"],
  description: "Validate a desired configuration without changing state",
  minimalContext: true,
  inputErrorExitCode: 1,
  args: { file: configFileArg },
  parse: (input) => configFileInputSchema.parse(input),
  resultSchema: z
    .object({
      valid: z.literal(true),
      configuration: desiredConfigurationSchema,
    })
    .strict(),
  run: runConfigValidate,
});
const configPlanCommand = command({
  path: ["config", "plan"],
  description: "Compare desired configuration with persisted settings",
  minimalContext: true,
  inputErrorExitCode: 1,
  args: {
    file: configFileArg,
    "detailed-exit-code": {
      type: "boolean",
      description: "Exit 2 for changes, 0 for no changes, 1 for errors",
    },
  },
  parse: (input) => configPlanInputSchema.parse(input),
  resultSchema: configurationPlanSchema,
  run: runConfigPlan,
});
const configApplyCommand = command({
  path: ["config", "apply"],
  description: "Atomically apply desired configuration",
  minimalContext: true,
  inputErrorExitCode: 1,
  args: {
    file: configFileArg,
    restart: {
      type: "string",
      valueHint: "auto|always|never",
      description: "Restart policy (default: auto)",
    },
    wait: {
      type: "boolean",
      description: "Wait up to 30 seconds for gateway readiness",
    },
  },
  parse: (input) => configApplyInputSchema.parse(input),
  resultSchema: configApplyResultSchema,
  run: runConfigApply,
});
const configShowCommand = command({
  path: ["config", "show"],
  description: "Emit a re-applicable TOML document with secrets omitted",
  minimalContext: true,
  inputErrorExitCode: 1,
  parse: (input) => z.object({}).strict().parse(input),
  resultSchema: z
    .object({ document: z.string(), pendingRestart: z.boolean() })
    .strict(),
  run: runConfigShow,
});

export const commands = [
  configValidateCommand,
  configPlanCommand,
  configApplyCommand,
  configShowCommand,
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
  keysScopesCommand,
  accessShowCommand,
  accessCloudflareCommand,
  accessOidcCommand,
  accessDisableCommand,
  accessPolicyShowCommand,
  accessPolicyApplyCommand,
  accessPolicyTestCommand,
  accessPolicyClearCommand,
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
    scopes: keysScopesCommand.citty,
  },
});

const accessPolicyCommand = defineCommand({
  meta: {
    name: "local-base access policy",
    description: "Manage browser authorization policy",
  },
  args: globalArgs,
  subCommands: {
    show: accessPolicyShowCommand.citty,
    apply: accessPolicyApplyCommand.citty,
    test: accessPolicyTestCommand.citty,
    clear: accessPolicyClearCommand.citty,
  },
});

export const accessCommand = defineCommand({
  meta: { name: "local-base access", description: "Manage browser access" },
  args: globalArgs,
  subCommands: {
    show: accessShowCommand.citty,
    cloudflare: accessCloudflareCommand.citty,
    oidc: accessOidcCommand.citty,
    disable: accessDisableCommand.citty,
    policy: accessPolicyCommand,
  },
});

const configCommand = defineCommand({
  meta: {
    name: "local-base config",
    description: "Manage declarative configuration",
  },
  args: globalArgs,
  subCommands: {
    validate: configValidateCommand.citty,
    plan: configPlanCommand.citty,
    apply: configApplyCommand.citty,
    show: configShowCommand.citty,
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
    config: configCommand,
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
    access: accessCommand,
    reset: resetCommand.citty,
    uninstall: uninstallCommand.citty,
  },
});

export function groupForPath(path: string[]): CittyCommand | undefined {
  if (path.length === 2 && path[0] === "access" && path[1] === "policy")
    return accessPolicyCommand;
  if (path.length !== 1) return undefined;
  if (path[0] === "models") return modelsCommand;
  if (path[0] === "keys") return keysCommand;
  if (path[0] === "access") return accessCommand;
  if (path[0] === "config") return configCommand;
  return undefined;
}
