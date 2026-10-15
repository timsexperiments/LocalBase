import {
  legacyFeatures,
  listModels,
  withLegacyFeatures,
  type ModelSpecWithFeatures,
} from "../../../catalog";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { CatalogInput } from "../../app/commands/inputs";
import {
  describeMemoryFigure,
  evaluateModelFit,
  modelMemoryJson,
  type ModelMemoryJson,
} from "../memory-fit";
import { cliMemoryInputs } from "./memory-inputs";

export function runCatalog(
  input: CatalogInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { models: (ModelSpecWithFeatures & ModelMemoryJson)[] } } {
  const models = listModels(input.kind, ctx.config.allowExperimental);
  const memory = cliMemoryInputs(ctx, models);
  const listed = models.map((model) => {
    const fit = evaluateModelFit(
      model,
      ctx.specs.gpuVramGb,
      memory.get(model.modelId),
    );
    const coding =
      model.kind === "llm" ? ` | coding=${model.codingScore}/10` : "";
    execution.output.info(
      `${model.kind.padEnd(5)} | ${model.modelId.padEnd(38)} | size=${model.size.padEnd(10)} | min_vram=${String(fit.minVramGb).padStart(3)} GB ${fit.source.padEnd(8)} | fit=${fit.verdict.padEnd(8)} | storage=${model.storageGb.toFixed(2)} GB | status=${model.commercialStatus}${coding}`,
    );
    execution.output.info(
      `      in=${model.inputModalities.join(",")} out=${model.outputModalities.join(",")} features=${legacyFeatures(model).join(",")}${model.qualificationState === "experimental" ? " qualification=experimental (unqualified)" : ""}`,
    );
    if (fit.source === "observed")
      execution.output.info(
        `      memory: ${describeMemoryFigure(fit.figure)}`,
      );
    if (model.videoRuntime?.mode === "s2v")
      execution.output.info(
        "      Requires Linux x64 with a single NVIDIA GPU.",
      );
    execution.output.info(`      catch: ${model.catch}`);
    return { ...withLegacyFeatures(model), ...modelMemoryJson(fit) };
  });
  return { data: { models: listed } };
}
