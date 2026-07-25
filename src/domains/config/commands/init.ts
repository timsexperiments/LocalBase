import { initConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { InitInput } from "../../app/commands/inputs";
import { publicConfiguration } from "../../app/commands/results";

export function runInit(
  _input: InitInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { configuration: ReturnType<typeof publicConfiguration> } } {
  const config = initConfig(ctx.database, ctx.config.root, ctx.specs.gpuVramGb);
  execution.output.info(`Initialized local-base at ${config.root}`);
  execution.output.info(`LLM directory: ${config.llmModelsDir}`);
  execution.output.info(`STT directory: ${config.sttModelsDir}`);
  return { data: { configuration: publicConfiguration(config) } };
}
