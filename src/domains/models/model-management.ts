import { lstatSync, realpathSync, statfsSync, unlinkSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { CATALOG, byId, type ModelKind, type ModelSpec } from "../../catalog";
import { installModel, type LocalBaseConfig } from "../../manager";
import type { RuntimeConfigController } from "../runtime/config-snapshot";
import type { RuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import type { RuntimeModality } from "../runtime/modality";
import { withRootOperation } from "../service/ownership";
import {
  ModelManagementError,
  type ModelManagement,
  type ModelManagementAction,
  type ModelManagementOperation,
} from "./model-management-contract";

const fields = {
  llm: {
    selected: "selectedLlmModels",
    active: "activeLlmModel",
    directory: "llmModelsDir",
  },
  stt: {
    selected: "selectedSttModels",
    active: "activeSttModel",
    directory: "sttModelsDir",
  },
  tts: {
    selected: "selectedTtsModels",
    active: "activeTtsModel",
    directory: "ttsModelsDir",
  },
  image: {
    selected: "selectedImageModels",
    active: "activeImageModel",
    directory: "imageModelsDir",
  },
  video: {
    selected: "selectedVideoModels",
    active: "activeVideoModel",
    directory: "videoModelsDir",
  },
} as const satisfies Record<
  ModelKind,
  {
    selected: keyof LocalBaseConfig;
    active: keyof LocalBaseConfig;
    directory: keyof LocalBaseConfig;
  }
>;

function missing(error: unknown): boolean {
  return error instanceof Error && "code" in error && error.code === "ENOENT";
}

/** Reject every symlink below the canonical root, including directory components. */
function safeFile(root: string, path: string) {
  const canonicalRoot = realpathSync(root);
  const suffix = relative(canonicalRoot, resolve(path));
  if (
    !suffix ||
    isAbsolute(suffix) ||
    suffix === ".." ||
    suffix.startsWith(`..${sep}`)
  ) {
    throw new ModelManagementError(
      "unsafe_path",
      "Artifact must remain inside the model root.",
    );
  }
  let current = canonicalRoot;
  const parts = suffix.split(sep);
  for (const [index, part] of parts.entries()) {
    current = join(current, part);
    let stat;
    try {
      stat = lstatSync(current);
    } catch (error) {
      if (missing(error)) return null;
      throw error;
    }
    if (
      stat.isSymbolicLink() ||
      (index < parts.length - 1 ? !stat.isDirectory() : !stat.isFile())
    ) {
      throw new ModelManagementError(
        "unsafe_path",
        "Artifact path contains a symlink or a non-file target.",
      );
    }
    if (index === parts.length - 1) return stat;
  }
  return null;
}

function storageAt(path: string): ModelManagement["storage"] {
  let current = path;
  for (;;) {
    try {
      const stat = statfsSync(current);
      return {
        availableBytes: stat.bavail * stat.bsize,
        totalBytes: stat.blocks * stat.bsize,
      };
    } catch (error) {
      if (!missing(error) || dirname(current) === current)
        return { availableBytes: null, totalBytes: null };
      current = dirname(current);
    }
  }
}

function paths(config: LocalBaseConfig, model: ModelSpec): string[] {
  return model.artifacts.map((artifact) =>
    join(config[fields[model.kind].directory], artifact.filename),
  );
}

function installConflict(
  config: LocalBaseConfig,
  model: ModelSpec,
  referenced: ReadonlySet<string | null>,
): string | null {
  const targetPaths = new Set(paths(config, model));
  const protectedModel = CATALOG.find((other) => {
    const field = fields[other.kind];
    return (
      (other.modelId === model.modelId ||
        paths(config, other).some((path) => targetPaths.has(path))) &&
      (config[field.selected].includes(other.modelId) ||
        config[field.active] === other.modelId ||
        referenced.has(other.modelId))
    );
  });
  if (!protectedModel) return null;
  return protectedModel.modelId === model.modelId
    ? "Disable this model and wait for the runtime to release it before installing."
    : `Disable ${protectedModel.modelId} and wait for its runtime to release shared artifacts before installing.`;
}

function inspect(config: LocalBaseConfig, model: ModelSpec) {
  let installedBytes = 0;
  let installed = true;
  let downloadBytes = 0;
  let authoritative = model.artifacts.length > 0;
  for (const artifact of model.artifacts) {
    const path = join(config[fields[model.kind].directory], artifact.filename);
    const stat = safeFile(config.root, path);
    const partial = safeFile(config.root, `${path}.partial`);
    installedBytes += (stat?.size ?? 0) + (partial?.size ?? 0);
    installed &&= !!stat && stat.size === artifact.expectedSizeBytes;
    const size = artifact.expectedSizeBytes;
    if (
      size === undefined ||
      !Number.isSafeInteger(size) ||
      size <= 0 ||
      !/^[a-fA-F0-9]{64}$/.test(artifact.sha256 ?? "")
    )
      authoritative = false;
    else downloadBytes += size;
  }
  return {
    installed,
    installedBytes,
    downloadBytes: authoritative ? downloadBytes : null,
  };
}

/**
 * Keep one instance per server. run resolves to running for accepted installs;
 * poll read for completion. Other actions finish before run resolves. Rejections
 * before acceptance are ModelManagementError; background failures stay in memory.
 * active means persisted default, not a claim that a runtime is resident.
 */
export function createModelManagement({
  runtimeConfig,
  lifecycle,
  protectedModelIds = () => new Set<string>(),
}: {
  runtimeConfig: RuntimeConfigController;
  lifecycle: () => Readonly<Record<RuntimeModality, RuntimeLifecycleSnapshot>>;
  protectedModelIds?: () => ReadonlySet<string>;
}) {
  const operations = new Map<string, ModelManagementOperation>();
  let installing: string | null = null;
  let inFlight: Promise<void> = Promise.resolve();

  async function read(): Promise<ModelManagement> {
    runtimeConfig.refreshSync();
    const config = runtimeConfig.copy();
    const storage = storageAt(config.root);
    const referenced = new Set([
      ...protectedModelIds(),
      ...Object.values(lifecycle()).map((runtime) => runtime.modelId),
    ]);
    return {
      storage,
      models: CATALOG.map((model) => {
        let facts;
        let installUnavailableReason: string | null = null;
        try {
          facts = inspect(config, model);
          safeFile(
            config.root,
            join(config[fields[model.kind].directory], ".checksums.json"),
          );
        } catch (error) {
          facts ??= {
            installed: false,
            installedBytes: 0,
            downloadBytes: null,
          };
          installUnavailableReason =
            error instanceof ModelManagementError
              ? error.message
              : "Could not inspect model storage.";
        }
        const available = storageAt(
          config[fields[model.kind].directory],
        ).availableBytes;
        const conflict = installConflict(config, model, referenced);
        if (installing !== null)
          installUnavailableReason =
            "Wait for the current installation to finish.";
        else if (conflict !== null) installUnavailableReason = conflict;
        else if (installUnavailableReason === null) {
          if (facts.downloadBytes === null)
            installUnavailableReason =
              "Catalog model lacks authoritative sizes or SHA-256 hashes.";
          else if (available === null)
            installUnavailableReason = "Could not determine available storage.";
          else if (available < facts.downloadBytes)
            installUnavailableReason =
              "Insufficient storage for model installation.";
        }
        return {
          id: model.modelId,
          ...facts,
          enabled: config[fields[model.kind].selected].includes(model.modelId),
          active: config[fields[model.kind].active] === model.modelId,
          canInstall: installUnavailableReason === null,
          installUnavailableReason,
          operation: operations.get(model.modelId) ?? null,
        };
      }),
    };
  }

  async function run(
    modelId: string,
    action: ModelManagementAction,
  ): Promise<ModelManagementOperation> {
    const model = byId(modelId);
    if (!model)
      throw new ModelManagementError(
        "invalid_request",
        "Unknown catalog model.",
      );
    if (installing !== null)
      throw new ModelManagementError(
        "conflict",
        "Wait for the current installation to finish.",
      );
    runtimeConfig.refreshSync();
    const config = runtimeConfig.copy();
    const field = fields[model.kind];
    const operation: ModelManagementOperation = {
      action,
      state: "complete",
      detail: "Complete",
      downloadedBytes: null,
      totalBytes: null,
    };
    if (action === "install") {
      installing = modelId;
      const running: ModelManagementOperation = {
        ...operation,
        state: "running",
        detail: "Preparing installation",
        downloadedBytes: 0,
        totalBytes: null,
      };
      const admission = Promise.withResolvers<void>();
      let admitted = false;
      const progress = new Map<string, number>();
      inFlight = withRootOperation(config.root, "install model", async () => {
        runtimeConfig.refreshSync();
        const fresh = runtimeConfig.copy();
        safeFile(fresh.root, join(fresh[field.directory], ".checksums.json"));
        const conflict = installConflict(
          fresh,
          model,
          new Set([
            ...protectedModelIds(),
            ...Object.values(lifecycle()).map((runtime) => runtime.modelId),
          ]),
        );
        if (conflict !== null)
          throw new ModelManagementError("conflict", conflict);
        const facts = inspect(fresh, model);
        if (facts.downloadBytes === null)
          throw new ModelManagementError(
            "invalid_request",
            "Catalog model lacks authoritative sizes or SHA-256 hashes.",
          );
        const available = storageAt(fresh[field.directory]).availableBytes;
        if (available === null)
          throw new ModelManagementError(
            "storage_unavailable",
            "Could not determine available storage.",
          );
        // Reserve the full download even for present artifacts: verification may replace them.
        if (available < facts.downloadBytes)
          throw new ModelManagementError(
            "insufficient_storage",
            "Insufficient storage for model installation.",
          );
        running.totalBytes = facts.downloadBytes;
        operations.set(modelId, running);
        admitted = true;
        admission.resolve();
        await installModel(fresh, modelId, undefined, (event) => {
          if (event.kind === "download-progress")
            progress.set(event.artifactFilename, event.downloadedBytes);
          if (event.kind === "verification-completed") {
            const artifact = model.artifacts.find(
              (item) => item.filename === event.artifactFilename,
            );
            if (artifact?.expectedSizeBytes !== undefined)
              progress.set(event.artifactFilename, artifact.expectedSizeBytes);
          }
          operations.set(modelId, {
            ...running,
            detail: event.kind,
            downloadedBytes: [...progress.values()].reduce(
              (sum, value) => sum + value,
              0,
            ),
          });
        });
      }).then(
        () => {
          operations.set(modelId, {
            ...running,
            state: "complete",
            detail: "Installation complete",
            downloadedBytes: running.totalBytes,
          });
          installing = null;
        },
        (error) => {
          if (admitted) {
            operations.set(modelId, {
              ...(operations.get(modelId) ?? running),
              state: "failed",
              detail:
                "Installation failed. Check storage, network access, and catalog verification.",
            });
          } else {
            admission.reject(error);
          }
          installing = null;
        },
      );
      await admission.promise;
      return running;
    }
    if (action === "uninstall") {
      await withRootOperation(config.root, "uninstall model", async () => {
        runtimeConfig.refreshSync();
        const fresh = runtimeConfig.copy();
        if (
          fresh[field.selected].includes(modelId) ||
          fresh[field.active] === modelId ||
          protectedModelIds().has(modelId) ||
          Object.values(lifecycle()).some(
            (runtime) => runtime.modelId === modelId,
          )
        ) {
          throw new ModelManagementError(
            "conflict",
            "Disable the model and wait for runtime reconciliation before uninstalling.",
          );
        }
        const shared = new Set(
          CATALOG.filter((other) => other.modelId !== modelId).flatMap(
            (other) =>
              paths(fresh, other).flatMap((path) => [path, `${path}.partial`]),
          ),
        );
        const targets = [
          ...new Set(
            paths(fresh, model).flatMap((path) => [path, `${path}.partial`]),
          ),
        ].filter((path) => !shared.has(path));
        const present = targets.filter(
          (path) => safeFile(fresh.root, path) !== null,
        );
        for (const path of present) unlinkSync(path);
      });
    } else {
      await runtimeConfig.update((fresh) => {
        const selected = fresh[field.selected];
        if (
          (action === "enable" || action === "activate") &&
          !inspect(fresh, model).installed
        )
          throw new ModelManagementError(
            "conflict",
            "Install the model first.",
          );
        if (action === "activate") {
          if (!selected.includes(modelId))
            throw new ModelManagementError(
              "conflict",
              "Enable the model before activating it.",
            );
          fresh[field.active] = modelId;
        } else if (action === "enable") {
          if (!selected.includes(modelId)) selected.push(modelId);
        } else if (action === "disable") {
          if (fresh[field.active] === modelId) {
            if (model.kind === "llm")
              throw new ModelManagementError(
                "conflict",
                "Activate another LLM before disabling the default.",
              );
            fresh[field.active] = "";
          }
          fresh[field.selected] = selected.filter((id) => id !== modelId);
        }
      });
    }
    operations.set(modelId, operation);
    return operation;
  }
  /** Wait for the accepted installation to settle. Stop admitting requests before shutdown. */
  function whenIdle(): Promise<void> {
    return inFlight;
  }
  return { read, run, whenIdle };
}
