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
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
} from "../../browser-access";
import {
  browserAccessPolicySchema,
  evaluateBrowserAccessPolicy,
} from "../../browser-policy";

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
  const current = await loadBrowserAccessConfig(ctx.config.root);
  const config = await saveBrowserAccessConfig(ctx.config.root, {
    provider: {
      kind: "cloudflare-access",
      teamDomain: input.teamDomain,
      audience: input.audience,
    },
    origin: input.origin,
    permissions: input.permissions,
    ...(current?.policy ? { policy: current.policy } : {}),
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
  const current = await loadBrowserAccessConfig(ctx.config.root);
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
    ...(current?.policy ? { policy: current.policy } : {}),
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
  const policy = config?.policy ?? null;
  execution.output.info(
    policy
      ? `Browser access policy: ${Object.keys(policy.roles).length} roles, ${policy.bindings.length} bindings.`
      : "Browser access policy is not configured.",
  );
  return { data: { policy } };
}

export async function runAccessPolicyApply(
  input: AccessPolicyApplyInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await loadBrowserAccessConfig(ctx.config.root);
  if (!config)
    throw new CliInputError("Configure a browser identity provider first.");
  let source: string;
  try {
    source = await readFile(input.file, "utf8");
  } catch {
    throw new CliInputError(`Unable to read access policy: ${input.file}`);
  }
  let value: unknown;
  try {
    value = JSON.parse(source);
  } catch {
    throw new CliInputError("Access policy must be valid JSON.");
  }
  const parsed = browserAccessPolicySchema.safeParse(value);
  if (!parsed.success)
    throw new CliInputError(
      `Invalid access policy: ${parsed.error.issues.map((issue) => issue.message).join("; ")}`,
    );
  const saved = await saveBrowserAccessConfig(ctx.config.root, {
    ...config,
    policy: parsed.data,
  });
  execution.output.info(
    "Saved browser access policy. Restart LocalBase to apply it.",
  );
  return {
    data: { policy: saved.policy!, restartRequired: true as const },
  };
}

export async function runAccessPolicyTest(
  input: AccessPolicyTestInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await loadBrowserAccessConfig(ctx.config.root);
  if (!config)
    throw new CliInputError("Configure a browser identity provider first.");
  const decision = config.policy
    ? evaluateBrowserAccessPolicy(config.policy, {
        issuer: input.issuer,
        subject: input.subject,
        ...(input.email ? { verifiedEmail: input.email } : {}),
      })
    : { matchedRoles: [], permissions: config.permissions };
  execution.output.info(
    `Matched ${decision.matchedRoles.length} roles and ${decision.permissions.length} permissions.`,
  );
  return {
    data: {
      policyConfigured: Boolean(config.policy),
      matchedRoles: decision.matchedRoles,
      permissions: decision.permissions,
    },
  };
}

export async function runAccessPolicyClear(
  _input: AccessPolicyClearInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const config = await loadBrowserAccessConfig(ctx.config.root);
  if (!config)
    throw new CliInputError("Configure a browser identity provider first.");
  if (!config.policy) {
    execution.output.info("Browser access policy was already clear.");
    return { data: { cleared: false, restartRequired: false } };
  }
  const { policy: _, ...providerWideConfig } = config;
  await saveBrowserAccessConfig(ctx.config.root, providerWideConfig);
  execution.output.info(
    "Cleared browser access policy. Provider-wide permissions apply after restart.",
  );
  return { data: { cleared: true, restartRequired: true } };
}
