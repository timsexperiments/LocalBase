import {
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { modelSpecSchema, type ModelSpec } from "../catalog";
import { safeFilenameSchema } from "../utils/checksum";
import { setFeedAnnotations } from "./annotations";
import { readRemoteCatalogLayer } from "./remote-feed";
import { canonicalLocalBaseRoot } from "../utils/root";
import {
  applyCatalogLayers,
  catalogProvenance,
  type CatalogComposition,
  type CatalogDiagnostic,
  type CatalogLayer,
  type CatalogLayerEntry,
} from "./sources";

/** Larger files are not model specs; skip them rather than read them. */
const MAX_ENTRY_BYTES = 1024 * 1024;

/** `<root>/catalog.d`: one JSON model spec (or patch) per file. */
export function userCatalogDirectory(root: string): string {
  return join(root, "catalog.d");
}

/**
 * Reads `<root>/catalog.d/*.json` in name order. Missing directories are
 * an empty layer; unreadable or malformed files become diagnostics.
 */
export function readUserCatalogLayer(root: string): CatalogLayer {
  const directory = userCatalogDirectory(root);
  let names: string[];
  try {
    names = readdirSync(directory);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ENOTDIR") {
      return { tier: "user", entries: [] };
    }
    return {
      tier: "user",
      entries: [],
      diagnostics: [
        {
          tier: "user",
          path: directory,
          message: `The user catalog directory could not be read: ${(error as Error).message}`,
        },
      ],
    };
  }
  const entries: CatalogLayerEntry[] = [];
  const diagnostics: CatalogDiagnostic[] = [];
  for (const name of names.filter((n) => n.endsWith(".json")).sort()) {
    const path = join(directory, name);
    try {
      const stat = lstatSync(path);
      if (!stat.isFile()) {
        diagnostics.push({
          tier: "user",
          path,
          message: "Skipped: not a regular file.",
        });
        continue;
      }
      if (stat.size > MAX_ENTRY_BYTES) {
        diagnostics.push({
          tier: "user",
          path,
          message: "Skipped: larger than 1 MiB.",
        });
        continue;
      }
      entries.push({ path, data: JSON.parse(readFileSync(path, "utf8")) });
    } catch (error) {
      diagnostics.push({
        tier: "user",
        path,
        message: `Skipped: ${error instanceof SyntaxError ? "invalid JSON" : "unreadable"} (${(error as Error).message}).`,
      });
    }
  }
  return { tier: "user", entries, diagnostics };
}

/**
 * Loads every catalog tier for `root` into the effective catalog: the last
 * good remote feed download (unless `remote` replaces it) and then the user
 * tier. Never touches the network; `refreshCatalogFeed` downloads the feed.
 */
export function loadCatalogSources(
  root: string,
  options: Readonly<{ remote?: CatalogLayer | null }> = {},
): CatalogComposition {
  const remote =
    options.remote === undefined
      ? readRemoteCatalogLayer(root)
      : options.remote;
  setFeedAnnotations(remote?.annotations ?? []);
  return applyCatalogLayers([
    ...(remote ? [remote] : []),
    readUserCatalogLayer(root),
  ]);
}

/**
 * Loads the catalog tiers for the root a command line names, before command
 * input validation looks model ids up. Mirrors the context's root order
 * (`--root`, then `LOCALBASE_ROOT`, then the default). An unusable root
 * loads nothing here; the context reports it later.
 */
export function loadCatalogForCommandLine(
  cliRoot: string | undefined,
  environment: Record<string, string | undefined> = process.env,
): void {
  try {
    loadCatalogSources(
      canonicalLocalBaseRoot(
        cliRoot ??
          environment.LOCALBASE_ROOT ??
          join(homedir(), ".local", "share", "local-base"),
      ),
    );
  } catch {
    // Invalid roots fail with a clear error when the context is created.
  }
}

export function userCatalogFilename(modelId: string): string {
  return safeFilenameSchema.parse(`${modelId}.json`);
}

/**
 * Writes a complete user entry to `<root>/catalog.d/<modelId>.json`
 * atomically and reloads the catalog. Throws if the spec is invalid.
 */
export function writeUserCatalogEntry(
  root: string,
  spec: unknown,
): Readonly<{ path: string; model: ModelSpec }> {
  const model = modelSpecSchema.parse(spec) as ModelSpec;
  const directory = userCatalogDirectory(root);
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  const path = join(directory, userCatalogFilename(model.modelId));
  const temporary = `${path}.${process.pid}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(spec, null, 2)}\n`, {
    mode: 0o600,
  });
  renameSync(temporary, path);
  loadCatalogSources(root);
  return { path, model };
}

/**
 * Deletes the user file that provides `modelId` and reloads the catalog.
 * Returns the removed path, or null when no user file provides the entry.
 */
export function removeUserCatalogEntry(
  root: string,
  modelId: string,
): string | null {
  loadCatalogSources(root);
  const provenance = catalogProvenance(modelId);
  const directory = userCatalogDirectory(root);
  let path =
    provenance.tier === "user" && provenance.path?.startsWith(`${directory}/`)
      ? provenance.path
      : null;
  if (!path) {
    // A skipped (invalid) file named after the model can still be removed.
    const candidate = join(directory, userCatalogFilename(modelId));
    try {
      if (lstatSync(candidate).isFile()) path = candidate;
    } catch {
      path = null;
    }
  }
  if (!path) return null;
  rmSync(path, { force: true });
  loadCatalogSources(root);
  return path;
}
