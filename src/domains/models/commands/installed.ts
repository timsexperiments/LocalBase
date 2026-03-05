import { installedModels, type LocalBaseConfig } from "../../../manager";
import { parseFlag, parseKind } from "../../../utils/args";

export function runInstalled(args: string[], config: LocalBaseConfig): number {
  const kind = parseKind(parseFlag(args, "--kind"));
  const found = installedModels(config, kind);
  if (found.length === 0) {
    console.log(kind ? `No installed ${kind.toUpperCase()} models found.` : "No installed models found.");
    return 0;
  }
  for (const file of found) console.log(file);
  return 0;
}
