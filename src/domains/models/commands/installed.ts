import { installedModels } from "../../../manager";
import { parseFlag, parseKind } from "../../../utils/args";
import type { AppContext } from "../../../context";

export async function runInstalled(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const kind = parseKind(parseFlag(args, "--kind"));
  const found = await installedModels(ctx.config, kind);
  if (found.length === 0) {
    console.log(
      kind
        ? `No installed ${kind.toUpperCase()} models found.`
        : "No installed models found.",
    );
    return 0;
  }
  for (const file of found) console.log(file);
  return 0;
}
