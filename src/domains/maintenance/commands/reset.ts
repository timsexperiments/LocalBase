import { resetDatabase } from "../../../manager";
import { detectSpecs } from "../../../system";
import { hasYesFlag, parseFlag } from "../../../utils/args";

export function runReset(args: string[]): number {
  if (!hasYesFlag(args)) {
    console.error("reset is destructive. Re-run with --yes to confirm.");
    return 2;
  }
  const resetRoot = parseFlag(args, "--root");
  const fresh = resetDatabase(resetRoot, detectSpecs().gpuVramGb);
  console.log(`Database reset complete at ${fresh.root}`);
  console.log("Reinstall/bootstrap complete with default configuration.");
  return 0;
}
