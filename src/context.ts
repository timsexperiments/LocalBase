import { detectSpecs, type HostSpecs } from "./system";
import { loadConfig, type LocalBaseConfig } from "./manager";
import { parseFlag, toInt } from "./utils/args";
import { createLogger, type ILogger } from "./utils/logger";

export interface AppContext {
  logger: ILogger;
  specs: HostSpecs;
  config: LocalBaseConfig;
}

export function createAppContext(args: string[]): AppContext {
  const specs = detectSpecs();
  const root = process.env.LOCALBASE_ROOT ?? parseFlag(args, "--root");
  const config = loadConfig(root, specs.gpuVramGb);

  // Apply environment configuration overrides
  if (process.env.LOCALBASE_HOST) config.host = process.env.LOCALBASE_HOST;
  if (process.env.LOCALBASE_PORT) config.port = toInt(process.env.LOCALBASE_PORT, config.port);
  
  if (process.env.LOCALBASE_STT_HOST) config.sttHost = process.env.LOCALBASE_STT_HOST;
  if (process.env.LOCALBASE_STT_PORT) config.sttPort = toInt(process.env.LOCALBASE_STT_PORT, config.sttPort);
  
  if (process.env.LOCALBASE_CTX_SIZE) config.ctxSize = toInt(process.env.LOCALBASE_CTX_SIZE, config.ctxSize);

  const logFormat = process.env.LOG_FORMAT;
  const logger = createLogger(logFormat);

  return { logger, specs, config };
}
