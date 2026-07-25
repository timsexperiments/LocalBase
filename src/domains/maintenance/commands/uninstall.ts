import { uninstallManaged } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { UninstallInput } from "../../app/commands/inputs";
import { CliInputError } from "../../app/commands/errors";

export function runUninstall(
  input: UninstallInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { removed: true; root: string } } {
  if (!input.yes) {
    throw new CliInputError(
      "uninstall removes all managed data. Re-run with --yes to confirm.",
    );
  }
  const removed = uninstallManaged(ctx.database, ctx.config.root);
  execution.output.info(`Removed all local-base managed data at ${removed}`);
  return { data: { removed: true, root: removed } };
}
