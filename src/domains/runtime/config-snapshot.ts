import { readConfig, saveConfig, type LocalBaseConfig } from "../../manager";
import type { DatabaseSession } from "../../db/client";
import { canonicalLocalBaseRoot } from "../../utils/root";

type DeepReadonly<Value> = Value extends (...args: never[]) => unknown
  ? Value
  : Value extends ReadonlyArray<infer Item>
    ? readonly DeepReadonly<Item>[]
    : Value extends object
      ? { readonly [Key in keyof Value]: DeepReadonly<Value[Key]> }
      : Value;

type ImmutableLocalBaseConfig = DeepReadonly<LocalBaseConfig>;

export type GatewayListenerIdentity = Readonly<{
  host: string;
  port: number;
}>;

export type RuntimeProcessSettings = Readonly<{
  root: string;
  gateway: GatewayListenerIdentity;
}>;

export type RuntimeConfigSnapshot = Readonly<{
  revision: number;
  config: ImmutableLocalBaseConfig;
}>;

function copyConfig(
  config: ImmutableLocalBaseConfig | LocalBaseConfig,
): LocalBaseConfig {
  const cloned = structuredClone(config);
  return {
    ...cloned,
    selectedLlmModels: [...cloned.selectedLlmModels],
    selectedSttModels: [...cloned.selectedSttModels],
    selectedImageModels: [...cloned.selectedImageModels],
  };
}

function deepFreeze<Value>(value: Value, seen = new WeakSet<object>()): Value {
  if (!value || typeof value !== "object" || seen.has(value)) return value;
  seen.add(value);
  for (const nested of Object.values(value)) deepFreeze(nested, seen);
  return Object.freeze(value);
}

function immutableConfig(config: LocalBaseConfig): ImmutableLocalBaseConfig {
  return deepFreeze(copyConfig(config));
}

function normalizedValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(normalizedValue);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, nested]) => [key, normalizedValue(nested)]),
    );
  }
  return value;
}

function configFingerprint(
  config: ImmutableLocalBaseConfig | LocalBaseConfig,
): string {
  return JSON.stringify(normalizedValue(config));
}

export function runtimeProcessSettings(
  root: string,
  gateway: GatewayListenerIdentity,
): RuntimeProcessSettings {
  return Object.freeze({
    root: canonicalLocalBaseRoot(root),
    gateway: Object.freeze({ ...gateway }),
  });
}

/** Owns persisted runtime configuration independently from process startup settings. */
export class RuntimeConfigController {
  private snapshot: RuntimeConfigSnapshot;

  constructor(
    private readonly database: DatabaseSession,
    root: string,
    config: LocalBaseConfig,
  ) {
    this.root = canonicalLocalBaseRoot(root);
    if (canonicalLocalBaseRoot(config.root) !== this.root) {
      throw new Error("Runtime configuration must use the process root.");
    }
    this.snapshot = Object.freeze({
      revision: 0,
      config: immutableConfig(config),
    });
  }

  readonly root: string;

  read(): RuntimeConfigSnapshot {
    return this.snapshot;
  }

  copy(): LocalBaseConfig {
    return copyConfig(this.snapshot.config);
  }

  async refresh(): Promise<RuntimeConfigSnapshot> {
    return this.replace(await readConfig(this.root));
  }

  persist(config: LocalBaseConfig): RuntimeConfigSnapshot {
    const next = copyConfig(config);
    if (canonicalLocalBaseRoot(next.root) !== this.root) {
      throw new Error("Runtime configuration cannot change the process root.");
    }
    saveConfig(this.database, next);
    return this.replace(next);
  }

  update(
    updateConfig: (config: LocalBaseConfig) => LocalBaseConfig | void,
  ): RuntimeConfigSnapshot {
    const next = copyConfig(this.snapshot.config);
    const updated = updateConfig(next) ?? next;
    return this.persist(updated);
  }

  private replace(config: LocalBaseConfig): RuntimeConfigSnapshot {
    if (canonicalLocalBaseRoot(config.root) !== this.root) {
      throw new Error("Runtime configuration cannot change the process root.");
    }
    if (configFingerprint(this.snapshot.config) === configFingerprint(config)) {
      return this.snapshot;
    }
    this.snapshot = Object.freeze({
      revision: this.snapshot.revision + 1,
      config: immutableConfig(config),
    });
    return this.snapshot;
  }
}
