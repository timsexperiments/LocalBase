import { existsSync } from "node:fs";
import { join } from "node:path";
import {
  CATALOG_FEED_URL_ENV,
  catalogFeedSettings,
  catalogFeedStatus,
  refreshCatalogFeed,
  writeCatalogFeedSettings,
  type CatalogFeedFetch,
  type CatalogFeedStatus,
} from "../../../catalog/remote-feed";
import { loadCatalogSources } from "../../../catalog/user-catalog";
import type { AppContext } from "../../../context";
import { CliInputError } from "../../app/commands/errors";
import type { CommandExecution } from "../../app/commands/framework";
import type { FeedRefreshInput, FeedUseInput } from "../../app/commands/inputs";
import type { CommandOutput } from "../../app/commands/output";

type FeedContext = Pick<AppContext, "config">;
type FeedCommandOptions = Readonly<{
  fetch?: CatalogFeedFetch;
  environment?: Record<string, string | undefined>;
}>;

const sourceLabels: Record<CatalogFeedStatus["source"], string> = {
  default: "the published LocalBase feed",
  settings: "set with `local-base models feed use`",
  environment: `set by ${CATALOG_FEED_URL_ENV}`,
};

/** The text `models feed status` prints, also used after a refresh. */
export function catalogFeedStatusLines(status: CatalogFeedStatus): string[] {
  const lines = [
    `Feed:      ${status.url ?? "Off"} (${sourceLabels[status.source]})`,
  ];
  if (status.problem) lines.push(`Warning:   ${status.problem}`);
  if (!status.enabled) return lines;
  lines.push(
    status.fetchedAt
      ? `Fetched:   ${status.fetchedAt}${status.stale ? " (due for a refresh)" : ""}`
      : "Fetched:   Never; the built-in catalog is in use",
  );
  if (status.checkedAt && status.checkedAt !== status.fetchedAt) {
    lines.push(`Checked:   ${status.checkedAt}`);
  }
  if (status.revision) lines.push(`Revision:  ${status.revision}`);
  if (status.fetchedAt) {
    lines.push(
      `Models:    ${status.entries} in the feed · ${status.added.length} added (community) · ${status.builtIn} already built in${status.skipped ? ` · ${status.skipped} skipped as invalid` : ""}`,
    );
  }
  if (status.lastError) {
    lines.push(`Last error: ${status.lastError}`);
  }
  return lines;
}

function feedStatus(root: string, options: FeedCommandOptions) {
  return catalogFeedStatus(root, {
    ...(options.environment ? { environment: options.environment } : {}),
  });
}

function print(out: CommandOutput, lines: readonly string[]) {
  for (const line of lines) out.info(line);
}

export async function runFeedRefresh(
  input: FeedRefreshInput,
  ctx: FeedContext,
  execution: CommandExecution,
  options: FeedCommandOptions = {},
) {
  const root = ctx.config.root;
  const result = await refreshCatalogFeed(root, {
    force: input.force,
    ...options,
  });
  loadCatalogSources(root);
  const status = feedStatus(root, options);
  const out = execution.output;
  const summaries: Record<typeof result.outcome, string> = {
    disabled: "The remote catalog feed is off.",
    fresh:
      "The cached feed is less than a day old; nothing was downloaded. Use --force to download it now.",
    unchanged: "The feed has not changed.",
    updated: `Downloaded the feed: ${status.added.length} community model(s) added to the catalog.`,
    failed: `The feed could not be refreshed: ${result.error}${status.fetchedAt ? " The last good copy stays in use." : " The built-in catalog stays in use."}`,
  };
  if (result.outcome === "failed") out.error(summaries.failed);
  else out.info(summaries[result.outcome]);
  print(out, catalogFeedStatusLines(status));
  return {
    data: { outcome: result.outcome, error: result.error, feed: status },
    ...(result.outcome === "failed" ? { exitCode: 1 } : {}),
  };
}

export function runFeedStatus(
  _input: unknown,
  ctx: FeedContext,
  execution: CommandExecution,
  options: FeedCommandOptions = {},
) {
  const status = feedStatus(ctx.config.root, options);
  print(execution.output, catalogFeedStatusLines(status));
  return { data: { feed: status } };
}

export function runFeedUse(
  input: FeedUseInput,
  ctx: FeedContext,
  execution: CommandExecution,
  options: FeedCommandOptions = {},
) {
  const root = ctx.config.root;
  if (!existsSync(join(root, ".localbase-root.json"))) {
    throw new CliInputError(
      `${root} is not a LocalBase root yet. Run \`local-base init\` first.`,
    );
  }
  writeCatalogFeedSettings(root, input.feed === "off" ? null : input.feed);
  loadCatalogSources(root);
  const out = execution.output;
  const settings = catalogFeedSettings(root, options.environment);
  if (settings.source === "environment") {
    out.error(
      `Saved, but ${CATALOG_FEED_URL_ENV} is set and takes precedence while it is.`,
    );
  }
  out.info(
    input.feed === "off"
      ? "The remote catalog feed is off. Only the built-in and user catalogs load."
      : input.feed === "default"
        ? "Using the published LocalBase feed."
        : `Using the feed at ${input.feed}.`,
  );
  if (settings.url && input.feed !== "off") {
    out.info(
      "Run `local-base models feed refresh` to download it now; a running gateway checks within the hour.",
    );
  }
  return { data: { feed: feedStatus(root, options) } };
}
