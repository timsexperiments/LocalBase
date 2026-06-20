import { resetDatabase } from "../../../manager";
import { hasYesFlag, parseFlag } from "../../../utils/args";
import type { AppContext } from "../../../context";

export function runReset(args: string[], ctx: AppContext): number {
  if (!hasYesFlag(args)) {
    console.error("reset is destructive. Re-run with --yes to confirm.");
    return 2;
  }
  const resetRoot = parseFlag(args, "--root");
  const fresh = resetDatabase(resetRoot, ctx.specs.gpuVramGb);
  console.log(`Database reset complete at ${fresh.root}`);
  console.log("Reinstall/bootstrap complete with default configuration.");
  return 0;
}
