import { z } from "zod";
import { capabilitySchema, type Capability } from "./capabilities";

/**
 * Filtering and sorting for catalog listings, shared by the CLI's
 * `models catalog` and the web UI's model manager. It works on plain rows so
 * it stays free of runtime imports; each caller maps its own model shape
 * into a {@link CatalogRow}.
 */

export const fitVerdictSchema = z.enum(["fits", "tight", "wont-fit"]);
export type FitVerdict = z.infer<typeof fitVerdictSchema>;

/** The catalog's commercial-use classification; the license filter uses it. */
export const licenseSchema = z.enum(["open", "conditional", "prohibited"]);
export type License = z.infer<typeof licenseSchema>;

export const catalogSortSchema = z.enum([
  "name",
  "size",
  "memory",
  "fit",
  "context",
]);
export type CatalogSort = z.infer<typeof catalogSortSchema>;

export type CatalogRow = {
  id: string;
  name: string;
  /** Free text that search also matches, such as a description. */
  text?: readonly string[];
  kind: string;
  /** Effective capabilities: runtime over detected over declared. */
  capabilities: readonly string[];
  fit: FitVerdict | null;
  installed: boolean;
  license: License | null;
  storageGb: number;
  memoryGb: number;
  contextTokens: number | null;
};

export type CatalogQuery = {
  search?: string;
  kinds?: readonly string[];
  /** A model must have every listed capability. */
  capabilities?: readonly Capability[];
  /** A model's verdict must be one of these. */
  fits?: readonly FitVerdict[];
  installed?: boolean;
  licenses?: readonly License[];
  /** Without a sort the input order is kept. */
  sort?: CatalogSort;
  reverse?: boolean;
};

export function catalogRowMatches(row: CatalogRow, query: CatalogQuery) {
  if (query.kinds?.length && !query.kinds.includes(row.kind)) return false;
  if (query.installed !== undefined && row.installed !== query.installed)
    return false;
  if (query.fits?.length && (!row.fit || !query.fits.includes(row.fit)))
    return false;
  if (
    query.licenses?.length &&
    (!row.license || !query.licenses.includes(row.license))
  )
    return false;
  if (query.capabilities?.some((id) => !row.capabilities.includes(id)))
    return false;
  const terms = (query.search ?? "").toLowerCase().split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  const haystack = [row.id, row.name, ...(row.text ?? [])]
    .join("\n")
    .toLowerCase();
  return terms.every((term) => haystack.includes(term));
}

const fitRank: Record<FitVerdict, number> = {
  fits: 0,
  tight: 1,
  "wont-fit": 2,
};

/**
 * Natural order for each field: name A to Z, smallest download or memory
 * first, best fit first, and longest context first. Unknown values always
 * sort last, even when reversed.
 */
function compareBy(sort: CatalogSort, a: CatalogRow, b: CatalogRow) {
  switch (sort) {
    case "name":
      return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
    case "size":
      return a.storageGb - b.storageGb;
    case "memory":
      return a.memoryGb - b.memoryGb;
    case "fit":
      return fitRank[a.fit!] - fitRank[b.fit!] || a.memoryGb - b.memoryGb;
    case "context":
      return b.contextTokens! - a.contextTokens!;
  }
}

function unknown(sort: CatalogSort, row: CatalogRow) {
  return (
    (sort === "fit" && row.fit === null) ||
    (sort === "context" && row.contextTokens === null)
  );
}

/** Rows that match the query, in the requested order. */
export function queryCatalog<T>(
  items: readonly T[],
  toRow: (item: T) => CatalogRow,
  query: CatalogQuery,
): T[] {
  const rows = items
    .map((item, index) => ({ item, index, row: toRow(item) }))
    .filter(({ row }) => catalogRowMatches(row, query));
  const sort = query.sort;
  if (sort) {
    const direction = query.reverse ? -1 : 1;
    rows.sort((a, b) => {
      const missingA = unknown(sort, a.row);
      const missingB = unknown(sort, b.row);
      if (missingA !== missingB) return missingA ? 1 : -1;
      const order =
        (missingA ? 0 : compareBy(sort, a.row, b.row)) ||
        a.row.name.localeCompare(b.row.name) ||
        a.row.id.localeCompare(b.row.id);
      return direction * order || a.index - b.index;
    });
  } else if (query.reverse) rows.reverse();
  return rows.map(({ item }) => item);
}

/** Whether any filter is set; sorting alone never hides a model. */
export function catalogQueryFilters(query: CatalogQuery): boolean {
  return Boolean(
    query.search?.trim() ||
    query.kinds?.length ||
    query.capabilities?.length ||
    query.fits?.length ||
    query.installed !== undefined ||
    query.licenses?.length,
  );
}

const listParam =
  <T extends string>(schema: z.ZodType<T>) =>
  (values: readonly string[]) =>
    values
      .flatMap((value) => value.split(","))
      .map((value) => value.trim())
      .filter((value): value is T => schema.safeParse(value).success);

const parseCapabilities = listParam<Capability>(capabilitySchema);
const parseFits = listParam<FitVerdict>(fitVerdictSchema);
const parseLicenses = listParam<License>(licenseSchema);

/** The query parameters a catalog query reads and writes. */
export const catalogQueryKeys = [
  "q",
  "kind",
  "capability",
  "fit",
  "installed",
  "license",
  "sort",
  "reverse",
] as const;

/**
 * Reads a query from URL parameters. Unknown values are dropped rather than
 * rejected, so a stale or hand-edited link still opens the catalog.
 */
export function parseCatalogQuery(
  params: URLSearchParams,
  kinds?: readonly string[],
): CatalogQuery {
  const query: CatalogQuery = {};
  const search = params.get("q")?.trim();
  if (search) query.search = search;
  const kind = params
    .getAll("kind")
    .flatMap((value) => value.split(","))
    .map((value) => value.trim())
    .filter((value) => value && (!kinds || kinds.includes(value)));
  if (kind.length) query.kinds = kind;
  const capabilities = parseCapabilities(params.getAll("capability"));
  if (capabilities.length) query.capabilities = capabilities;
  const fits = parseFits(params.getAll("fit"));
  if (fits.length) query.fits = fits;
  const installed = params.get("installed");
  if (installed === "true" || installed === "false")
    query.installed = installed === "true";
  const licenses = parseLicenses(params.getAll("license"));
  if (licenses.length) query.licenses = licenses;
  const sort = catalogSortSchema.safeParse(params.get("sort"));
  if (sort.success) query.sort = sort.data;
  if (params.get("reverse") === "true") query.reverse = true;
  return query;
}

/** URL parameters for a query, omitting everything left at its default. */
export function catalogQueryParams(query: CatalogQuery): URLSearchParams {
  const params = new URLSearchParams();
  if (query.search?.trim()) params.set("q", query.search.trim());
  if (query.kinds?.length) params.set("kind", query.kinds.join(","));
  for (const capability of query.capabilities ?? [])
    params.append("capability", capability);
  if (query.fits?.length) params.set("fit", query.fits.join(","));
  if (query.installed !== undefined)
    params.set("installed", String(query.installed));
  if (query.licenses?.length) params.set("license", query.licenses.join(","));
  if (query.sort) params.set("sort", query.sort);
  if (query.reverse) params.set("reverse", "true");
  return params;
}

/** The parts of a `/_localbase/models` entry that a catalog query reads. */
export type CatalogMetadata = {
  id: string;
  catalog: {
    name: string;
    displayName?: string;
    kind: string;
    quantization: string;
    summary?: string;
    commercialStatus?: License;
    contextWindowTokens: number | null;
    features: readonly string[];
    capabilityResolution?: { capabilities: readonly { id: string }[] };
    memory: {
      minimumVramEstimateGb: number;
      storageEstimateGb: number;
      fit?: { verdict: FitVerdict };
    };
  };
  device: { installed: boolean };
};

/**
 * A row for a model metadata entry, used by the gateway's list filter and
 * the web UI. Capabilities prefer the effective set and fall back to the
 * declared features older servers send.
 */
export function metadataCatalogRow(model: CatalogMetadata): CatalogRow {
  const { catalog } = model;
  return {
    id: model.id,
    name: catalog.displayName ?? catalog.name,
    text: [catalog.name, catalog.quantization, catalog.summary ?? ""],
    kind: catalog.kind,
    capabilities:
      catalog.capabilityResolution?.capabilities.map(({ id }) => id) ??
      catalog.features,
    fit: catalog.memory.fit?.verdict ?? null,
    installed: model.device.installed,
    license: catalog.commercialStatus ?? null,
    storageGb: catalog.memory.storageEstimateGb,
    memoryGb: catalog.memory.minimumVramEstimateGb,
    contextTokens: catalog.contextWindowTokens,
  };
}
