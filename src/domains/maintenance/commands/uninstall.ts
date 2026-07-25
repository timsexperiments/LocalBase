import { uninstallManaged } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { UninstallInput } from "../../app/commands/inputs";

export function runUninstall(
  input: UninstallInput,
  ctx: AppContext,
  execution: CommandExecution,
): number {
  if (!input.yes) {
    execution.output.error(
      "uninstall removes all managed data. Re-run with --yes to confirm.",
    );
    return 2;
  }
  const removed = uninstallManaged(ctx.database, ctx.config.root);
  execution.output.info(`Removed all local-base managed data at ${removed}`);
  return 0;
}
