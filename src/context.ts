import { detectSpecs, type HostSpecs } from "./system";
import { loadConfig, type LocalBaseConfig } from "./manager";
import { parseFlag } from "./utils/args";

export interface AppContext {
  specs: HostSpecs;
  config: LocalBaseConfig;
}

export function createAppContext(args: string[]): AppContext {
  const specs = detectSpecs();
  const root = parseFlag(args, "--root");
  const config = loadConfig(root, specs.gpuVramGb);
  return { specs, config };
}
