import { installedModels } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { InstalledInput } from "../../app/commands/inputs";

export async function runInstalled(
  input: InstalledInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<number> {
  const found = await installedModels(ctx.config, input.kind);
  if (found.length === 0) {
    execution.output.info(
      input.kind
        ? `No installed ${input.kind.toUpperCase()} models found.`
        : "No installed models found.",
    );
    return 0;
  }
  for (const file of found) execution.output.info(file);
  return 0;
}
