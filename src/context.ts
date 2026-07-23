import { detectSpecs, type HostSpecs } from "./system";
import {
  defaultConfig,
  defaultRoot,
  loadConfig,
  type LocalBaseConfig,
} from "./manager";
import { parseFlag, toInt } from "./utils/args";
import { createLogger, type ILogger } from "./utils/logger";
import { DatabaseSession } from "./db/client";

/**
 * Dependency Injection (DI) Container for LocalBase application context.
 */
export interface AppContext {
  logger: ILogger;
  specs: HostSpecs;
  config: LocalBaseConfig;
  database: DatabaseSession;
}

/**
 * Bootstraps and configures the Dependency Injection container.
 * Applies environment variable configuration overrides on top of SQLite-stored config.
 */
export async function createAppContext(
  args: string[],
  initializeDatabase = true,
): Promise<AppContext> {
  const database = new DatabaseSession();
  const specs = await detectSpecs();
  const root = process.env.LOCALBASE_ROOT ?? parseFlag(args, "--root");
  let config: LocalBaseConfig;
  try {
    config = initializeDatabase
      ? loadConfig(database, root, specs.gpuVramGb)
      : defaultConfig(root ?? defaultRoot(), specs.gpuVramGb);
  } catch (error) {
    database.close();
    throw error;
  }

  // Server environment configuration overrides
  if (process.env.LOCALBASE_HOST) config.host = process.env.LOCALBASE_HOST;
  if (process.env.LOCALBASE_PORT)
    config.port = toInt(process.env.LOCALBASE_PORT, config.port);

  if (process.env.LOCALBASE_STT_HOST)
    config.sttHost = process.env.LOCALBASE_STT_HOST;
  if (process.env.LOCALBASE_STT_PORT)
    config.sttPort = toInt(process.env.LOCALBASE_STT_PORT, config.sttPort);

  if (process.env.LOCALBASE_CTX_SIZE)
    config.ctxSize = toInt(process.env.LOCALBASE_CTX_SIZE, config.ctxSize);

  const logFormat = process.env.LOG_FORMAT;
  const logger = createLogger(logFormat);

  return { logger, specs, config, database };
}
