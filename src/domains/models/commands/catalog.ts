import {
  legacyFeatures,
  listModels,
  withLegacyFeatures,
  type ModelSpec,
  type ModelSpecWithFeatures,
} from "../../../catalog";
import {
  queryCatalog,
  type CatalogQuery,
} from "../../../catalog/catalog-query";
import {
  annotationSummaryFor,
  headlinePeak,
} from "../../../catalog/annotations";
import { formatGb } from "../../../catalog/model-details";
import { catalogProvenance } from "../../../catalog/sources";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { CatalogInput } from "../../app/commands/inputs";
import {
  describeMemoryFigure,
  evaluateModelFit,
  modelMemoryJson,
  type MemoryFitEvaluation,
  type ModelMemoryJson,
} from "../memory-fit";
import { inspectCatalogInstallations } from "../model-metadata";
import { cliCapabilityReports, cliMemoryInputs } from "./memory-inputs";

/** The shared catalog query a set of `models catalog` flags describes. */
export function catalogQueryFromInput(input: CatalogInput): CatalogQuery {
  return {
    search: input.search,
    capabilities: input.capability,
    fits: input.fits ? ["fits", "tight"] : input.fit,
    installed: input.installed,
    licenses: input.license,
    sort: input.sort,
    reverse: input.reverse,
  };
}

/** Text a catalog search matches besides the model id and name. */
export function catalogSearchText(model: ModelSpec): string[] {
  return [
    model.family,
    model.provider,
    model.quant,
    model.size,
    model.catch,
    model.notes,
  ];
}

export async function runCatalog(
  input: CatalogInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { models: (ModelSpecWithFeatures & ModelMemoryJson)[] } }> {
  const models = listModels(input.kind, ctx.config.allowExperimental);
  const memory = cliMemoryInputs(ctx, models);
  const fits = new Map<string, MemoryFitEvaluation>(
    models.map((model) => [
      model.modelId,
      evaluateModelFit(model, ctx.specs.gpuVramGb, memory.get(model.modelId)),
    ]),
  );
  const query = catalogQueryFromInput(input);
  // Resolving capabilities and installations reads model files, so only
  // filters that need them pay for it.
  const [capabilities, installations] = await Promise.all([
    query.capabilities?.length ? cliCapabilityReports(ctx, models) : null,
    query.installed === undefined
      ? null
      : inspectCatalogInstallations(ctx.config, models),
  ]);
  const selected = queryCatalog(
    models,
    (model) => {
      const fit = fits.get(model.modelId)!;
      return {
        id: model.modelId,
        name: model.displayName ?? model.family,
        text: catalogSearchText(model),
        kind: model.kind,
        capabilities: (
          capabilities?.get(model.modelId)?.capabilities ?? model.capabilities
        ).map(({ id }) => id),
        fit: fit.verdict,
        installed: installations?.get(model.modelId) ?? false,
        license: model.commercialStatus,
        storageGb: model.storageGb,
        memoryGb: fit.minVramGb,
        contextTokens: model.contextWindowTokens,
      };
    },
    query,
  );
  const listed = selected.map((model) => {
    const fit = fits.get(model.modelId)!;
    const provenance = catalogProvenance(model.modelId);
    const coding =
      model.codingScore === undefined
        ? ""
        : ` | coding=${model.codingScore}/10`;
    execution.output.info(
      `${model.kind.padEnd(5)} | ${model.modelId.padEnd(38)} | size=${model.size.padEnd(10)} | min_vram=${String(fit.minVramGb).padStart(3)} GB ${fit.source.padEnd(8)} | fit=${fit.verdict.padEnd(8)} | storage=${model.storageGb.toFixed(2)} GB | status=${model.commercialStatus}${coding}`,
    );
    execution.output.info(
      `      in=${model.inputModalities.join(",")} out=${model.outputModalities.join(",")} features=${legacyFeatures(model).join(",")}${model.qualificationState === "experimental" ? " qualification=experimental (unqualified)" : ""}${provenance.trust === "qualified" ? "" : ` trust=${provenance.trust} source=${provenance.tier}`}`,
    );
    for (const unsupported of model.unsupportedCapabilities ?? [])
      execution.output.info(
        `      unsupported: ${unsupported.id} (verified: ${unsupported.reason})`,
      );
    const feed = annotationSummaryFor(model);
    if (feed) {
      const peak = headlinePeak(feed.memoryPeaks);
      execution.output.info(
        `      feed: ${feed.status ?? "annotated"}${feed.revisionMatch ? "" : " (other revision)"}${peak ? ` · measured peak ${formatGb(peak.peakBytes)}` : ""}${feed.recipe ? ` · recipe ${feed.recipe.fileCount} files` : ""}`,
      );
      for (const mark of feed.unsupportedCapabilities)
        if (mark.applied)
          execution.output.info(
            `      unsupported: ${mark.id} (feed, verified: ${mark.reason})`,
          );
    }
    if (fit.source === "observed")
      execution.output.info(
        `      memory: ${describeMemoryFigure(fit.figure)}`,
      );
    if (model.videoRuntime?.mode === "s2v")
      execution.output.info(
        "      Requires Linux x64 with a single NVIDIA GPU.",
      );
    execution.output.info(`      catch: ${model.catch}`);
    return {
      ...withLegacyFeatures(model),
      ...modelMemoryJson(fit),
      trust: provenance.trust,
      catalogTier: provenance.tier,
    };
  });
  if (!listed.length && models.length)
    execution.output.info("No models match these filters.");
  return { data: { models: listed } };
}
