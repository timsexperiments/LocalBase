import { byId } from "../../../catalog";
import { installModel } from "../../../manager";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { InstallInput } from "../../app/commands/inputs";

export async function runInstall(
  input: InstallInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { installed: Array<{ modelId: string; path: string }> } }> {
  if (input.all) {
    const modelsToInstall = [
      ...ctx.config.selectedLlmModels,
      ...ctx.config.selectedSttModels,
      ...ctx.config.selectedImageModels,
    ];

    if (modelsToInstall.length === 0) {
      execution.output.info(
        "No models selected in the configuration to install.",
      );
      return { data: { installed: [] } };
    }

    execution.output.info(
      `Installing all ${modelsToInstall.length} selected models...`,
    );
    const installed: Array<{ modelId: string; path: string }> = [];
    for (const modelId of modelsToInstall) {
      if (!byId(modelId)) {
        execution.output.info(
          `⚠️  Skipping "${modelId}": Model does not exist in the catalog.`,
        );
        continue;
      }
      try {
        const path = await installModel(ctx.config, modelId);
        execution.output.info(`✅ Installed: ${path}`);
        installed.push({ modelId, path });
      } catch (err) {
        execution.output.error(
          `❌ Failed to install "${modelId}": ${(err as Error).message}`,
        );
        throw err;
      }
    }
    execution.output.info("\n✅ All selected models installed successfully.");
    return { data: { installed } };
  }

  const path = await installModel(ctx.config, input.modelId!);
  execution.output.info(`Installed: ${path}`);
  return { data: { installed: [{ modelId: input.modelId!, path }] } };
}
