import type { AppContext } from "../../../../context";
import type { CommandExecution } from "../../../app/commands/framework";
import { CliInputError } from "../../../app/commands/errors";
import type {
  AccessCloudflareInput,
  AccessDisableInput,
  AccessOidcInput,
  AccessShowInput,
} from "../../../app/commands/inputs";
import {
  disableBrowserAccess,
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
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
  return {
    data: { config: config ? summarizeBrowserAccessConfig(config) : null },
  };
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
  return {
    data: {
      config: summarizeBrowserAccessConfig(config),
      restartRequired: true as const,
    },
  };
}

export async function runAccessOidc(
  input: AccessOidcInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const clientAuthentication = input.publicClient
    ? ({ kind: "none" } as const)
    : ({
        kind: "client-secret-basic",
        clientSecret: process.env[input.clientSecretEnv ?? ""] ?? "",
      } as const);
  if (
    clientAuthentication.kind === "client-secret-basic" &&
    !clientAuthentication.clientSecret
  ) {
    throw new CliInputError(
      `OpenID Connect client secret environment variable ${input.clientSecretEnv} is empty or unavailable.`,
    );
  }
  const config = await saveBrowserAccessConfig(ctx.config.root, {
    provider: {
      kind: "oidc",
      issuer: input.issuer,
      clientId: input.clientId,
      clientAuthentication,
    },
    origin: input.origin,
    permissions: input.permissions,
  });
  execution.output.info(
    "Saved OpenID Connect configuration. Restart LocalBase to apply it.",
  );
  return {
    data: {
      config: summarizeBrowserAccessConfig(config),
      restartRequired: true as const,
    },
  };
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
