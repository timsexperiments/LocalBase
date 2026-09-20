import type { AppContext } from "../../../../context";
import type { CommandExecution } from "../../../app/commands/framework";
import type {
  AccessCloudflareInput,
  AccessDisableInput,
  AccessShowInput,
} from "../../../app/commands/inputs";
import {
  disableBrowserAccess,
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
} from "../../browser-access";

export async function runAccessShow(
  _input: AccessShowInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await loadBrowserAccessConfig(ctx.config.root);
  execution.output.info(
    config
      ? `Browser access: ${config.provider.kind} for ${config.origin}`
      : "Browser access is not configured.",
  );
  return { data: { config } };
}

export async function runAccessCloudflare(
  input: AccessCloudflareInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await saveBrowserAccessConfig(ctx.config.root, {
    provider: {
      kind: "cloudflare-access",
      teamDomain: input.teamDomain,
      audience: input.audience,
    },
    origin: input.origin,
    permissions: input.permissions,
  });
  execution.output.info(
    "Saved Cloudflare Access configuration. Restart LocalBase to apply it.",
  );
  return { data: { config, restartRequired: true as const } };
}

export async function runAccessDisable(
  _input: AccessDisableInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const disabled = await disableBrowserAccess(ctx.config.root);
  execution.output.info(
    `${disabled ? "Disabled browser access." : "Browser access was already disabled."} Restart LocalBase to apply it.`,
  );
  return { data: { disabled, restartRequired: disabled } };
}
