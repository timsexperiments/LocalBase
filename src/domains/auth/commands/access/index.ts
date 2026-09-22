import { readFile } from "node:fs/promises";
import type { AppContext } from "../../../../context";
import type { CommandExecution } from "../../../app/commands/framework";
import { CliInputError } from "../../../app/commands/errors";
import type {
  AccessCloudflareInput,
  AccessDisableInput,
  AccessGithubAddInput,
  AccessGithubListInput,
  AccessGithubRemoveInput,
  AccessOidcAddInput,
  AccessOidcListInput,
  AccessOidcRemoveInput,
  AccessPolicyApplyInput,
  AccessPolicyClearInput,
  AccessPolicyShowInput,
  AccessPolicyTestInput,
  AccessShowInput,
  AccessUsersDisableInput,
  AccessUsersEnableInput,
  AccessUsersInviteInput,
  AccessUsersListInput,
  AccessUsersRemoveInput,
  AccessUsersRolesInput,
} from "../../../app/commands/inputs";
import {
  disableBrowserAccess,
  defaultBrowserPermissions,
  loadBrowserAccessConfig,
  removeAccessRegistration,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
  upsertAccessRegistration,
} from "../../browser-access";
import {
  accessControlConfigSchema,
  applyAccessControl,
  clearAccessControl,
  loadAccessControl,
  resolveAccessControl,
} from "../../access-control";
import {
  disableManagedUser,
  enableManagedUser,
  inviteManagedUser,
  listManagedUsers,
  removeManagedUser,
  replaceManagedUserRoles,
} from "../../users";
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
        upsertAccessRegistration(current, {
          registration: {
            kind: "oidc",
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
    summary?.provider.kind === "direct"
      ? summary.provider.registrations.filter(
          (registration) => registration.kind === "oidc",
        )
      : [];
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
      const registration =
        current?.provider.kind === "direct"
          ? current.provider.registrations.find(
              (candidate) => candidate.id === input.id,
            )
          : undefined;
      if (registration?.kind !== "oidc")
        throw new CliInputError(
          `OpenID Connect registration ${input.id} not found.`,
        );
      const removal = removeAccessRegistration(current, input.id);
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

export async function runAccessGithubAdd(
  input: AccessGithubAddInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const clientSecret = process.env[input.clientSecretEnv] ?? "";
  if (!clientSecret)
    throw new CliInputError(
      `GitHub client secret environment variable ${input.clientSecretEnv} is empty or unavailable.`,
    );
  const config = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const current = await loadBrowserAccessConfig(root);
      return await saveBrowserAccessConfig(
        root,
        upsertAccessRegistration(current, {
          registration: {
            kind: "github-oauth",
            id: input.id,
            name: input.name,
            clientId: input.clientId,
            clientSecret,
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
    `Saved GitHub OAuth registration ${input.id}. Restart LocalBase to apply it.`,
  );
  return {
    data: {
      config: summarizeBrowserAccessConfig(config),
      restartRequired: true as const,
    },
  };
}

export async function runAccessGithubList(
  _input: AccessGithubListInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const current = await loadBrowserAccessConfig(ctx.config.root);
  const summary = current ? summarizeBrowserAccessConfig(current) : null;
  const registrations =
    summary?.provider.kind === "direct"
      ? summary.provider.registrations.filter(
          (registration) => registration.kind === "github-oauth",
        )
      : [];
  execution.output.info(`${registrations.length} GitHub OAuth registrations.`);
  return { data: { registrations } };
}

export async function runAccessGithubRemove(
  input: AccessGithubRemoveInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const result = await withRootOperation(
    ctx.config.root,
    "configure browser access",
    async (root) => {
      const current = await loadBrowserAccessConfig(root);
      const registration =
        current?.provider.kind === "direct"
          ? current.provider.registrations.find(
              (candidate) => candidate.id === input.id,
            )
          : undefined;
      if (registration?.kind !== "github-oauth")
        throw new CliInputError(
          `GitHub OAuth registration ${input.id} not found.`,
        );
      const removal = removeAccessRegistration(current, input.id);
      if (removal.kind === "not-found")
        throw new CliInputError(
          `GitHub OAuth registration ${input.id} not found.`,
        );
      if (removal.kind === "disabled") {
        await disableBrowserAccess(root);
        return null;
      }
      return await saveBrowserAccessConfig(root, removal.config);
    },
  );
  execution.output.info(
    result
      ? `Removed GitHub OAuth registration ${input.id}. Restart LocalBase to apply it.`
      : `Removed ${input.id} and disabled browser access. Restart LocalBase to apply it.`,
  );
  return {
    data: {
      removed: true as const,
      config: result ? summarizeBrowserAccessConfig(result) : null,
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
  const policy = loadAccessControl(ctx.database.get(ctx.config.root));
  execution.output.info(
    policy
      ? `Browser access policy: ${policy.roles.length} roles, ${policy.bindings.length} bindings.`
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
  const parsed = accessControlConfigSchema.safeParse(value);
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
      return applyAccessControl(ctx.database.get(root), parsed.data);
    },
  );
  execution.output.info(
    "Saved browser access policy. Changes apply immediately.",
  );
  return {
    data: { policy: saved, restartRequired: false as const },
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
  const decision = resolveAccessControl(ctx.database.get(ctx.config.root), {
    issuer: input.issuer,
    subject: input.subject,
    ...(input.email ? { verifiedEmail: input.email } : {}),
  }) ?? { matchedRoles: [], permissions: config.permissions };
  execution.output.info(
    `Matched ${decision.matchedRoles.length} roles and ${decision.permissions.length} permissions.`,
  );
  return {
    data: {
      policyConfigured:
        loadAccessControl(ctx.database.get(ctx.config.root)) !== null,
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
      return clearAccessControl(ctx.database.get(root));
    },
  );
  if (!cleared) {
    execution.output.info("Browser access policy was already clear.");
    return { data: { cleared: false, restartRequired: false } };
  }
  execution.output.info(
    "Cleared browser access policy. Provider-wide permissions now apply.",
  );
  return { data: { cleared: true, restartRequired: false } };
}

export async function runAccessUsersList(
  _input: AccessUsersListInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const users = listManagedUsers(ctx.database.get(ctx.config.root));
  execution.output.info(`${users.length} managed users.`);
  return { data: { users } };
}

export async function runAccessUsersInvite(
  input: AccessUsersInviteInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const result = await withRootOperation(
    ctx.config.root,
    "provision a browser user",
    async (root) => {
      const config = await loadBrowserAccessConfig(root);
      if (!config)
        throw new CliInputError("Configure a browser identity provider first.");
      const user = inviteManagedUser(ctx.database.get(root), input);
      return { user, signInUrl: new URL("/app", config.origin).toString() };
    },
  );
  execution.output.info(
    `Invited ${result.user.email}. Sign in at ${result.signInUrl}`,
  );
  return { data: result };
}

export async function runAccessUsersRoles(
  input: AccessUsersRolesInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const user = await withRootOperation(
    ctx.config.root,
    "replace browser user roles",
    async (root) => replaceManagedUserRoles(ctx.database.get(root), input),
  );
  execution.output.info(`Replaced roles for ${user.email}.`);
  return { data: { user } };
}

export async function runAccessUsersEnable(
  input: AccessUsersEnableInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const user = await withRootOperation(
    ctx.config.root,
    "enable a browser user",
    async (root) => enableManagedUser(ctx.database.get(root), input),
  );
  execution.output.info(`Enabled ${user.email}.`);
  return { data: { user } };
}

export async function runAccessUsersDisable(
  input: AccessUsersDisableInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const user = await withRootOperation(
    ctx.config.root,
    "disable a browser user",
    async (root) => disableManagedUser(ctx.database.get(root), input),
  );
  execution.output.info(`Disabled ${user.email}.`);
  return { data: { user } };
}

export async function runAccessUsersRemove(
  input: AccessUsersRemoveInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const user = await withRootOperation(
    ctx.config.root,
    "remove a browser user",
    async (root) => removeManagedUser(ctx.database.get(root), input),
  );
  execution.output.info(`Removed ${user.email}.`);
  return { data: { user } };
}
