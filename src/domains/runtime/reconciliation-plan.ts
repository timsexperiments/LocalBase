import { modelDirectories, type LocalBaseConfig } from "../../manager";
import type { RuntimeConfigSnapshot } from "./config-snapshot";
import { runtimeModalities, type RuntimeModality } from "./modality";

export type ConfigFieldOwnership =
  | "restart-required"
  | "llm-launch"
  | "stt-launch"
  | "image-launch"
  | "modality-selection-request-scoped"
  | "observability";

export const configFieldOwnership = {
  root: "restart-required",
  llmModelsDir: "restart-required",
  sttModelsDir: "restart-required",
  imageModelsDir: "restart-required",
  host: "llm-launch",
  port: "llm-launch",
  ctxSize: "llm-launch",
  sttHost: "stt-launch",
  sttPort: "stt-launch",
  selectedLlmModels: "modality-selection-request-scoped",
  selectedSttModels: "modality-selection-request-scoped",
  selectedImageModels: "modality-selection-request-scoped",
  activeLlmModel: "llm-launch",
  activeSttModel: "stt-launch",
  activeImageModel: "image-launch",
  hfToken: "modality-selection-request-scoped",
  parallel: "llm-launch",
  otelEndpoint: "observability",
  otelHeaders: "observability",
  otelSampleRatio: "observability",
  memory: "restart-required",
} as const satisfies Record<keyof LocalBaseConfig, ConfigFieldOwnership>;

export type RuntimeConfigField = keyof LocalBaseConfig;

export type RuntimeOverrideConfigField = Exclude<
  RuntimeConfigField,
  "root" | "llmModelsDir" | "sttModelsDir" | "imageModelsDir" | "memory"
>;

export type RuntimeOverrideOwnership = Readonly<{
  configFields?: readonly RuntimeOverrideConfigField[];
  configuredModalities?: Partial<Readonly<Record<RuntimeModality, boolean>>>;
}>;

export type RestartRequiredPlan = Readonly<{
  sourceRevision: number;
  targetRevision: number;
  action: "unchanged" | "restart-required";
  changedFields: readonly (
    "root" | "llmModelsDir" | "sttModelsDir" | "imageModelsDir" | "memory"
  )[];
}>;

export type ModalityReconciliationAction =
  "unchanged" | "add" | "drain-and-replace" | "drain-and-remove";

export type ModalityReconciliationPlan = Readonly<{
  modality: RuntimeModality;
  sourceRevision: number;
  targetRevision: number;
  action: ModalityReconciliationAction;
  sourceConfigured: boolean;
  targetConfigured: boolean;
  changedLaunchFields: readonly RuntimeConfigField[];
}>;

export type ObservabilityReconciliationPlan = Readonly<{
  sourceRevision: number;
  targetRevision: number;
  action: "unchanged" | "replace";
  changedFields: readonly (
    "otelEndpoint" | "otelHeaders" | "otelSampleRatio"
  )[];
}>;

export type RequestScopeReconciliationPlan = Readonly<{
  sourceRevision: number;
  targetRevision: number;
  changedFields: readonly (
    | "selectedLlmModels"
    | "selectedSttModels"
    | "selectedImageModels"
    | "hfToken"
  )[];
}>;

export type RuntimeReconciliationPlan = Readonly<{
  sourceRevision: number;
  targetRevision: number;
  restartRequired: RestartRequiredPlan;
  modalities: Readonly<Record<RuntimeModality, ModalityReconciliationPlan>>;
  requestScope: RequestScopeReconciliationPlan;
  observability: ObservabilityReconciliationPlan;
}>;

const restartRequiredFields = [
  "root",
  "llmModelsDir",
  "sttModelsDir",
  "imageModelsDir",
  "memory",
] as const;

const rootDerivedDirectoryFields = [
  "llmModelsDir",
  "sttModelsDir",
  "imageModelsDir",
] as const;

const modalityLaunchFields = {
  llm: ["host", "port", "ctxSize", "activeLlmModel", "parallel"],
  stt: ["sttHost", "sttPort", "activeSttModel"],
  image: ["activeImageModel"],
} as const satisfies Record<RuntimeModality, readonly RuntimeConfigField[]>;

const requestScopeFields = [
  "selectedLlmModels",
  "selectedSttModels",
  "selectedImageModels",
  "hfToken",
] as const;

const observabilityFields = [
  "otelEndpoint",
  "otelHeaders",
  "otelSampleRatio",
] as const;

function freezeFields<Field extends string>(
  fields: readonly Field[],
): readonly Field[] {
  return Object.freeze([...fields]);
}

function valuesEqual(left: unknown, right: unknown): boolean {
  if (Array.isArray(left) && Array.isArray(right)) {
    return (
      left.length === right.length &&
      left.every((value, index) => valuesEqual(value, right[index]))
    );
  }
  if (left && right && typeof left === "object" && typeof right === "object") {
    const leftEntries = Object.entries(left).sort(([leftKey], [rightKey]) =>
      leftKey.localeCompare(rightKey),
    );
    const rightEntries = Object.entries(right).sort(([leftKey], [rightKey]) =>
      leftKey.localeCompare(rightKey),
    );
    return (
      leftEntries.length === rightEntries.length &&
      leftEntries.every(
        ([key, value], index) =>
          key === rightEntries[index]?.[0] &&
          valuesEqual(value, rightEntries[index]?.[1]),
      )
    );
  }
  return left === right;
}

function changedFields<Field extends RuntimeConfigField>(
  source: RuntimeConfigSnapshot,
  target: RuntimeConfigSnapshot,
  fields: readonly Field[],
  overrides: ReadonlySet<RuntimeConfigField>,
): readonly Field[] {
  return freezeFields(
    fields.filter(
      (field) =>
        !overrides.has(field) &&
        !valuesEqual(source.config[field], target.config[field]),
    ),
  );
}

function assertRootDerivedDirectories(snapshot: RuntimeConfigSnapshot): void {
  const expected = modelDirectories(snapshot.config.root);
  for (const field of rootDerivedDirectoryFields) {
    if (snapshot.config[field] !== expected[field]) {
      throw new Error(
        `${field} must be derived from root in runtime snapshots.`,
      );
    }
  }
}

function ownedFields(
  ownership: RuntimeOverrideOwnership,
): ReadonlySet<RuntimeConfigField> {
  const fields = new Set<RuntimeConfigField>(ownership.configFields ?? []);
  for (const field of restartRequiredFields) {
    if (fields.has(field)) {
      throw new Error(`${field} cannot be owned by an in-process override.`);
    }
  }
  return fields;
}

export function configuredRuntimeModality(
  modality: RuntimeModality,
  config: RuntimeConfigSnapshot["config"],
  overrides: RuntimeOverrideOwnership,
): boolean {
  const overridden = overrides.configuredModalities?.[modality];
  if (overridden !== undefined) return overridden;
  if (modality === "llm") return true;
  if (modality === "stt") return config.selectedSttModels.length > 0;
  return config.selectedImageModels.length > 0;
}

function modalityPlan(
  modality: RuntimeModality,
  source: RuntimeConfigSnapshot,
  target: RuntimeConfigSnapshot,
  ownership: RuntimeOverrideOwnership,
  overrides: ReadonlySet<RuntimeConfigField>,
): ModalityReconciliationPlan {
  const sourceConfigured = configuredRuntimeModality(
    modality,
    source.config,
    ownership,
  );
  const targetConfigured = configuredRuntimeModality(
    modality,
    target.config,
    ownership,
  );
  const changedLaunchFields = changedFields(
    source,
    target,
    modalityLaunchFields[modality],
    overrides,
  );
  const action: ModalityReconciliationAction = !sourceConfigured
    ? targetConfigured
      ? "add"
      : "unchanged"
    : !targetConfigured
      ? "drain-and-remove"
      : changedLaunchFields.length > 0
        ? "drain-and-replace"
        : "unchanged";
  return Object.freeze({
    modality,
    sourceRevision: source.revision,
    targetRevision: target.revision,
    action,
    sourceConfigured,
    targetConfigured,
    changedLaunchFields,
  });
}

export function createRuntimeReconciliationPlan(
  source: RuntimeConfigSnapshot,
  target: RuntimeConfigSnapshot,
  ownership: RuntimeOverrideOwnership = {},
): RuntimeReconciliationPlan {
  assertRootDerivedDirectories(source);
  assertRootDerivedDirectories(target);
  const overrides = ownedFields(ownership);
  const restartRequiredChanges = changedFields(
    source,
    target,
    restartRequiredFields,
    overrides,
  );
  const requestScopeChanges = changedFields(
    source,
    target,
    requestScopeFields,
    overrides,
  );
  const observabilityChanges = changedFields(
    source,
    target,
    observabilityFields,
    overrides,
  );
  const modalities = Object.fromEntries(
    runtimeModalities.map((modality) => [
      modality,
      modalityPlan(modality, source, target, ownership, overrides),
    ]),
  ) as Record<RuntimeModality, ModalityReconciliationPlan>;

  return Object.freeze({
    sourceRevision: source.revision,
    targetRevision: target.revision,
    restartRequired: Object.freeze({
      sourceRevision: source.revision,
      targetRevision: target.revision,
      action:
        restartRequiredChanges.length === 0 ? "unchanged" : "restart-required",
      changedFields: restartRequiredChanges,
    }),
    modalities: Object.freeze(modalities),
    requestScope: Object.freeze({
      sourceRevision: source.revision,
      targetRevision: target.revision,
      changedFields: requestScopeChanges,
    }),
    observability: Object.freeze({
      sourceRevision: source.revision,
      targetRevision: target.revision,
      action: observabilityChanges.length === 0 ? "unchanged" : "replace",
      changedFields: observabilityChanges,
    }),
  });
}
