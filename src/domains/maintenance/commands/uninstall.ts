import { uninstallManaged } from "../../../manager";
import { hasYesFlag, parseFlag } from "../../../utils/args";
import type { AppContext } from "../../../context";

export function runUninstall(args: string[], ctx: AppContext): number {
  if (!hasYesFlag(args)) {
    console.error(
      "uninstall removes all managed data. Re-run with --yes to confirm.",
    );
    return 2;
  }
  const targetRoot = parseFlag(args, "--root");
  const removed = uninstallManaged(targetRoot);
  console.log(`Removed all local-base managed data at ${removed}`);
  return 0;
}
