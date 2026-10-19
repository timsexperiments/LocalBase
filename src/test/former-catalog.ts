import { FORMER_CATALOG_DATA } from "../catalog/former";
import { applyCatalogLayers } from "../catalog/sources";

/**
 * Loads every former built-in entry as a user entry, the way an install that
 * had them installed keeps them after upgrading (see
 * `adoptFormerCatalogEntries`). In-process tests whose fixtures are former
 * entries call this before they look models up, and `restoreBuiltInCatalog`
 * afterwards.
 */
export function keepFormerCatalogEntries(): void {
  applyCatalogLayers([
    {
      tier: "user",
      entries: FORMER_CATALOG_DATA.map((data, index) => ({
        path: `/former/${index}.json`,
        data,
      })),
    },
  ]);
}

/** Back to the built-in catalog alone. */
export function restoreBuiltInCatalog(): void {
  applyCatalogLayers([]);
}
