import { CATALOG, byId } from "../../../catalog";
import {
  capabilityLabel,
  capabilitySourceLabels,
  describeCapabilityMismatch,
} from "../../../catalog/capabilities";
import {
  describeFit,
  describeLaunchShape,
  formatFileSize,
  formatGb,
  formatTokens,
  licenseLabels,
  memoryFigureLabels,
  modeLabels,
} from "../../../catalog/model-details";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { ShowInput } from "../../app/commands/inputs";
import {
  inspectCatalogInstallations,
  modelMetadataById,
  type ModelMetadata,
} from "../model-metadata";
import { cliCapabilityReports, cliMemoryInputs } from "./memory-inputs";

export type ModelStatus = Readonly<{
  installed: boolean;
  enabled: boolean;
  default: boolean;
}>;

/** The text lines `models show` prints; shared wording with the UI. */
export function modelDetailLines(
  model: ModelMetadata,
  status: ModelStatus,
): string[] {
  const { catalog } = model;
  const memory = catalog.memory;
  const resolution = catalog.capabilityResolution;
  const mode =
    catalog.capabilities?.kind === "embedding"
      ? modeLabels.embedding
      : modeLabels[catalog.kind];
  const lines = [
    catalog.displayName,
    `  ID:             ${model.id}`,
    `  Type:           ${mode}${catalog.qualificationState === "experimental" ? " (experimental, not yet qualified)" : ""}`,
    `  Status:         ${[
      status.installed ? "Installed" : "Not installed",
      status.enabled ? "Enabled" : "Disabled",
      ...(status.default ? ["Default"] : []),
    ].join(" · ")}`,
    `  License:        ${licenseLabels[catalog.commercialStatus]}`,
    `  Publisher:      ${catalog.provider}`,
    `  Source:         ${catalog.source} (revision ${catalog.revision})`,
    `  Context length: ${catalog.contextWindowTokens === null ? (catalog.kind === "llm" ? "Not listed in the catalog" : "Not applicable") : formatTokens(catalog.contextWindowTokens)}`,
    `  Quantization:   ${catalog.quantization}`,
    `  Summary:        ${catalog.summary}`,
    ...(catalog.notes ? [`  Notes:          ${catalog.notes}`] : []),
    "",
    "Files",
    ...catalog.artifacts.map(
      (artifact) =>
        `  ${artifact.filename} · ${formatFileSize(artifact.sizeBytes)}${artifact.role === "supplementary" ? " · supporting file" : ""}${artifact.source ? ` · from ${artifact.source.repositoryUrl} (revision ${artifact.source.revision})` : ""}`,
    ),
    "",
    "Capabilities",
    ...(resolution.capabilities.length
      ? resolution.capabilities.map(
          ({ id, source }) =>
            `  ${capabilityLabel(id)} · ${capabilitySourceLabels[source]}`,
        )
      : ["  None listed"]),
    ...resolution.mismatches.map(
      (mismatch) => `  Note: ${describeCapabilityMismatch(mismatch)}`,
    ),
    "",
    "Memory",
    `  Needs at least: ${memory.minimumVramEstimateGb} GB${memory.source ? ` (${memoryFigureLabels[memory.source].toLowerCase()})` : ""}`,
  ];
  if (memory.reference)
    lines.push(
      `  ${memoryFigureLabels[memory.reference.source]}: ${formatGb(memory.reference.bytes)} with ${describeLaunchShape(memory.reference.launchShape)}`,
    );
  for (const peak of memory.observedPeaks ?? [])
    lines.push(
      `  Seen on this machine: ${formatGb(peak.peakBytes)} with ${describeLaunchShape(peak.launchShape)}, ${peak.observedAt.slice(0, 10)}`,
    );
  if (memory.fit) lines.push(`  This machine:   ${describeFit(memory.fit)}`);
  return lines;
}

export async function runShow(
  input: ShowInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { model: ModelMetadata; status: ModelStatus } }> {
  const spec = byId(input.modelId)!;
  const models = [spec];
  const [capabilities, installations] = await Promise.all([
    cliCapabilityReports(ctx, models),
    inspectCatalogInstallations(ctx.config, models),
  ]);
  const model = modelMetadataById(input.modelId, {
    catalog: CATALOG,
    config: ctx.config,
    installations,
    capabilities,
    memory: cliMemoryInputs(ctx, models),
    systemMemoryGb: ctx.specs.gpuVramGb,
  })!;
  const config = ctx.config;
  const status: ModelStatus = {
    installed: model.device.installed,
    enabled: model.device.selected,
    default: [
      config.activeLlmModel,
      config.activeSttModel,
      config.activeTtsModel,
      config.activeImageModel,
      config.activeVideoModel,
    ].includes(model.id),
  };
  for (const line of modelDetailLines(model, status))
    execution.output.info(line);
  return { data: { model, status } };
}
