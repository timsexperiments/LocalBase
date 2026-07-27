import { detectSpecs, type HostSpecs } from "./system";
import {
  defaultConfig,
  defaultRoot,
  loadConfig,
  type LocalBaseConfig,
} from "./manager";
import { createLogger, type ILogger } from "./utils/logger";
import { DatabaseSession } from "./db/client";
import { z } from "zod";
import {
  dataRootSchema,
  globalOptionsSchema,
  hostSchema,
  type GlobalOptions,
} from "./domains/app/commands/inputs";
import { CliInputError, formatZodError } from "./domains/app/commands/errors";
import { canonicalLocalBaseRoot } from "./utils/root";
import {
  acquireServeInitializationLease,
  type OperationLease,
} from "./domains/service/ownership";

/**
 * Dependency Injection (DI) Container for LocalBase application context.
 */
export interface AppContext {
  logger: ILogger;
  specs: HostSpecs;
  config: LocalBaseConfig;
  database: DatabaseSession;
  initializationOperation?: OperationLease;
}

const environmentOverridesSchema = z
  .object({
    root: dataRootSchema.optional(),
    host: hostSchema.optional(),
    port: z
      .string()
      .regex(/^\d+$/, "LOCALBASE_PORT must be an integer")
      .transform(Number)
      .pipe(z.number().int().min(1).max(65_535))
      .optional(),
    sttHost: hostSchema.optional(),
    sttPort: z
      .string()
      .regex(/^\d+$/, "LOCALBASE_STT_PORT must be an integer")
      .transform(Number)
      .pipe(z.number().int().min(1).max(65_535))
      .optional(),
    ctxSize: z
      .string()
      .regex(/^\d+$/, "LOCALBASE_CTX_SIZE must be an integer")
      .transform(Number)
      .pipe(z.number().int().min(1))
      .optional(),
  })
  .strict();

const environmentVariableNames: Record<string, string> = {
  root: "LOCALBASE_ROOT",
  host: "LOCALBASE_HOST",
  port: "LOCALBASE_PORT",
  sttHost: "LOCALBASE_STT_HOST",
  sttPort: "LOCALBASE_STT_PORT",
  ctxSize: "LOCALBASE_CTX_SIZE",
};

export type EnvironmentOverrides = z.infer<typeof environmentOverridesSchema>;

export function parseEnvironmentOverrides(
  environment: Record<string, string | undefined>,
): EnvironmentOverrides {
  const result = environmentOverridesSchema.safeParse({
    root: environment.LOCALBASE_ROOT,
    host: environment.LOCALBASE_HOST,
    port: environment.LOCALBASE_PORT,
    sttHost: environment.LOCALBASE_STT_HOST,
    sttPort: environment.LOCALBASE_STT_PORT,
    ctxSize: environment.LOCALBASE_CTX_SIZE,
  });
  if (result.success) return result.data;
  throw new CliInputError(
    formatZodError(
      result.error,
      (path) => environmentVariableNames[String(path[0])] ?? "environment",
    ),
  );
}

export function resolveEffectiveRoot(
  cliRoot: string | undefined,
  environmentRoot: string | undefined,
  configuredRoot?: string,
): string {
  return cliRoot ?? environmentRoot ?? configuredRoot ?? defaultRoot();
}

/**
 * Bootstraps and configures the Dependency Injection container.
 * Applies environment variable configuration overrides on top of SQLite-stored config.
 */
export async function createAppContext(
  options: GlobalOptions,
  initializeDatabase = true,
  initializeUnderOperationLock = false,
): Promise<AppContext> {
  const parsedOptions = globalOptionsSchema.parse(options);
  const overrides = parseEnvironmentOverrides(process.env);
  const database = new DatabaseSession();
  let initializationOperation: OperationLease | undefined;
  try {
    const specs = await detectSpecs();
    const root = canonicalLocalBaseRoot(
      resolveEffectiveRoot(parsedOptions.root, overrides.root),
    );
    if (initializeUnderOperationLock) {
      initializationOperation = await acquireServeInitializationLease(
        root,
        process.env.LOCALBASE_SERVICE_TOKEN,
      );
    }
    const config: LocalBaseConfig = initializeDatabase
      ? loadConfig(database, root, specs.gpuVramGb)
      : defaultConfig(root, specs.gpuVramGb);

    if (overrides.host) config.host = overrides.host;
    if (overrides.port) config.port = overrides.port;
    if (overrides.sttHost) config.sttHost = overrides.sttHost;
    if (overrides.sttPort) config.sttPort = overrides.sttPort;
    if (overrides.ctxSize) config.ctxSize = overrides.ctxSize;

    const logger = createLogger(process.env.LOG_FORMAT);
    return {
      logger,
      specs,
      config,
      database,
      ...(initializationOperation ? { initializationOperation } : {}),
    };
  } catch (error) {
    await initializationOperation?.release();
    database.close();
    throw error;
  }
}
