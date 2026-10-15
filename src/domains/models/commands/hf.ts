import { byId } from "../../../catalog";
import { capabilityLabel } from "../../../catalog/capabilities";
import { catalogProvenance } from "../../../catalog/sources";
import {
  removeUserCatalogEntry,
  writeUserCatalogEntry,
} from "../../../catalog/user-catalog";
import type { AppContext } from "../../../context";
import { installModel } from "../../../manager";
import { CliInputError } from "../../app/commands/errors";
import type { CommandExecution } from "../../app/commands/framework";
import type {
  AddInput,
  RemoveEntryInput,
  SearchInput,
} from "../../app/commands/inputs";
import { formatFileSize } from "../../../catalog/model-details";
import {
  fetchModelInfo,
  generateEntry,
  parseHfReference,
  searchHub,
  type HubFetch,
} from "../huggingface";

export type HubCommandOptions = Readonly<{ fetch?: HubFetch }>;

function hubToken(ctx: Pick<AppContext, "config">): string | undefined {
  return ctx.config.hfToken || process.env.HF_TOKEN || undefined;
}

/** Refuses ids that a built-in or remote entry already owns. */
function assertUserOwned(modelId: string): boolean {
  if (!byId(modelId)) return false;
  const provenance = catalogProvenance(modelId);
  if (provenance.origin !== "user") {
    throw new CliInputError(
      `"${modelId}" is already a ${provenance.origin} catalog model. Choose another id with --id, or patch the existing entry by hand in catalog.d (see docs/user-catalog.md).`,
    );
  }
  return true;
}

export async function runAdd(
  input: AddInput,
  ctx: AppContext,
  execution: CommandExecution,
  options: HubCommandOptions = {},
) {
  const reference = parseHfReference(input.reference);
  if (input.id) assertUserOwned(input.id);
  const root = ctx.config.root;
  const token = hubToken(ctx);
  const info = await fetchModelInfo(
    {
      root,
      ...(options.fetch ? { fetch: options.fetch } : {}),
      ...(token ? { token } : {}),
    },
    reference.repo,
  );
  const entry = generateEntry(
    info,
    reference,
    input.id ? { modelId: input.id } : {},
  );
  const replaced = assertUserOwned(entry.modelId);
  const { path, model } = writeUserCatalogEntry(root, entry.spec);
  const out = execution.output;
  out.info(
    `${replaced ? "Updated" : "Added"} ${model.modelId} (${model.displayName})`,
  );
  out.info(`  From:          ${entry.repo} at ${entry.revision}`);
  for (const file of entry.weights) {
    out.info(
      `  File:          ${file.path} (${formatFileSize(file.size)}, sha256 ${file.sha256.slice(0, 12)}…)`,
    );
  }
  if (entry.projector) {
    out.info(`  Projector:     ${entry.projector.path} (image input)`);
  }
  out.info(
    `  License:       ${entry.license ?? "not stated"} (${model.commercialStatus})`,
  );
  out.info(`  Pipeline:      ${entry.pipelineTag ?? "not stated"}`);
  out.info(
    `  Context:       ${entry.contextLength ? `${entry.contextLength.toLocaleString("en-US")} tokens` : "unknown until installed"}`,
  );
  out.info(
    `  Capabilities:  ${model.capabilities.map(({ id }) => capabilityLabel(id)).join(", ")} (declared)`,
  );
  if (entry.detected.length > 0) {
    out.info(
      `                 ${entry.detected.map(capabilityLabel).join(", ")} (detected from the chat template; confirmed after install)`,
    );
  }
  out.info(`  Catalog:       User entry, not qualified by LocalBase (${path})`);
  let installedPath: string | null = null;
  if (input.install) {
    installedPath = await installModel(ctx.config, model.modelId);
    out.info(`Installed: ${installedPath}`);
  } else {
    out.info(`Install it with: local-base models install ${model.modelId}`);
  }
  return {
    data: {
      modelId: model.modelId,
      path,
      repo: entry.repo,
      revision: entry.revision,
      license: entry.license,
      pipelineTag: entry.pipelineTag,
      files: entry.weights.map((file) => ({
        path: file.path,
        sizeBytes: file.size,
        sha256: file.sha256,
      })),
      projector: entry.projector?.path ?? null,
      declaredCapabilities: model.capabilities.map(({ id }) => id),
      detectedCapabilities: [...entry.detected],
      contextWindowTokens: entry.contextLength,
      replaced,
      installedPath,
    },
  };
}

function compactCount(value: number): string {
  if (value >= 1e6) return `${(value / 1e6).toFixed(1)}M`;
  if (value >= 1e3) return `${(value / 1e3).toFixed(1)}k`;
  return String(value);
}

export async function runSearch(
  input: SearchInput,
  ctx: Pick<AppContext, "config">,
  execution: CommandExecution,
  options: HubCommandOptions = {},
) {
  const { hits, cached } = await searchHub(
    {
      root: ctx.config.root,
      limit: input.limit,
      ...(options.fetch ? { fetch: options.fetch } : {}),
    },
    input.query,
  );
  const out = execution.output;
  if (hits.length === 0) {
    out.info("No GGUF models on Hugging Face match that search.");
  } else {
    const rows = hits.map((hit) => [
      hit.repo,
      compactCount(hit.downloads),
      hit.license ?? "-",
      hit.pipelineTag ?? "-",
      hit.likely.label,
    ]);
    const header = ["MODEL", "DOWNLOADS", "LICENSE", "PIPELINE", "LIKELY KIND"];
    const widths = header.map((title, column) =>
      Math.max(title.length, ...rows.map((row) => row[column]!.length)),
    );
    for (const row of [header, ...rows]) {
      out.info(
        row
          .map((cell, column) =>
            column === row.length - 1 ? cell : cell.padEnd(widths[column]!),
          )
          .join("  "),
      );
    }
    out.info("");
    out.info(
      "Add one with: local-base models add hf:<org>/<repo>[:<file-or-quant>]",
    );
  }
  if (cached) out.info("(Results from the local cache.)");
  return {
    data: {
      query: input.query,
      cached,
      results: hits.map((hit) => ({
        repo: hit.repo,
        downloads: hit.downloads,
        ...(hit.likes === undefined ? {} : { likes: hit.likes }),
        license: hit.license,
        pipelineTag: hit.pipelineTag,
        likelyKind: hit.likely.label,
        addable: hit.likely.kind !== null,
      })),
    },
  };
}

function selectedModelIds(config: AppContext["config"]): readonly string[] {
  return [
    ...config.selectedLlmModels,
    ...config.selectedSttModels,
    ...config.selectedTtsModels,
    ...config.selectedImageModels,
    ...config.selectedVideoModels,
  ];
}

export function runRemoveEntry(
  input: RemoveEntryInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const provenance = catalogProvenance(input.modelId);
  const selected = selectedModelIds(ctx.config).includes(input.modelId);
  // Removing the only entry for a selected model would leave a configuration
  // that names a model the catalog no longer has.
  if (selected && byId(input.modelId) && provenance.origin === "user") {
    throw new CliInputError(
      `"${input.modelId}" is selected. Deselect it first with \`local-base configure\`, then remove the entry.`,
    );
  }
  const path = removeUserCatalogEntry(ctx.config.root, input.modelId);
  if (!path) {
    throw new CliInputError(
      byId(input.modelId)
        ? `"${input.modelId}" is a ${catalogProvenance(input.modelId).origin} catalog model with no user file; there is nothing to remove.`
        : `No user catalog entry named "${input.modelId}".`,
    );
  }
  const out = execution.output;
  out.info(`Removed ${path}`);
  const restored = byId(input.modelId)
    ? (catalogProvenance(input.modelId).tier as "built-in" | "remote")
    : null;
  if (restored) {
    out.info(`${input.modelId} now uses the ${restored} catalog entry again.`);
  }
  if (!restored) {
    out.info(
      "Downloaded model files were left in place in the models directory; delete them by hand to free the space.",
    );
  }
  return {
    data: { modelId: input.modelId, path, restoredTier: restored, selected },
  };
}
