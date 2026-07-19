import { listModels } from "../../../catalog";
import { parseFlag, parseKind } from "../../../utils/args";
import type { AppContext } from "../../../context";

export function runCatalog(args: string[], ctx: AppContext): number {
  const kind = parseKind(parseFlag(args, "--kind"));
  for (const model of listModels(kind)) {
    const coding =
      model.kind === "llm" ? ` | coding=${model.codingScore}/10` : "";
    console.log(
      `${model.kind.padEnd(5)} | ${model.modelId.padEnd(38)} | size=${model.size.padEnd(10)} | min_vram=${String(model.minVramGb).padStart(3)} GB | storage=${model.storageGb.toFixed(2)} GB | status=${model.commercialStatus}${coding}`,
    );
    console.log(
      `      in=${model.inputModalities.join(",")} out=${model.outputModalities.join(",")} features=${model.features.join(",")}`,
    );
    console.log(`      catch: ${model.catch}`);
  }
  return 0;
}
