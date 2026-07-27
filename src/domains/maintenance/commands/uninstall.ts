import { uninstallManaged } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { UninstallInput } from "../../app/commands/inputs";
import { CliInputError } from "../../app/commands/errors";
import {
  removeServiceWithinOperation,
  withServiceRootOperation,
} from "../../service/manager";
import { assertDestructiveLocalBaseRoot } from "../../../utils/root";

export async function runUninstall(
  input: UninstallInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { removed: true; root: string } }> {
  if (!input.yes) {
    throw new CliInputError(
      "uninstall removes all managed data. Re-run with --yes to confirm.",
    );
  }
  assertDestructiveLocalBaseRoot(ctx.config.root);
  const { removed, inspection } = await withServiceRootOperation(
    ctx.config.root,
    "uninstall",
    async (canonicalRoot) => ({
      inspection: await removeServiceWithinOperation(canonicalRoot),
      removed: uninstallManaged(ctx.database, canonicalRoot),
    }),
  );
  execution.output.info(
    inspection
      ? `Removed LocalBase ${inspection.service.manager} service definition.`
      : "No managed user service was installed.",
  );
  execution.output.info(`Removed all local-base managed data at ${removed}`);
  return { data: { removed: true, root: removed } };
}
