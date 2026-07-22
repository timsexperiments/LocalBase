import { byId } from "../../../catalog";
import { installModel } from "../../../manager";
import type { AppContext } from "../../../context";

export async function runInstall(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const all = args.includes("--all");

  if (all) {
    const modelsToInstall = [
      ...ctx.config.selectedLlmModels,
      ...ctx.config.selectedSttModels,
      ...ctx.config.selectedImageModels,
    ];

    if (modelsToInstall.length === 0) {
      console.log("No models selected in the configuration to install.");
      return 0;
    }

    console.log(`Installing all ${modelsToInstall.length} selected models...`);
    for (const modelId of modelsToInstall) {
      if (!byId(modelId)) {
        console.warn(
          `⚠️  Skipping "${modelId}": Model does not exist in the catalog.`,
        );
        continue;
      }
      try {
        const path = await installModel(ctx.config, modelId);
        console.log(`✅ Installed: ${path}`);
      } catch (err) {
        console.error(
          `❌ Failed to install "${modelId}":`,
          (err as Error).message,
        );
        return 1;
      }
    }
    console.log("\n✅ All selected models installed successfully.");
    return 0;
  }

  const modelId = args.filter((a) => !a.startsWith("-"))[1];
  if (!modelId) {
    console.error("install requires <model_id> or --all");
    return 2;
  }

  const path = await installModel(ctx.config, modelId);
  console.log(`Installed: ${path}`);
  return 0;
}
