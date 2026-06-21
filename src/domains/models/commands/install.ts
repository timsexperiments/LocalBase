import { installModel } from "../../../manager";
import type { AppContext } from "../../../context";

export async function runInstall(args: string[], ctx: AppContext): Promise<number> {
  const modelId = args[1];
  if (!modelId) {
    console.error("install requires <model_id>");
    return 2;
  }
  const path = await installModel(ctx.config, modelId);
  console.log(`Installed: ${path}`);
  return 0;
}
