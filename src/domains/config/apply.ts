import { z } from "zod";
import { DatabaseSession, databasePath } from "../../db/client";
import { assertInitializedLocalBaseRoot } from "../../utils/root";
import { defaultConfig, readConfigIfPresent } from "../../manager";
import {
  markRestartPending,
  restartPending,
  savedStaticConfiguration,
  staticConfigurationChanged,
} from "./activation";
import { CliInputError } from "../app/commands/errors";
import { withServiceStartHandoff } from "../service/ownership";
import {
  getServiceInspectionReadOnly,
  restartServiceWithinOperation,
  waitForServiceReady,
} from "../service/manager";
import {
  composeConfiguration,
  configurationPlanSchema,
  desiredConfigurationSchema,
  persistConfiguration,
  planConfiguration,
  type DesiredConfiguration,
} from "./declarative";

export const configApplyOptionsSchema = z
  .object({
    restart: z.enum(["auto", "always", "never"]).default("auto"),
    wait: z.boolean().default(false),
  })
  .strict();
export type ConfigApplyOptions = z.infer<typeof configApplyOptionsSchema>;

export const configApplyResultSchema = configurationPlanSchema
  .extend({
    activation: z.enum(["unchanged", "hot", "restart-required", "restarted"]),
    readiness: z.enum(["not-checked", "ready"]),
  })
  .strict();

const lifecycle = {
  inspect: getServiceInspectionReadOnly,
  restart: restartServiceWithinOperation,
  wait: waitForServiceReady,
};

export async function applyConfiguration(
  root: string,
  document: DesiredConfiguration,
  options: ConfigApplyOptions,
  services = lifecycle,
): Promise<z.infer<typeof configApplyResultSchema>> {
  const desired = desiredConfigurationSchema.parse(document);
  const policy = configApplyOptionsSchema.parse(options);
  return await withServiceStartHandoff(root, async (canonical, handoff) => {
    if (await Bun.file(databasePath(canonical)).exists()) {
      assertInitializedLocalBaseRoot(canonical);
      const database = new DatabaseSession();
      try {
        database.get(canonical);
      } finally {
        database.close();
      }
    }
    const current = await readConfigIfPresent(canonical);
    const plan = planConfiguration(
      current,
      desired,
      await restartPending(canonical),
    );
    const next = composeConfiguration(
      current ?? defaultConfig(canonical),
      desired,
    );
    let restart = policy.restart === "always";
    if (
      restart ||
      (plan.restartRequired && policy.restart === "auto") ||
      policy.wait
    ) {
      const inspection = await services.inspect(canonical);
      const state = inspection.service.state;
      const running = state === "running" || state === "starting";
      if (policy.restart === "auto" && plan.restartRequired) {
        if (
          state === "foreground" ||
          state === "unknown" ||
          state === "stopping"
        ) {
          throw new CliInputError(
            "Cannot automatically restart this gateway. Use --restart never to save, then restart its owner explicitly.",
          );
        }
        restart = true;
      }
      if (
        restart &&
        (!inspection.service.managerAvailable ||
          state === "foreground" ||
          state === "unknown" ||
          state === "stopping")
      ) {
        throw new CliInputError(
          "Cannot safely restart this gateway with the service manager.",
        );
      }
      if (
        policy.wait &&
        !restart &&
        (plan.restartRequired || (!running && state !== "foreground"))
      ) {
        throw new CliInputError(
          "--wait requires a running gateway without pending restart changes, or --restart always.",
        );
      }
    }
    if (plan.changed) {
      const database = new DatabaseSession();
      try {
        persistConfiguration(database, next);
      } finally {
        database.close();
      }
    }
    if (restart) {
      const database = new DatabaseSession();
      try {
        markRestartPending(database.get(canonical), next);
      } finally {
        database.close();
      }
    }
    try {
      if (restart) await services.restart(canonical, handoff);
      if (policy.wait) await services.wait(canonical);
    } catch (error) {
      if (restart) {
        const database = new DatabaseSession();
        try {
          const db = database.get(canonical);
          db.transaction(
            () => {
              if (
                !staticConfigurationChanged(savedStaticConfiguration(db), next)
              )
                markRestartPending(db, next);
            },
            { behavior: "immediate" },
          );
        } finally {
          database.close();
        }
      }
      throw new Error(
        `Configuration is saved, but activation failed: ${error instanceof Error ? error.message : String(error)}. Retry local-base restart, or config apply --restart always --wait.`,
      );
    }
    return {
      ...plan,
      pendingRestart: await restartPending(canonical),
      activation: restart
        ? "restarted"
        : plan.restartRequired
          ? "restart-required"
          : plan.changed
            ? "hot"
            : "unchanged",
      readiness: policy.wait ? "ready" : "not-checked",
    };
  });
}
