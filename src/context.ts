import { detectSpecs, type HostSpecs } from "./system";
import { loadConfig, type LocalBaseConfig } from "./manager";
import { parseFlag, toInt } from "./utils/args";
import { createLogger, type ILogger } from "./utils/logger";

/**
 * Dependency Injection (DI) Container for LocalBase application context.
 */
export interface AppContext {
  logger: ILogger;
  specs: HostSpecs;
  config: LocalBaseConfig;
}

/**
 * Bootstraps and configures the Dependency Injection container.
 * Applies environment variable configuration overrides on top of SQLite-stored config.
 */
export async function createAppContext(args: string[]): Promise<AppContext> {
  const specs = await detectSpecs();
  const root = process.env.LOCALBASE_ROOT ?? parseFlag(args, "--root");
  const config = loadConfig(root, specs.gpuVramGb);

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

  return { logger, specs, config };
}
