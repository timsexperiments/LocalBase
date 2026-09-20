import { z } from "zod";
import { type DatabaseSession } from "../../db/client";
import { type LocalBaseConfig, saveConfig } from "../../manager";
import { CliInputError, formatZodError } from "../app/commands/errors";
import { modelConfigurationSchema } from "../models/model-selection";
import { memorySafetyConfigSchema } from "../runtime/memory-safety";
import { configFieldOwnership } from "../runtime/reconciliation-plan";
import { parallelSlotsSchema } from "./parallel";
import { hostSchema, portSchema } from "./schema";
import {
  markRestartPending,
  savedStaticConfiguration,
  staticConfigurationChanged,
} from "./activation";
import { ensureLocalBaseRootMarker } from "../../utils/root";

export const desiredConfigurationSchema = z
  .object({
    version: z.literal(1),
    gateway: z.object({ host: hostSchema, port: portSchema }).strict(),
    runtime: z
      .object({
        host: hostSchema,
        port: portSchema,
        ctxSize: z.number().int().min(2048).max(2_147_483_647),
        parallel: parallelSlotsSchema,
        sttHost: hostSchema,
        sttPort: portSchema,
      })
      .strict(),
    models: modelConfigurationSchema,
    memory: memorySafetyConfigSchema,
  })
  .strict();

export type DesiredConfiguration = z.infer<typeof desiredConfigurationSchema>;

export const configurationPlanSchema = z
  .object({
    changed: z.boolean(),
    restartRequired: z.boolean(),
    pendingRestart: z.boolean(),
    changes: z.array(
      z
        .object({
          path: z.string(),
          before: z.json(),
          after: z.json(),
          activation: z.enum(["hot", "restart-required"]),
        })
        .strict(),
    ),
  })
  .strict();
export type ConfigurationPlan = z.infer<typeof configurationPlanSchema>;

function normalize(config: DesiredConfiguration): DesiredConfiguration {
  return {
    ...config,
    models: {
      ...config.models,
      selectedLlmModels: [...config.models.selectedLlmModels].sort(),
      selectedSttModels: [...config.models.selectedSttModels].sort(),
      selectedTtsModels: [...config.models.selectedTtsModels].sort(),
      selectedImageModels: [...config.models.selectedImageModels].sort(),
      selectedVideoModels: [...config.models.selectedVideoModels].sort(),
    },
  };
}

export function parseConfiguration(text: string): DesiredConfiguration {
  let value: unknown;
  try {
    value = Bun.TOML.parse(text);
  } catch {
    // Parser diagnostics can contain source text, including accidentally pasted secrets.
    throw new CliInputError("Invalid TOML configuration.");
  }
  const parsed = desiredConfigurationSchema.safeParse(value);
  if (!parsed.success) throw new CliInputError(formatZodError(parsed.error));
  return normalize(parsed.data);
}

export function configurationDocument(
  config: LocalBaseConfig,
): DesiredConfiguration {
  return normalize(
    desiredConfigurationSchema.parse({
      version: 1,
      gateway: { host: config.gatewayHost, port: config.gatewayPort },
      runtime: {
        host: config.host,
        port: config.port,
        ctxSize: config.ctxSize,
        parallel: config.parallel,
        sttHost: config.sttHost,
        sttPort: config.sttPort,
      },
      models: {
        selectedLlmModels: config.selectedLlmModels,
        selectedSttModels: config.selectedSttModels,
        selectedTtsModels: config.selectedTtsModels,
        selectedImageModels: config.selectedImageModels,
        selectedVideoModels: config.selectedVideoModels,
        activeLlmModel: config.activeLlmModel,
        activeSttModel: config.activeSttModel,
        activeTtsModel: config.activeTtsModel,
        activeImageModel: config.activeImageModel,
        activeVideoModel: config.activeVideoModel,
      },
      memory: config.memory,
    }),
  );
}

export function composeConfiguration(
  current: LocalBaseConfig,
  desired: DesiredConfiguration,
): LocalBaseConfig {
  return {
    ...current,
    ...desired.runtime,
    ...desired.models,
    gatewayHost: desired.gateway.host,
    gatewayPort: desired.gateway.port,
    memory: desired.memory,
  };
}

function activationFor(
  field: keyof LocalBaseConfig,
): "hot" | "restart-required" {
  return configFieldOwnership[field] === "restart-required"
    ? "restart-required"
    : "hot";
}

export function planConfiguration(
  current: LocalBaseConfig | undefined,
  desired: DesiredConfiguration,
  pendingRestart = false,
): ConfigurationPlan {
  const before = current ? configurationDocument(current) : undefined;
  const after = normalize(desired);
  const changes: ConfigurationPlan["changes"] = [];
  function compare(
    path: string,
    left: unknown,
    right: unknown,
    activation: "hot" | "restart-required",
  ) {
    if (right !== null && typeof right === "object" && !Array.isArray(right)) {
      const previous = z.record(z.string(), z.unknown()).parse(left ?? {});
      for (const [key, value] of Object.entries(right))
        compare(`${path}.${key}`, previous[key], value, activation);
    } else if (JSON.stringify(left) !== JSON.stringify(right)) {
      changes.push({
        path,
        before: z.json().parse(left ?? null),
        after: z.json().parse(right),
        activation,
      });
    }
  }
  for (const key of ["host", "port"] as const)
    compare(
      `gateway.${key}`,
      before?.gateway[key],
      after.gateway[key],
      "restart-required",
    );
  for (const field of [
    "host",
    "port",
    "ctxSize",
    "parallel",
    "sttHost",
    "sttPort",
  ] as const) {
    compare(
      `runtime.${field}`,
      before?.runtime[field],
      after.runtime[field],
      activationFor(field),
    );
  }
  compare("models", before?.models, after.models, "hot");
  compare("memory", before?.memory, after.memory, configFieldOwnership.memory);
  changes.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return {
    changed: changes.length > 0,
    pendingRestart,
    restartRequired:
      pendingRestart ||
      changes.some((change) => change.activation === "restart-required"),
    changes,
  };
}

/** Call while holding the root operation lock. */
export function persistConfiguration(
  database: DatabaseSession,
  config: LocalBaseConfig,
): void {
  configurationDocument(config);
  ensureLocalBaseRootMarker(config.root);
  const db = database.get(config.root);
  db.transaction(
    () => {
      const previous = savedStaticConfiguration(db);
      saveConfig(database, config);
      if (staticConfigurationChanged(previous, config))
        markRestartPending(db, config);
    },
    { behavior: "immediate" },
  );
}

export function renderConfiguration(config: DesiredConfiguration): string {
  const lines = [
    "# Secrets and process overrides are omitted and preserved on apply.",
    "version = 1",
  ];
  function table(path: string, fields: object) {
    lines.push("", `[${path}]`);
    for (const [key, value] of Object.entries(fields)) {
      if (value !== null && typeof value === "object" && !Array.isArray(value))
        continue;
      lines.push(`${key} = ${JSON.stringify(value)}`);
    }
    for (const [key, value] of Object.entries(fields)) {
      if (value !== null && typeof value === "object" && !Array.isArray(value))
        table(`${path}.${key}`, value);
    }
  }
  const normalized = normalize(config);
  table("gateway", normalized.gateway);
  table("runtime", normalized.runtime);
  table("models", normalized.models);
  table("memory", normalized.memory);
  return `${lines.join("\n")}\n`;
}
