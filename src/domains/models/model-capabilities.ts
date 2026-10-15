import { primaryArtifact, type ModelSpec } from "../../catalog";
import type { RuntimeCapabilityStore } from "../runtime/runtime-capabilities";
import {
  detectModelCapabilities,
  resolveModelCapabilities,
  type CapabilityDetectionReaders,
  type ResolvedCapabilities,
} from "./capability-detection";
import { directoryForModel } from "./model-metadata";

type ModelDirectories = Parameters<typeof directoryForModel>[0];

export interface ModelCapabilityReport extends ResolvedCapabilities {
  /** Facts detection read from engine profiles and installed files. */
  readonly evidence: readonly string[];
  /** The backend build that reported runtime capabilities, if any. */
  readonly runtimeBuild: string | null;
}

/** The artifact key runtime reports are stored under, or null if unpinned. */
export function runtimeCapabilityKey(model: ModelSpec): string | null {
  return primaryArtifact(model).sha256 ?? null;
}

/**
 * Resolves one model's effective capabilities from its declaration, its
 * installed files, and any stored runtime report for the same artifact. It
 * never downloads, hashes, or launches anything.
 */
export async function resolveCapabilitiesForModel(
  model: ModelSpec,
  config: ModelDirectories,
  store: RuntimeCapabilityStore | null,
  readers?: CapabilityDetectionReaders,
): Promise<ModelCapabilityReport> {
  const detected = await detectModelCapabilities(
    model,
    directoryForModel(config, model.kind),
    readers,
  ).catch(() => null);
  const key = runtimeCapabilityKey(model);
  let runtime = null;
  if (store && key) {
    try {
      runtime = store.read(model.modelId, key);
    } catch {
      runtime = null;
    }
  }
  return {
    ...resolveModelCapabilities(model, { detected, runtime }),
    evidence: detected?.evidence ?? [],
    runtimeBuild: runtime?.buildInfo ?? null,
  };
}

export async function resolveCatalogCapabilities(
  catalog: readonly ModelSpec[],
  config: ModelDirectories,
  store: RuntimeCapabilityStore | null,
  readers?: CapabilityDetectionReaders,
): Promise<ReadonlyMap<string, ModelCapabilityReport>> {
  const entries = await Promise.all(
    catalog.map(
      async (model) =>
        [
          model.modelId,
          await resolveCapabilitiesForModel(model, config, store, readers),
        ] as const,
    ),
  );
  return new Map(entries);
}
