import { restartPending } from "../activation";
import { z } from "zod";
import type { MinimalAppContext } from "../../../context";
import { readConfig, readConfigIfPresent } from "../../../manager";
import type { CommandExecution } from "../../app/commands/framework";
import { applyConfiguration, configApplyOptionsSchema } from "../apply";
import {
  configurationDocument,
  parseConfiguration,
  planConfiguration,
  renderConfiguration,
  type ConfigurationPlan,
} from "../declarative";

export const configFileInputSchema = z
  .object({ file: z.string().min(1) })
  .strict();
export const configPlanInputSchema = configFileInputSchema
  .extend({ detailedExitCode: z.boolean().default(false) })
  .strict();
export const configApplyInputSchema = configFileInputSchema
  .extend(configApplyOptionsSchema.shape)
  .strict();

async function readDocument(file: string) {
  return parseConfiguration(
    await (file === "-" ? Bun.stdin.text() : Bun.file(file).text()),
  );
}

function printPlan(plan: ConfigurationPlan, execution: CommandExecution): void {
  if (!plan.changed) execution.output.info("No configuration changes.");
  if (plan.pendingRestart)
    execution.output.info(
      "Pending restart: saved static settings await managed startup.",
    );
  for (const change of plan.changes) {
    execution.output.info(
      `${change.path}: ${JSON.stringify(change.before)} -> ${JSON.stringify(change.after)} [${change.activation}]`,
    );
  }
}

export async function runConfigValidate(
  input: z.infer<typeof configFileInputSchema>,
  _ctx: MinimalAppContext,
  execution: CommandExecution,
) {
  const configuration = await readDocument(input.file);
  execution.output.info("Configuration is valid (version 1).");
  return { data: { valid: true, configuration } };
}

export async function runConfigPlan(
  input: z.infer<typeof configPlanInputSchema>,
  ctx: MinimalAppContext,
  execution: CommandExecution,
) {
  const desired = await readDocument(input.file);
  const plan = planConfiguration(
    await readConfigIfPresent(ctx.config.root),
    desired,
    await restartPending(ctx.config.root),
  );
  printPlan(plan, execution);
  return {
    data: plan,
    exitCode:
      input.detailedExitCode && (plan.changed || plan.pendingRestart) ? 2 : 0,
  };
}

export async function runConfigApply(
  input: z.infer<typeof configApplyInputSchema>,
  ctx: MinimalAppContext,
  execution: CommandExecution,
) {
  const desired = await readDocument(input.file);
  const result = await applyConfiguration(ctx.config.root, desired, {
    restart: input.restart,
    wait: input.wait,
  });
  printPlan(result, execution);
  execution.output.info(
    `Activation: ${result.activation}. Readiness: ${result.readiness}.`,
  );
  return { data: result };
}

export async function runConfigShow(
  _input: object,
  ctx: MinimalAppContext,
  execution: CommandExecution,
) {
  const document = renderConfiguration(
    configurationDocument(await readConfig(ctx.config.root)),
  );
  const pendingRestart = await restartPending(ctx.config.root);
  if (!execution.global.json) {
    execution.output.info(document.trimEnd());
    if (pendingRestart)
      execution.output.error(
        "Pending restart: saved static settings await managed startup.",
      );
  }
  return { data: { document, pendingRestart } };
}
