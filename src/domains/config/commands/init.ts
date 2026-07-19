import { initConfig } from "../../../manager";
import { parseFlag } from "../../../utils/args";
import type { AppContext } from "../../../context";

export function runInit(args: string[], ctx: AppContext): number {
  const root = parseFlag(args, "--root");
  const config = initConfig(root, ctx.specs.gpuVramGb);
  console.log(`Initialized local-base at ${config.root}`);
  console.log(`LLM directory: ${config.llmModelsDir}`);
  console.log(`STT directory: ${config.sttModelsDir}`);
  return 0;
}
