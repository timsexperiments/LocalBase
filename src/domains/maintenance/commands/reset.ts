import { resetDatabase } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { ResetInput } from "../../app/commands/inputs";

export async function runReset(
  input: ResetInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<number> {
  if (!input.yes) {
    execution.output.error(
      "reset is destructive. Re-run with --yes to confirm.",
    );
    return 2;
  }
  const fresh = await resetDatabase(
    ctx.database,
    ctx.config.root,
    ctx.specs.gpuVramGb,
  );
  execution.output.info(`Database reset complete at ${fresh.root}`);
  execution.output.info("Initialized default configuration.");
  return 0;
}
