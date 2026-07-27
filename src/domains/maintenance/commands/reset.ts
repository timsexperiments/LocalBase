import { resetDatabase } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { ResetInput } from "../../app/commands/inputs";
import { CliInputError } from "../../app/commands/errors";
import {
  stopServiceWithinOperation,
  withServiceRootOperation,
} from "../../service/manager";
import { assertDestructiveLocalBaseRoot } from "../../../utils/root";

export async function runReset(
  input: ResetInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { reset: true; root: string } }> {
  if (!input.yes) {
    throw new CliInputError(
      "reset is destructive. Re-run with --yes to confirm.",
    );
  }
  assertDestructiveLocalBaseRoot(ctx.config.root);
  const fresh = await withServiceRootOperation(
    ctx.config.root,
    "reset",
    async (canonicalRoot) => {
      await stopServiceWithinOperation(canonicalRoot);
      return await resetDatabase(
        ctx.database,
        canonicalRoot,
        ctx.specs.gpuVramGb,
      );
    },
  );
  execution.output.info(`Database reset complete at ${fresh.root}`);
  execution.output.info("Initialized default configuration.");
  return { data: { reset: true, root: fresh.root } };
}
