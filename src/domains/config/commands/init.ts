import { initConfig } from "../../../manager";
import { detectSpecs } from "../../../system";
import { parseFlag } from "../../../utils/args";

export function runInit(args: string[]): number {
  const specs = detectSpecs();
  const root = parseFlag(args, "--root");
  const config = initConfig(root, specs.gpuVramGb);
  console.log(`Initialized local-base at ${config.root}`);
  console.log(`LLM directory: ${config.llmModelsDir}`);
  console.log(`STT directory: ${config.sttModelsDir}`);
  return 0;
}
