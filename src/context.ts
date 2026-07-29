import { detectSpecs, type HostSpecs } from "./system";
import {
  defaultConfig,
  defaultRoot,
  loadConfig,
  type LocalBaseConfig,
} from "./manager";
import {
  clearBootstrapDiagnostic,
  createLogger,
  writeBootstrapDiagnostic,
  type ILogger,
} from "./domains/observability/logging";
import { DatabaseSession } from "./db/client";
import { z } from "zod";
import {
  dataRootSchema,
  globalOptionsSchema,
  hostSchema,
  type GlobalOptions,
} from "./domains/app/commands/inputs";
import { CliInputError, formatZodError } from "./domains/app/commands/errors";
import {
  assertInitializedLocalBaseRoot,
  canonicalLocalBaseRoot,
} from "./utils/root";
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

export interface MinimalAppContext {
  logger: ILogger;
  config: Pick<LocalBaseConfig, "root">;
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

export function createMinimalAppContext(
  options: GlobalOptions,
): MinimalAppContext {
  const parsedOptions = globalOptionsSchema.parse(options);
  const environmentRoot = dataRootSchema
    .optional()
    .parse(process.env.LOCALBASE_ROOT);
  const root = canonicalLocalBaseRoot(
    resolveEffectiveRoot(parsedOptions.root, environmentRoot),
  );
  return {
    logger: createLogger(process.env.LOG_FORMAT),
    config: { root },
  };
}

/**
 * Bootstraps and configures the Dependency Injection container.
 * Applies environment variable configuration overrides on top of SQLite-stored config.
 */
export async function createAppContext(
  options: GlobalOptions,
  initializeDatabase = true,
  initializeUnderOperationLock = false,
  environment: Record<string, string | undefined> = process.env,
): Promise<AppContext> {
  const parsedOptions = globalOptionsSchema.parse(options);
  const environmentRoot = dataRootSchema
    .optional()
    .parse(environment.LOCALBASE_ROOT);
  const root = canonicalLocalBaseRoot(
    resolveEffectiveRoot(parsedOptions.root, environmentRoot),
  );
  const database = new DatabaseSession();
  const logger = createLogger(environment.LOG_FORMAT);
  let initializationOperation: OperationLease | undefined;
  let bootstrapWritten = false;
  try {
    if (initializeUnderOperationLock) {
      initializationOperation = await acquireServeInitializationLease(
        root,
        environment.LOCALBASE_SERVICE_TOKEN,
      );
      if (await Bun.file(`${root}/.localbase-root.json`).exists()) {
        assertInitializedLocalBaseRoot(root);
        try {
          await logger.enableFileLogging(root);
          await clearBootstrapDiagnostic(root);
        } catch (error) {
          if (environment.LOCALBASE_SERVICE_TOKEN) {
            try {
              await writeBootstrapDiagnostic(root, error);
              bootstrapWritten = true;
            } catch {
              // Root validation and no-follow checks take precedence over diagnostics.
            }
          }
          throw error;
        }
      }
    }
    const overrides = parseEnvironmentOverrides(environment);
    const specs = await detectSpecs();
    const config: LocalBaseConfig = initializeDatabase
      ? loadConfig(database, root, specs.gpuVramGb)
      : defaultConfig(root, specs.gpuVramGb);
    if (initializeUnderOperationLock) {
      await logger.enableFileLogging(root);
      await clearBootstrapDiagnostic(root);
    }

    if (overrides.host) config.host = overrides.host;
    if (overrides.port) config.port = overrides.port;
    if (overrides.sttHost) config.sttHost = overrides.sttHost;
    if (overrides.sttPort) config.sttPort = overrides.sttPort;
    if (overrides.ctxSize) config.ctxSize = overrides.ctxSize;

    return {
      logger,
      specs,
      config,
      database,
      ...(initializationOperation ? { initializationOperation } : {}),
    };
  } catch (error) {
    if (initializeUnderOperationLock) {
      if (environment.LOCALBASE_SERVICE_TOKEN && !bootstrapWritten) {
        await writeBootstrapDiagnostic(root, error).catch(() => {});
      }
      logger.event({
        severity: "error",
        eventName: "gateway.initialization-failed",
        category: "gateway",
        component: "gateway",
        runtime: "gateway",
        message: "Gateway initialization failed.",
        error: {
          type: error instanceof Error ? error.name : "Error",
          message: error instanceof Error ? error.message : String(error),
        },
      });
      await logger.close();
    }
    await initializationOperation?.release();
    database.close();
    throw error;
  }
}
