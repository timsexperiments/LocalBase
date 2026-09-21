import { readFile } from "node:fs/promises";
import type { AppContext } from "../../../../context";
import type { CommandExecution } from "../../../app/commands/framework";
import { CliInputError } from "../../../app/commands/errors";
import type {
  AccessCloudflareInput,
  AccessDisableInput,
  AccessOidcInput,
  AccessPolicyApplyInput,
  AccessPolicyClearInput,
  AccessPolicyShowInput,
  AccessPolicyTestInput,
  AccessShowInput,
} from "../../../app/commands/inputs";
import {
  disableBrowserAccess,
  browserAccessPolicySchema,
  evaluateBrowserAccessPolicy,
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
} from "../../browser-access";

async function configuredBrowserAccess(ctx: AppContext) {
  const config = await loadBrowserAccessConfig(ctx.config.root);
  if (!config)
    throw new CliInputError(
      "Browser access is not configured. Run local-base access cloudflare or local-base access oidc first.",
    );
  return config;
}

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

export async function runAccessPolicyShow(
  _input: AccessPolicyShowInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await loadBrowserAccessConfig(ctx.config.root);
  execution.output.info(
    config?.policy
      ? "Browser access policy is configured."
      : "Browser access uses provider-wide configured permissions.",
  );
  return { data: { policy: config?.policy ?? null } };
}

async function readBrowserAccessPolicy(path: string) {
  let contents: string;
  try {
    contents = await readFile(path, "utf8");
  } catch {
    throw new CliInputError("Unable to read browser access policy file.");
  }
  try {
    return browserAccessPolicySchema.parse(JSON.parse(contents));
  } catch {
    throw new CliInputError("Invalid browser access policy file.");
  }
}

export async function runAccessPolicyApply(
  input: AccessPolicyApplyInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await configuredBrowserAccess(ctx);
  const policy = await readBrowserAccessPolicy(input.file);
  const hasAccessManager = policy.bindings.some((binding) =>
    (policy.roles[binding.role] ?? []).includes("access:manage"),
  );
  if (!hasAccessManager)
    throw new CliInputError(
      "Browser access policy needs a binding to a role with access:manage.",
    );
  const saved = await saveBrowserAccessConfig(ctx.config.root, {
    ...config,
    policy,
  });
  execution.output.info(
    "Saved browser access policy. Restart LocalBase to apply it.",
  );
  return { data: { policy: saved.policy, restartRequired: true as const } };
}

export async function runAccessPolicyTest(
  input: AccessPolicyTestInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await configuredBrowserAccess(ctx);
  const result = evaluateBrowserAccessPolicy({
    config,
    identity: {
      issuer: input.issuer,
      subject: input.subject,
      ...(input.email ? { email: input.email } : {}),
    },
  });
  execution.output.info(
    `Matched ${result.matchedRoles.length} browser access role${result.matchedRoles.length === 1 ? "" : "s"}.`,
  );
  return {
    data: {
      matchedRoles: [...result.matchedRoles],
      permissions: result.permissions,
    },
  };
}

export async function runAccessPolicyClear(
  _input: AccessPolicyClearInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await configuredBrowserAccess(ctx);
  const cleared = Boolean(config.policy);
  if (cleared)
    await saveBrowserAccessConfig(ctx.config.root, {
      ...config,
      policy: undefined,
    });
  execution.output.info(
    "Browser access now uses provider-wide configured permissions. Restart LocalBase to apply it.",
  );
  return {
    data: {
      cleared,
      permissions: config.permissions,
      restartRequired: cleared,
    },
  };
}
