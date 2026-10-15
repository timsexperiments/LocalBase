/**
 * Writes `catalog/feed.json`, the remote catalog feed gateways download
 * (see `src/catalog/remote-feed.ts`), from the catalog data files.
 * `--check` exits non-zero when the checked-in feed is out of date.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { CATALOG_DATA } from "../src/catalog/models";
import { buildCatalogFeed } from "../src/catalog/remote-feed";

export const CATALOG_FEED_PATH = join(
  import.meta.dir,
  "..",
  "catalog",
  "feed.json",
);

export function renderCatalogFeed(): string {
  return `${JSON.stringify(buildCatalogFeed(CATALOG_DATA), null, 2)}\n`;
}

if (import.meta.main) {
  const rendered = renderCatalogFeed();
  if (process.argv.includes("--check")) {
    let current = "";
    try {
      current = readFileSync(CATALOG_FEED_PATH, "utf8");
    } catch {}
    if (current !== rendered) {
      console.error(
        "catalog/feed.json is out of date. Run `bun run catalog:feed`.",
      );
      process.exit(1);
    }
  } else {
    writeFileSync(CATALOG_FEED_PATH, rendered);
    console.log(`Wrote ${CATALOG_FEED_PATH}.`);
  }
}
