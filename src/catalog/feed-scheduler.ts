/**
 * Keeps a gateway's remote catalog feed current: one refresh right after
 * start (skipped while the cached copy is fresh) and then a check every
 * hour. Each check downloads only when the cached copy is older than the
 * feed TTL, so the published feed is fetched about once a day.
 */
import {
  refreshCatalogFeed,
  type CatalogFeedRefreshOptions,
  type CatalogFeedRefreshResult,
} from "./remote-feed";
import { loadCatalogSources } from "./user-catalog";

export const CATALOG_FEED_CHECK_INTERVAL_MS = 60 * 60 * 1000;

export type CatalogFeedScheduler = Readonly<{
  /** Resolves when the in-flight refresh, if any, settles. */
  stop: () => Promise<void>;
}>;

export function startCatalogFeedScheduler(
  root: string,
  options: Readonly<{
    /** Called after every check, including `fresh` and `disabled`. */
    onResult?: (result: CatalogFeedRefreshResult) => void;
    intervalMs?: number;
    refresh?: CatalogFeedRefreshOptions;
  }> = {},
): CatalogFeedScheduler {
  let stopped = false;
  let running: Promise<void> | null = null;
  const check = () => {
    if (stopped || running) return;
    running = (async () => {
      try {
        const result = await refreshCatalogFeed(root, options.refresh);
        if (stopped) return;
        if (result.outcome === "updated") loadCatalogSources(root);
        options.onResult?.(result);
      } catch {
        // A refresh never takes the gateway down; the next check retries.
      } finally {
        running = null;
      }
    })();
  };
  const timer = setInterval(
    check,
    options.intervalMs ?? CATALOG_FEED_CHECK_INTERVAL_MS,
  );
  timer.unref?.();
  check();
  return {
    stop: async () => {
      stopped = true;
      clearInterval(timer);
      await running;
    },
  };
}
