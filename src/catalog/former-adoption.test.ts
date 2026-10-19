import { afterEach, expect, test as bunTest } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  truncateSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  BUILT_IN_CATALOG,
  byId,
  listModels,
  primaryArtifact,
  type ModelSpec,
} from "../catalog";
import { DatabaseSession } from "../db/client";
import { modelMemoryFigure } from "../domains/models/memory-fit";
import { createModelManagement } from "../domains/models/model-management";
import { listServedModels } from "../domains/models/served-models";
import { RuntimeConfigController } from "../domains/runtime/config-snapshot";
import { createRuntimeLifecycleSnapshot } from "../domains/runtime/lifecycle-snapshot";
import type { RuntimeModality } from "../domains/runtime/modality";
import {
  defaultConfig,
  installedModels,
  readConfigSync,
  saveConfig,
} from "../manager";
import {
  keepFormerCatalogEntries,
  restoreBuiltInCatalog,
} from "../test/former-catalog";
import { FORMER_CATALOG_DATA, formerCatalogEntry } from "./former";
import { catalogProvenance, usesReferenceMeasurements } from "./sources";
import {
  FORMER_ENTRY_HINT,
  adoptFormerCatalogEntries,
  formerAdoptionLogPath,
  loadCatalogSources,
  removeUserCatalogEntry,
  userCatalogDirectory,
} from "./user-catalog";

// These tests replace the shared effective catalog; keep them serial.
const test = bunTest.serial;

/** Installed by the earlier release, the default chat model; not measured. */
const INSTALLED_DEFAULT = "qwen2.5-coder-1.5b-instruct-q4_k_m";
/** Selected by the earlier release, never downloaded; measured. */
const SELECTED_NOT_INSTALLED = "mistral-nemo-12b-instruct-q4_k_m";
/** Never installed or selected. */
const UNUSED = "gemma-3-27b-it-q4_k_m";
/** Selected speech and image models that also left the catalog. */
const SELECTED_STT = "whisper-tiny-en-q8_0";
const SELECTED_IMAGE = "dreamshaper-v8";

const roots: string[] = [];
afterEach(() => {
  restoreBuiltInCatalog();
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

function scratchRoot(): string {
  const root = mkdtempSync(join(tmpdir(), "localbase-former-"));
  roots.push(root);
  return root;
}

function installSparse(root: string, model: ModelSpec): string {
  const artifact = primaryArtifact(model);
  const directory = join(root, "models", model.kind);
  mkdirSync(directory, { recursive: true });
  const path = join(directory, artifact.filename);
  writeFileSync(path, "");
  truncateSync(path, artifact.expectedSizeBytes ?? 0);
  return path;
}

function former(modelId: string): ModelSpec {
  const model = formerCatalogEntry(modelId);
  if (!model) throw new Error(`Missing former entry ${modelId}`);
  return model;
}

/**
 * A root as an earlier release left it: these models were built in, the
 * settings select them and the default chat model is installed.
 */
function upgradedRoot(): string {
  const root = scratchRoot();
  keepFormerCatalogEntries();
  const database = new DatabaseSession();
  try {
    const config = defaultConfig(root, 64);
    config.selectedLlmModels = [INSTALLED_DEFAULT, SELECTED_NOT_INSTALLED];
    config.activeLlmModel = INSTALLED_DEFAULT;
    config.selectedSttModels = [SELECTED_STT];
    config.activeSttModel = SELECTED_STT;
    config.selectedImageModels = [SELECTED_IMAGE];
    config.activeImageModel = SELECTED_IMAGE;
    saveConfig(database, config);
  } finally {
    database.close();
  }
  installSparse(root, former(INSTALLED_DEFAULT));
  // The upgrade: this binary no longer ships them.
  restoreBuiltInCatalog();
  return root;
}

function runtimeSnapshots() {
  const snapshot = (modality: RuntimeModality) =>
    createRuntimeLifecycleSnapshot({
      modality,
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    });
  return {
    llm: snapshot("llm"),
    stt: snapshot("stt"),
    tts: snapshot("tts"),
    image: snapshot("image"),
    video: snapshot("video"),
  };
}

test("former entries stay out of browsing and new installs", () => {
  const root = scratchRoot();
  loadCatalogSources(root);
  for (const { modelId } of [former(INSTALLED_DEFAULT), former(UNUSED)]) {
    expect(byId(modelId)).toBeUndefined();
    expect(listModels().some((model) => model.modelId === modelId)).toBe(false);
  }
  expect(existsSync(userCatalogDirectory(root))).toBe(false);
});

test("an installed former entry becomes a user entry with its pinned files", () => {
  const root = scratchRoot();
  installSparse(root, former(INSTALLED_DEFAULT));
  loadCatalogSources(root);

  const kept = byId(INSTALLED_DEFAULT);
  expect(kept).toBeDefined();
  expect(byId(UNUSED)).toBeUndefined();
  expect(catalogProvenance(INSTALLED_DEFAULT)).toMatchObject({
    trust: "user",
    origin: "user",
    tier: "user",
  });
  // Same id, revision, files and checksums as the release that shipped it.
  const shipped = former(INSTALLED_DEFAULT);
  expect(kept).toMatchObject({
    source: shipped.source,
    repositoryRevision: shipped.repositoryRevision,
    artifacts: shipped.artifacts,
    capabilities: shipped.capabilities,
  });
  expect(kept?.notes.startsWith(FORMER_ENTRY_HINT)).toBe(true);

  const file = JSON.parse(
    readFileSync(
      join(userCatalogDirectory(root), `${INSTALLED_DEFAULT}.json`),
      "utf8",
    ),
  );
  const data = FORMER_CATALOG_DATA.find(
    (entry) => (entry as { modelId: string }).modelId === INSTALLED_DEFAULT,
  ) as Record<string, unknown>;
  expect({ ...file, notes: data.notes }).toEqual(data);
  // Loading again changes nothing.
  expect(adoptFormerCatalogEntries(root)).toEqual([]);
});

test("a kept entry keeps its checked-in memory records until it is modified", () => {
  const root = scratchRoot();
  installSparse(root, former(SELECTED_NOT_INSTALLED));
  loadCatalogSources(root);
  const kept = byId(SELECTED_NOT_INSTALLED)!;
  expect(usesReferenceMeasurements(kept)).toBe(true);
  expect(modelMemoryFigure(kept).source).toBe("measured");

  const path = join(
    userCatalogDirectory(root),
    `${SELECTED_NOT_INSTALLED}.json`,
  );
  writeFileSync(
    path,
    JSON.stringify({
      ...JSON.parse(readFileSync(path, "utf8")),
      contextWindowTokens: 4096,
    }),
  );
  loadCatalogSources(root);
  expect(usesReferenceMeasurements(byId(SELECTED_NOT_INSTALLED)!)).toBe(false);
});

test("a user patch of a former built-in entry is applied on top of the kept spec", () => {
  const root = scratchRoot();
  installSparse(root, former(INSTALLED_DEFAULT));
  const path = join(userCatalogDirectory(root), `${INSTALLED_DEFAULT}.json`);
  mkdirSync(userCatalogDirectory(root), { recursive: true });
  writeFileSync(
    path,
    JSON.stringify({ modelId: INSTALLED_DEFAULT, displayName: "My coder" }),
  );
  loadCatalogSources(root);
  expect(byId(INSTALLED_DEFAULT)).toMatchObject({
    displayName: "My coder",
    artifacts: former(INSTALLED_DEFAULT).artifacts,
  });
});

test("a kept entry the user removes is not written again", () => {
  const root = scratchRoot();
  installSparse(root, former(INSTALLED_DEFAULT));
  loadCatalogSources(root);
  expect(removeUserCatalogEntry(root, INSTALLED_DEFAULT)).not.toBeNull();
  loadCatalogSources(root);
  expect(byId(INSTALLED_DEFAULT)).toBeUndefined();
  expect(readFileSync(formerAdoptionLogPath(root), "utf8")).toContain(
    INSTALLED_DEFAULT,
  );
});

test("upgrading keeps selected and default former models, their ids and settings", async () => {
  const root = upgradedRoot();
  expect(byId(INSTALLED_DEFAULT)).toBeUndefined();

  // Startup loads the catalog, then the settings.
  loadCatalogSources(root);
  const config = readConfigSync(root);
  expect(config.activeLlmModel).toBe(INSTALLED_DEFAULT);
  expect(config.selectedLlmModels).toEqual([
    INSTALLED_DEFAULT,
    SELECTED_NOT_INSTALLED,
  ]);
  expect(config.activeSttModel).toBe(SELECTED_STT);
  expect(config.activeImageModel).toBe(SELECTED_IMAGE);
  for (const id of [
    INSTALLED_DEFAULT,
    SELECTED_NOT_INSTALLED,
    SELECTED_STT,
    SELECTED_IMAGE,
  ]) {
    expect(byId(id)).toBeDefined();
    expect(catalogProvenance(id).trust).toBe("user");
  }
  expect(byId(UNUSED)).toBeUndefined();

  // API clients keep calling the same ids.
  const served = await listServedModels(config);
  expect(served.map(({ id }) => id)).toEqual(
    expect.arrayContaining([
      INSTALLED_DEFAULT,
      SELECTED_NOT_INSTALLED,
      SELECTED_STT,
      SELECTED_IMAGE,
    ]),
  );
  expect(await installedModels(config, "llm")).toContain(INSTALLED_DEFAULT);
  // Built-in entries are unchanged beside them.
  for (const model of BUILT_IN_CATALOG)
    expect(catalogProvenance(model.modelId).trust).toBe("qualified");
});

test("kept models stay manageable: listed, enabled, disabled and uninstalled", async () => {
  const root = upgradedRoot();
  loadCatalogSources(root);
  const database = new DatabaseSession();
  try {
    const config = readConfigSync(root);
    const runtimeConfig = new RuntimeConfigController(database, root, config);
    const management = createModelManagement({
      runtimeConfig,
      lifecycle: runtimeSnapshots,
    });
    const listed = (await management.read()).models;
    expect(listed.find(({ id }) => id === INSTALLED_DEFAULT)).toMatchObject({
      installed: true,
      enabled: true,
      active: true,
    });
    expect(
      listed.find(({ id }) => id === SELECTED_NOT_INSTALLED),
    ).toMatchObject({ installed: false, enabled: true });
    expect(listed.some(({ id }) => id === UNUSED)).toBe(false);

    await management.run(SELECTED_NOT_INSTALLED, "disable");
    expect(runtimeConfig.copy().selectedLlmModels).toEqual([INSTALLED_DEFAULT]);

    // Switch the default to a current entry, then retire the old one.
    const replacement = byId("qwen3.5-4b-q4_k_m")!;
    installSparse(root, replacement);
    await management.run(replacement.modelId, "enable");
    await management.run(replacement.modelId, "activate");
    await management.run(INSTALLED_DEFAULT, "disable");
    await management.run(INSTALLED_DEFAULT, "uninstall");
    const path = join(
      root,
      "models",
      "llm",
      primaryArtifact(former(INSTALLED_DEFAULT)).filename,
    );
    expect(existsSync(path)).toBe(false);
    expect(runtimeConfig.copy().activeLlmModel).toBe(replacement.modelId);
    // Like a model added from Hugging Face, its entry stays until removed.
    expect(byId(INSTALLED_DEFAULT)).toBeDefined();
  } finally {
    database.close();
  }
});
