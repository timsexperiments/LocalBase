import { listModels } from "../../../catalog";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { CatalogInput } from "../../app/commands/inputs";

export function runCatalog(
  input: CatalogInput,
  _ctx: AppContext,
  execution: CommandExecution,
): { data: { models: ReturnType<typeof listModels> } } {
  const models = listModels(input.kind);
  for (const model of models) {
    const coding =
      model.kind === "llm" ? ` | coding=${model.codingScore}/10` : "";
    execution.output.info(
      `${model.kind.padEnd(5)} | ${model.modelId.padEnd(38)} | size=${model.size.padEnd(10)} | min_vram=${String(model.minVramGb).padStart(3)} GB | storage=${model.storageGb.toFixed(2)} GB | status=${model.commercialStatus}${coding}`,
    );
    execution.output.info(
      `      in=${model.inputModalities.join(",")} out=${model.outputModalities.join(",")} features=${model.features.join(",")}`,
    );
    execution.output.info(`      catch: ${model.catch}`);
  }
  return { data: { models } };
}
