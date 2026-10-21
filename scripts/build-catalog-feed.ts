/**
 * Writes the remote catalog feeds gateways download (see
 * `src/catalog/remote-feed.ts`) from the catalog data files:
 *
 * - `catalog/feed-v2.json`: models plus annotations (verified status,
 *   unsupported capabilities, measured memory peaks, multi-file recipes);
 * - `catalog/feed.json`: the models-only v1 document older binaries read.
 *
 * Former built-in entries (`src/catalog/former/`) stay in both feeds so
 * binaries built before they left the catalog keep finding them; their
 * annotations carry `verified` rather than `qualified`.
 *
 * `--check` exits non-zero when either checked-in feed is out of date.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { BUILT_IN_CATALOG } from "../src/catalog";
import { buildFeedAnnotations } from "../src/catalog/annotations";
import { CATALOG_DATA } from "../src/catalog/models";
import { FORMER_CATALOG, FORMER_CATALOG_DATA } from "../src/catalog/former";
import {
  buildCatalogFeed,
  buildLegacyCatalogFeed,
} from "../src/catalog/remote-feed";
import { MEMORY_MEASUREMENTS } from "../src/domains/runtime/memory-measurements";

const catalogDirectory = join(import.meta.dir, "..", "catalog");

/** The v1 feed, frozen in shape for binaries released before v2. */
export const CATALOG_FEED_PATH = join(catalogDirectory, "feed.json");
/** The v2 feed the current binary downloads by default. */
export const CATALOG_FEED_V2_PATH = join(catalogDirectory, "feed-v2.json");

function render(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

export function renderCatalogFeed(): string {
  return render(
    buildLegacyCatalogFeed([...CATALOG_DATA, ...FORMER_CATALOG_DATA]),
  );
}

export function renderCatalogFeedV2(): string {
  return render(
    buildCatalogFeed(
      [...CATALOG_DATA, ...FORMER_CATALOG_DATA],
      [
        ...buildFeedAnnotations(BUILT_IN_CATALOG, MEMORY_MEASUREMENTS),
        ...buildFeedAnnotations(FORMER_CATALOG, MEMORY_MEASUREMENTS, {
          status: "verified",
        }),
      ],
    ),
  );
}

if (import.meta.main) {
  const outputs = [
    [CATALOG_FEED_PATH, renderCatalogFeed()],
    [CATALOG_FEED_V2_PATH, renderCatalogFeedV2()],
  ] as const;
  if (process.argv.includes("--check")) {
    for (const [path, rendered] of outputs) {
      let current = "";
      try {
        current = readFileSync(path, "utf8");
      } catch {}
      if (current !== rendered) {
        console.error(
          `${path.slice(catalogDirectory.length - "catalog".length)} is out of date. Run \`bun run catalog:feed\`.`,
        );
        process.exit(1);
      }
    }
  } else {
    for (const [path, rendered] of outputs) {
      writeFileSync(path, rendered);
      console.log(`Wrote ${path}.`);
    }
  }
}
