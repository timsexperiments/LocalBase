import { readFile } from "node:fs/promises";
import type { AppContext } from "../../../../context";
import type { CommandExecution } from "../../../app/commands/framework";
import { CliInputError } from "../../../app/commands/errors";
import type {
  AccessCloudflareInput,
  AccessDisableInput,
  AccessOidcAddInput,
  AccessOidcListInput,
  AccessOidcRemoveInput,
  AccessPolicyApplyInput,
  AccessPolicyClearInput,
  AccessPolicyShowInput,
  AccessPolicyTestInput,
  AccessShowInput,
} from "../../../app/commands/inputs";
import {
  disableBrowserAccess,
  defaultBrowserPermissions,
  loadBrowserAccessConfig,
  removeOidcRegistration,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
  upsertOidcRegistration,
} from "../../browser-access";
import {
  browserAccessPolicySchema,
  evaluateBrowserAccessPolicy,
} from "../../browser-policy";
import { withRootOperation } from "../../../service/ownership";

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
  const config = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const current = await loadBrowserAccessConfig(root);
      return await saveBrowserAccessConfig(root, {
        provider: {
          kind: "cloudflare-access",
          teamDomain: input.teamDomain,
          audience: input.audience,
        },
        origin: input.origin,
        permissions:
          input.permissions ??
          current?.permissions ??
          defaultBrowserPermissions,
        ...(current?.policy ? { policy: current.policy } : {}),
      });
    },
  );
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

export async function runAccessOidcAdd(
  input: AccessOidcAddInput,
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
  const config = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const current = await loadBrowserAccessConfig(root);
      return await saveBrowserAccessConfig(
        root,
        upsertOidcRegistration(current, {
          registration: {
            id: input.id,
            name: input.name,
            issuer: input.issuer,
            clientId: input.clientId,
            clientAuthentication,
          },
          origin: input.origin,
          permissions:
            input.permissions ??
            current?.permissions ??
            defaultBrowserPermissions,
        }),
      );
    },
  );
  execution.output.info(
    `Saved OpenID Connect registration ${input.id}. Restart LocalBase to apply it.`,
  );
  return {
    data: {
      config: summarizeBrowserAccessConfig(config),
      restartRequired: true as const,
    },
  };
}

export async function runAccessOidcList(
  _input: AccessOidcListInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const current = await loadBrowserAccessConfig(ctx.config.root);
  const summary = current ? summarizeBrowserAccessConfig(current) : null;
  const registrations =
    summary?.provider.kind === "oidc" ? summary.provider.registrations : [];
  execution.output.info(`${registrations.length} OIDC registrations.`);
  return { data: { registrations } };
}

export async function runAccessOidcRemove(
  input: AccessOidcRemoveInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const result = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const current = await loadBrowserAccessConfig(root);
      const removal = removeOidcRegistration(current, input.id);
      if (removal.kind === "not-found")
        throw new CliInputError(
          `OpenID Connect registration ${input.id} not found.`,
        );
      if (removal.kind === "disabled") {
        await disableBrowserAccess(root);
        return null;
      }
      return await saveBrowserAccessConfig(root, removal.config);
    },
  );
  if (!result) {
    execution.output.info(
      `Removed ${input.id} and disabled browser access. Restart LocalBase to apply it.`,
    );
    return {
      data: {
        removed: true as const,
        config: null,
        restartRequired: true as const,
      },
    };
  }
  execution.output.info(
    `Removed OpenID Connect registration ${input.id}. Restart LocalBase to apply it.`,
  );
  return {
    data: {
      removed: true as const,
      config: summarizeBrowserAccessConfig(result),
      restartRequired: true as const,
    },
  };
}

export async function runAccessDisable(
  _input: AccessDisableInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const disabled = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    disableBrowserAccess,
  );
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
  const saved = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const config = await loadBrowserAccessConfig(root);
      if (!config)
        throw new CliInputError("Configure a browser identity provider first.");
      return await saveBrowserAccessConfig(root, {
        ...config,
        policy: parsed.data,
      });
    },
  );
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
  const cleared = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const config = await loadBrowserAccessConfig(root);
      if (!config)
        throw new CliInputError("Configure a browser identity provider first.");
      if (!config.policy) return false;
      const { policy: _, ...providerWideConfig } = config;
      await saveBrowserAccessConfig(root, providerWideConfig);
      return true;
    },
  );
  if (!cleared) {
    execution.output.info("Browser access policy was already clear.");
    return { data: { cleared: false, restartRequired: false } };
  }
  execution.output.info(
    "Cleared browser access policy. Provider-wide permissions apply after restart.",
  );
  return { data: { cleared: true, restartRequired: true } };
}
