import { createCustomVoiceStore } from "../../voices/voice-store";
import { CATALOG, byId } from "../../../catalog";
import {
  capabilityLabel,
  capabilitySourceLabels,
  describeCapabilityMismatch,
} from "../../../catalog/capabilities";
import {
  catalogTierLabels,
  catalogTrustLabels,
  describeFit,
  describeLaunchShape,
  describeMemoryBasis,
  formatFileSize,
  formatGb,
  formatTokens,
  licenseLabels,
  memoryFigureLabels,
  modeLabels,
  planningMinimumNote,
} from "../../../catalog/model-details";
import type { AnnotationSummary } from "../../../catalog/annotations";
import { catalogProvenance } from "../../../catalog/sources";
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
  /** The user or remote file the entry came from, when not built in. */
  catalogPath?: string,
): string[] {
  const { catalog } = model;
  const memory = catalog.memory;
  const resolution = catalog.capabilityResolution;
  const mode =
    catalog.capabilities?.kind === "embedding" ||
    catalog.capabilities?.kind === "decision"
      ? modeLabels[catalog.capabilities.kind]
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
    `  Catalog:        ${catalogTrustLabels[catalog.trust]} · ${catalogTierLabels[catalog.catalogTier]}${catalogPath ? ` (${catalogPath})` : ""}`,
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
    ...catalog.unsupportedCapabilities.map(
      ({ id, reason }) =>
        `  ${capabilityLabel(id)} · Verified unsupported: ${reason}`,
    ),
    ...resolution.mismatches.map(
      (mismatch) => `  Note: ${describeCapabilityMismatch(mismatch)}`,
    ),
    "",
    "Memory",
    `  Planning min:   ${memory.minimumVramEstimateGb} GB${memory.source ? ` (${memoryFigureLabels[memory.source].toLowerCase()})` : ""}`,
  ];
  if (memory.reference)
    lines.push(
      `  ${memoryFigureLabels[memory.reference.source]}: ${formatGb(memory.reference.bytes)} with ${describeLaunchShape(memory.reference.launchShape)} (${describeMemoryBasis(memory.reference)})`,
    );
  for (const peak of memory.observedPeaks ?? [])
    lines.push(
      `  Seen on this machine: ${formatGb(peak.peakBytes)} with ${describeLaunchShape(peak.launchShape)}, ${peak.observedAt.slice(0, 10)}`,
    );
  if (memory.fit) lines.push(`  This machine:   ${describeFit(memory.fit)}`);
  lines.push(`  Note:           ${planningMinimumNote}`);
  if (catalog.feedAnnotation)
    lines.push("", ...feedAnnotationLines(catalog.feedAnnotation));
  return lines;
}

const feedStatusLabels = {
  qualified: "Qualified by LocalBase",
  verified: "Verified by LocalBase",
} as const;

function describeFeedShape(shape: string): string {
  const described = describeLaunchShape(shape);
  return described === shape ? shape.split(";").join(", ") : described;
}

/** The "From the catalog feed" section of `models show`. */
export function feedAnnotationLines(annotation: AnnotationSummary): string[] {
  const lines = [
    "From the catalog feed",
    `  Status:         ${annotation.status ? feedStatusLabels[annotation.status] : "Not qualified or verified"}`,
    `  Published for:  ${annotation.repo}${annotation.file ? ` · ${annotation.file}` : ""} at ${annotation.revision}`,
  ];
  if (!annotation.revisionMatch)
    lines.push(
      "  Note:           The feed describes another revision than the one pinned here, so its marks are not applied.",
    );
  for (const peak of annotation.memoryPeaks)
    lines.push(
      `  Measured peak:  ${formatGb(peak.peakBytes)} with ${describeFeedShape(peak.launchShape)} (${peak.variant} on ${peak.hardware}, ${peak.measuredAt})`,
    );
  if (annotation.recipe)
    lines.push(
      `  Recipe:         ${annotation.recipe.fileCount} files, ${formatFileSize(annotation.recipe.totalBytes)}, pinned by sha256`,
    );
  for (const mark of annotation.unsupportedCapabilities)
    lines.push(
      `  ${mark.label} · Verified unsupported: ${mark.reason}${mark.applied ? "" : annotation.revisionMatch ? " (not applied: your entry declares it)" : " (not applied)"}`,
    );
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
    customVoices:
      spec.kind === "tts"
        ? createCustomVoiceStore(
            ctx.database.get(ctx.config.root),
            ctx.config.root,
          ).list()
        : [],
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
  for (const line of modelDetailLines(
    model,
    status,
    catalogProvenance(model.id).path,
  ))
    execution.output.info(line);
  return { data: { model, status } };
}
