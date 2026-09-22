import { useEffect, useRef, useState, type FormEvent } from "react";
import { z } from "zod";
import {
  permissionSchema,
  type Permission,
} from "../domains/auth/authorization";
import {
  accessControlConfigSchema,
  accessControlRoleSchema,
} from "../domains/auth/access-control";
import { defaultBrowserPermissions } from "../domains/auth/browser-access-contract";
import {
  accessManagementMutationResponseSchema,
  accessManagementReadResponseSchema,
  keyManagementMutationResponseSchema,
  keyManagementReadResponseSchema,
} from "../domains/auth/management-contract";
import { api, type Connection } from "./client";

type AccessConfig = z.infer<
  typeof accessManagementReadResponseSchema
>["config"];
type AccessPolicy = z.infer<
  typeof accessManagementReadResponseSchema
>["policy"];
type ManagedUser = z.infer<
  typeof accessManagementReadResponseSchema
>["users"][number];
type AccessRole = z.infer<
  typeof accessManagementReadResponseSchema
>["roles"][number];
type EmailDelivery = z.infer<
  typeof accessManagementReadResponseSchema
>["emailDelivery"];
type ApiKey = z.infer<typeof keyManagementReadResponseSchema>["keys"][number];

const permissionGroups = [
  [
    "Inference",
    permissionSchema.options.filter((item) => item.startsWith("inference:")),
  ],
  [
    "Models",
    permissionSchema.options.filter((item) => item.startsWith("models:")),
  ],
  [
    "Configuration",
    permissionSchema.options.filter((item) =>
      item.startsWith("configuration:"),
    ),
  ],
  [
    "API keys",
    permissionSchema.options.filter((item) => item.startsWith("keys:")),
  ],
  [
    "Browser access",
    permissionSchema.options.filter((item) => item.startsWith("access:")),
  ],
  [
    "Sessions",
    permissionSchema.options.filter((item) => item.startsWith("sessions:")),
  ],
  [
    "System",
    permissionSchema.options.filter((item) => item.startsWith("system:")),
  ],
] as const;

function PermissionPicker({
  value,
  onChange,
  disabled,
}: {
  value: readonly Permission[];
  onChange: (permissions: Permission[]) => void;
  disabled?: boolean;
}) {
  return (
    <div className="permission-groups">
      {permissionGroups.map(([label, permissions]) => (
        <fieldset key={label}>
          <legend>{label}</legend>
          {permissions.map((permission) => (
            <label className="permission-option" key={permission}>
              <input
                type="checkbox"
                disabled={disabled}
                checked={value.includes(permission)}
                onChange={(event) =>
                  onChange(
                    permissionSchema.options.filter((candidate) =>
                      candidate === permission
                        ? event.target.checked
                        : value.includes(candidate),
                    ),
                  )
                }
              />
              <span>{permission}</span>
            </label>
          ))}
        </fieldset>
      ))}
    </div>
  );
}

function providerLabel(config: AccessConfig) {
  if (!config) return "Not configured";
  return config.provider.kind === "cloudflare-access"
    ? `Cloudflare Access · ${config.provider.teamDomain}`
    : `Direct sign-in · ${config.provider.registrations.length} ${config.provider.registrations.length === 1 ? "registration" : "registrations"}`;
}

export function apiKeyStatus(
  key: Pick<ApiKey, "expiresAt" | "revokedAt">,
  now = Date.now(),
): "Active" | "Expired" | "Revoked" {
  if (key.revokedAt) return "Revoked";
  if (!key.expiresAt) return "Active";
  const expiresAt = Date.parse(key.expiresAt);
  return Number.isFinite(expiresAt) && expiresAt > now ? "Active" : "Expired";
}

export function starterAccessPolicy(email: string) {
  return accessControlConfigSchema.parse({
    roles: [
      {
        name: "admin",
        description: "Full LocalBase administration",
        permissions: permissionSchema.options,
      },
      {
        name: "member",
        description: "Chat and model discovery",
        permissions: ["inference:chat", "models:read"],
      },
    ],
    bindings: [{ kind: "email", role: "admin", email }],
    defaultRole: "member",
  });
}

export function reconcileInviteRoles(
  roles: readonly AccessRole[],
  selected: readonly string[],
  defaultRole: string | null,
): string[] {
  const available = new Set(roles.map((role) => role.name));
  const retained = selected.filter((role) => available.has(role));
  if (retained.length) return retained;
  const fallback = defaultRole ?? roles[0]?.name;
  return fallback ? [fallback] : [];
}

export function isCurrentManagedUser(
  userEmail: string,
  verifiedEmail: string | undefined,
): boolean {
  return verifiedEmail?.toLowerCase() === userEmail.toLowerCase();
}

export function assignedRolesGrantAccessManagement(
  assigned: readonly string[],
  roles: readonly AccessRole[],
): boolean {
  return assigned.some((name) =>
    roles
      .find((role) => role.name === name)
      ?.permissions.includes("access:manage"),
  );
}

export function AuthManagement({
  connection,
}: {
  connection: Connection | null;
}) {
  const [section, setSection] = useState<"people" | "access" | "keys">(
    "people",
  );
  const [access, setAccess] = useState<AccessConfig>(null);
  const [accessPolicy, setAccessPolicy] = useState<AccessPolicy>(null);
  const [users, setUsers] = useState<ManagedUser[]>([]);
  const [roles, setRoles] = useState<AccessRole[]>([]);
  const [accessLoadState, setAccessLoadState] = useState<
    "loading" | "loaded" | "error"
  >("loading");
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [accessError, setAccessError] = useState("");
  const [keysError, setKeysError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [secret, setSecret] = useState("");
  const [confirming, setConfirming] = useState("");
  const [provider, setProvider] = useState<
    "cloudflare-access" | "oidc" | "github-oauth" | "magic-link"
  >("cloudflare-access");
  const [origin, setOrigin] = useState("");
  const [teamDomain, setTeamDomain] = useState("");
  const [audience, setAudience] = useState("");
  const [registrationId, setRegistrationId] = useState("");
  const [registrationName, setRegistrationName] = useState("");
  const [issuer, setIssuer] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [publicClient, setPublicClient] = useState(false);
  const [accessPermissions, setAccessPermissions] = useState<Permission[]>([
    ...defaultBrowserPermissions,
  ]);
  const [policy, setPolicy] = useState("");
  const [policyConfigured, setPolicyConfigured] = useState(false);
  const [policyRevision, setPolicyRevision] = useState<string | null>(null);
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRoles, setInviteRoles] = useState<string[]>([]);
  const [sendInviteEmail, setSendInviteEmail] = useState(false);
  const [inviteUrl, setInviteUrl] = useState("");
  const [emailDelivery, setEmailDelivery] = useState<EmailDelivery>(null);
  const [smtpHost, setSmtpHost] = useState("");
  const [smtpPort, setSmtpPort] = useState("587");
  const [smtpSecurity, setSmtpSecurity] = useState<"tls" | "starttls">(
    "starttls",
  );
  const [smtpFrom, setSmtpFrom] = useState("");
  const [smtpPasswordAuth, setSmtpPasswordAuth] = useState(false);
  const [smtpUsername, setSmtpUsername] = useState("");
  const [smtpPassword, setSmtpPassword] = useState("");
  const [smtpTestRecipient, setSmtpTestRecipient] = useState("");
  const [roleName, setRoleName] = useState("");
  const [roleDescription, setRoleDescription] = useState("");
  const [rolePermissions, setRolePermissions] = useState<Permission[]>([
    "inference:chat",
    "models:read",
  ]);
  const [keyName, setKeyName] = useState("");
  const [keyExpiry, setKeyExpiry] = useState("");
  const [keyPermissions, setKeyPermissions] = useState<Permission[]>([
    "inference:chat",
    "models:read",
  ]);

  function hydrate(config: AccessConfig) {
    setAccess(config);
    if (!config) return;
    setOrigin(config.origin);
    setAccessPermissions([...config.permissions]);
    if (config.provider.kind === "cloudflare-access") {
      setProvider(config.provider.kind);
      setTeamDomain(config.provider.teamDomain);
      setAudience(config.provider.audience);
    } else {
      editRegistration(config.provider.registrations[0]);
    }
  }

  function editRegistration(
    registration:
      | Extract<
          NonNullable<AccessConfig>["provider"],
          { kind: "direct" }
        >["registrations"][number]
      | undefined,
  ) {
    if (registration) setProvider(registration.kind);
    setRegistrationId(registration?.id ?? "");
    setRegistrationName(registration?.name ?? "");
    setIssuer(registration?.kind === "oidc" ? registration.issuer : "");
    setClientId(
      registration?.kind === "oidc" || registration?.kind === "github-oauth"
        ? registration.clientId
        : "",
    );
    setPublicClient(
      registration?.kind === "oidc" &&
        registration.clientAuthentication === "none",
    );
    setClientSecret("");
  }

  async function load(signal?: AbortSignal) {
    if (!connection) return;
    setConfirming("");
    const [accessResult, keyResult] = await Promise.allSettled([
      api("/_localbase/access-management", connection, { signal })
        .then((response) => response.json())
        .then((value) => accessManagementReadResponseSchema.parse(value)),
      api("/_localbase/api-keys", connection, { signal })
        .then((response) => response.json())
        .then((value) => keyManagementReadResponseSchema.parse(value)),
    ]);
    if (signal?.aborted) return;
    if (accessResult.status === "fulfilled") {
      hydrate(accessResult.value.config);
      setAccessPolicy(accessResult.value.policy);
      setUsers(accessResult.value.users);
      setRoles(accessResult.value.roles);
      setPolicy(
        accessResult.value.policy
          ? JSON.stringify(accessResult.value.policy, null, 2)
          : "",
      );
      setPolicyConfigured(accessResult.value.policy !== null);
      setPolicyRevision(accessResult.value.policyRevision);
      setEmailDelivery(accessResult.value.emailDelivery);
      if (accessResult.value.emailDelivery) {
        setSmtpHost(accessResult.value.emailDelivery.host);
        setSmtpPort(String(accessResult.value.emailDelivery.port));
        setSmtpSecurity(accessResult.value.emailDelivery.security);
        setSmtpFrom(accessResult.value.emailDelivery.from);
        setSmtpPasswordAuth(
          accessResult.value.emailDelivery.authentication === "password",
        );
      } else {
        setSmtpHost("");
        setSmtpPort("587");
        setSmtpSecurity("starttls");
        setSmtpFrom("");
        setSmtpPasswordAuth(false);
        setSmtpUsername("");
        setSmtpPassword("");
      }
      setInviteRoles((selected) =>
        reconcileInviteRoles(
          accessResult.value.roles,
          selected,
          accessResult.value.policy?.defaultRole ?? null,
        ),
      );
      setAccessLoadState("loaded");
      setAccessError("");
    } else {
      setAccessLoadState("error");
      setAccessError(
        accessResult.reason instanceof Error
          ? accessResult.reason.message
          : "Could not load browser access.",
      );
    }
    if (keyResult.status === "fulfilled") {
      setKeys(keyResult.value.keys);
      setKeysError("");
    } else {
      setKeysError(
        keyResult.reason instanceof Error
          ? keyResult.reason.message
          : "Could not load API keys.",
      );
    }
  }

  useEffect(() => {
    if (!connection) return;
    const abort = new AbortController();
    void load(abort.signal);
    return () => abort.abort();
  }, [connection?.kind]);

  async function post(path: string, value: unknown) {
    if (!connection) throw new Error("Sign in to manage LocalBase.");
    return await api(path, connection, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(value),
    });
  }

  async function saveProvider(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action:
          provider === "cloudflare-access"
            ? "configure-cloudflare"
            : provider === "oidc"
              ? "upsert-oidc"
              : provider === "github-oauth"
                ? "upsert-github"
                : "upsert-magic-link",
        ...(provider === "cloudflare-access"
          ? {
              provider: { kind: provider, teamDomain, audience },
            }
          : provider === "oidc"
            ? {
                registration: {
                  kind: provider,
                  id: registrationId,
                  name: registrationName,
                  issuer,
                  clientId,
                  clientAuthentication: publicClient
                    ? { kind: "none" as const }
                    : {
                        kind: "client-secret-basic" as const,
                        clientSecret,
                      },
                },
              }
            : provider === "github-oauth"
              ? {
                  registration: {
                    kind: provider,
                    id: registrationId,
                    name: registrationName,
                    clientId,
                    clientSecret,
                  },
                }
              : {
                  registration: {
                    kind: provider,
                    id: registrationId,
                    name: registrationName,
                  },
                }),
        origin,
        permissions: accessPermissions,
      });
      const result = accessManagementMutationResponseSchema.parse(
        await response.json(),
      );
      if ("config" in result) hydrate(result.config);
      setClientSecret("");
      setNotice("Saved. Restart LocalBase to apply browser access changes.");
    } catch (error) {
      setAccessError(
        error instanceof Error
          ? error.message
          : "Browser access update failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function removeRegistration(id: string) {
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action: "remove-registration",
        registrationId: id,
      });
      const result = accessManagementMutationResponseSchema.parse(
        await response.json(),
      );
      if (!("removedRegistrationId" in result))
        throw new Error("The registration was not removed.");
      hydrate(result.config);
      if (!result.config) editRegistration(undefined);
      setConfirming("");
      setNotice(
        result.config
          ? "Registration removed. Restart LocalBase to apply the change."
          : "Last registration removed. Browser access will be disabled after restart.",
      );
    } catch (error) {
      setAccessError(
        error instanceof Error
          ? error.message
          : "Could not remove the registration.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function savePolicy() {
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const parsed = accessControlConfigSchema.parse(JSON.parse(policy));
      await applyPolicy(parsed);
      setNotice("Policy saved. The change is active now.");
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Access policy update failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function applyPolicy(next: NonNullable<AccessPolicy>) {
    const response = await post("/_localbase/access-management", {
      action: "apply-policy",
      policy: next,
      expectedPolicyRevision: policyRevision,
    });
    accessManagementMutationResponseSchema.parse(await response.json());
  }

  async function inviteUser(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action: "invite-user",
        email: inviteEmail,
        roles: inviteRoles,
        sendEmail: sendInviteEmail,
      });
      const result = accessManagementMutationResponseSchema.parse(
        await response.json(),
      );
      if (!("user" in result) || !("signInUrl" in result))
        throw new Error("The invitation response was incomplete.");
      setInviteEmail("");
      setInviteUrl(result.signInUrl);
      setNotice(
        result.emailDelivered
          ? "User invited and email sent."
          : sendInviteEmail
            ? "User invited, but email delivery failed. Copy the sign-in link."
            : "User invited. Copy the sign-in link.",
      );
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not invite the user.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function saveEmailDelivery(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action: "configure-email-delivery",
        config: {
          host: smtpHost,
          port: Number(smtpPort),
          security: smtpSecurity,
          authentication: smtpPasswordAuth
            ? {
                kind: "password",
                username: smtpUsername,
                password: smtpPassword,
              }
            : { kind: "none" },
          from: smtpFrom,
        },
      });
      accessManagementMutationResponseSchema.parse(await response.json());
      setSmtpPassword("");
      setNotice("Email delivery saved.");
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error
          ? error.message
          : "Could not save email delivery.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function testEmailDelivery() {
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action: "test-email-delivery",
        to: smtpTestRecipient,
      });
      accessManagementMutationResponseSchema.parse(await response.json());
      setNotice(`Test email sent to ${smtpTestRecipient}.`);
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not send test email.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function disableEmailDeliveryConfig() {
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action: "disable-email-delivery",
      });
      accessManagementMutationResponseSchema.parse(await response.json());
      setNotice("Email delivery disabled.");
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error
          ? error.message
          : "Could not disable email delivery.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function createStarterPolicy(event: FormEvent) {
    event.preventDefault();
    const verifiedEmail = connection?.verifiedEmail;
    if (!verifiedEmail) {
      setAccessError(
        "Your identity provider did not supply a verified email. Configure the policy with Advanced JSON or the CLI.",
      );
      return;
    }
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      await applyPolicy(starterAccessPolicy(verifiedEmail));
      setNotice(
        "Access policy created. Your verified email has the admin role.",
      );
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error
          ? error.message
          : "Could not create the access policy.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function mutateUser(
    action:
      "replace-user-roles" | "enable-user" | "disable-user" | "remove-user",
    email: string,
    assignedRoles?: readonly string[],
  ) {
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const response = await post("/_localbase/access-management", {
        action,
        email,
        ...(assignedRoles ? { roles: assignedRoles } : {}),
      });
      accessManagementMutationResponseSchema.parse(await response.json());
      setConfirming("");
      setNotice(
        action === "remove-user"
          ? `${email} removed.`
          : action === "replace-user-roles"
            ? `Roles saved for ${email}.`
            : `${email} ${action === "enable-user" ? "enabled" : "disabled"}.`,
      );
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not update the user.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function saveRole(event: FormEvent) {
    event.preventDefault();
    if (!accessPolicy) {
      setAccessError("Configure an access policy before creating roles.");
      return;
    }
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      const role = accessControlRoleSchema.parse({
        name: roleName,
        description: roleDescription,
        permissions: rolePermissions,
      });
      await applyPolicy(
        accessControlConfigSchema.parse({
          ...accessPolicy,
          roles: [...accessPolicy.roles, role],
        }),
      );
      setRoleName("");
      setRoleDescription("");
      setRolePermissions(["inference:chat", "models:read"]);
      setNotice(`Role ${role.name} created.`);
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not create the role.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function replaceRole(role: AccessRole) {
    if (!accessPolicy) return;
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      await applyPolicy(
        accessControlConfigSchema.parse({
          ...accessPolicy,
          roles: accessPolicy.roles.map((candidate) =>
            candidate.name === role.name ? role : candidate,
          ),
        }),
      );
      setNotice(`Role ${role.name} saved.`);
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not save the role.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function removeRole(name: string) {
    if (!accessPolicy) return;
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      await applyPolicy(
        accessControlConfigSchema.parse({
          ...accessPolicy,
          roles: accessPolicy.roles.filter((role) => role.name !== name),
        }),
      );
      setConfirming("");
      setNotice(`Role ${name} removed.`);
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not remove the role.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function setDefaultRole(name: string | null) {
    if (!accessPolicy) return;
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      await applyPolicy(
        accessControlConfigSchema.parse({
          ...accessPolicy,
          defaultRole: name,
        }),
      );
      setNotice(
        name
          ? `Eligible signed-in identities now receive ${name}.`
          : "Default role cleared.",
      );
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error
          ? error.message
          : "Could not change the default role.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function clearPolicy() {
    if (!policyRevision) return;
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      await (
        await post("/_localbase/access-management", {
          action: "clear-policy",
          expectedPolicyRevision: policyRevision,
        })
      ).body?.cancel();
      setConfirming("");
      setNotice("Policy cleared. Provider-wide permissions now apply.");
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Could not clear the policy.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function createKey(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setSecret("");
    setNotice("");
    setKeysError("");
    try {
      const response = await post("/_localbase/api-keys", {
        action: "create",
        name: keyName,
        ...(keyExpiry ? { expiresDays: Number(keyExpiry) } : {}),
        scopes: keyPermissions,
      });
      const result = keyManagementMutationResponseSchema.parse(
        await response.json(),
      );
      if (!("secret" in result)) throw new Error("The new secret was missing.");
      setSecret(result.secret);
      setKeyName("");
      setKeyExpiry("");
      setNotice("API key created. Copy its secret now.");
      await load();
    } catch (error) {
      setKeysError(
        error instanceof Error ? error.message : "API key creation failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function mutateKey(
    key: ApiKey,
    action: "rotate" | "revoke" | "set-scopes",
    scopes?: readonly Permission[],
  ) {
    setBusy(true);
    setSecret("");
    setNotice("");
    setKeysError("");
    try {
      const response = await post("/_localbase/api-keys", {
        action,
        keyId: key.id,
        ...(scopes ? { scopes } : {}),
      });
      const result = keyManagementMutationResponseSchema.parse(
        await response.json(),
      );
      if ("secret" in result) {
        setSecret(result.secret);
        setNotice("API key rotated. Copy its new secret now.");
      } else {
        setNotice(action === "revoke" ? "API key revoked." : "Scopes saved.");
      }
      setConfirming("");
      await load();
    } catch (error) {
      setKeysError(
        error instanceof Error ? error.message : "API key update failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  if (!connection)
    return <p className="notice">Sign in to manage access and API keys.</p>;

  return (
    <div className="auth-management">
      <div className="admin-tabs" aria-label="Administration">
        <button
          type="button"
          aria-pressed={section === "people"}
          onClick={() => setSection("people")}
        >
          People & roles
        </button>
        <button
          type="button"
          aria-pressed={section === "access"}
          onClick={() => setSection("access")}
        >
          Browser access
        </button>
        <button
          type="button"
          aria-pressed={section === "keys"}
          onClick={() => setSection("keys")}
        >
          API keys
        </button>
      </div>
      {notice && (
        <div className="notice admin-notice" role="status">
          <span>{notice}</span>
          <button onClick={() => setNotice("")} aria-label="Dismiss notice">
            ✕
          </button>
        </div>
      )}
      {secret && (
        <section className="secret-once" aria-label="New API key secret">
          <strong>Copy this secret now</strong>
          <p>It will not be shown again.</p>
          <code>{secret}</code>
          <div className="admin-actions">
            <button
              onClick={() =>
                void navigator.clipboard
                  .writeText(secret)
                  .then(() => setNotice("Secret copied."))
                  .catch(() =>
                    setKeysError(
                      "Could not copy the secret. Select it and copy it manually.",
                    ),
                  )
              }
            >
              Copy secret
            </button>
            <button onClick={() => setSecret("")}>Done</button>
          </div>
        </section>
      )}
      {section === "people" ? (
        <PeopleAndRoles
          policy={accessPolicy}
          users={users}
          roles={roles}
          loadState={accessLoadState}
          busy={busy}
          error={accessError}
          verifiedEmail={connection.verifiedEmail}
          inviteEmail={inviteEmail}
          inviteRoles={inviteRoles}
          sendInviteEmail={sendInviteEmail}
          canEmailInvite={emailDelivery !== null}
          inviteUrl={inviteUrl}
          roleName={roleName}
          roleDescription={roleDescription}
          rolePermissions={rolePermissions}
          confirming={confirming}
          setInviteEmail={setInviteEmail}
          setInviteRoles={setInviteRoles}
          setSendInviteEmail={setSendInviteEmail}
          setRoleName={setRoleName}
          setRoleDescription={setRoleDescription}
          setRolePermissions={setRolePermissions}
          setConfirming={setConfirming}
          inviteUser={inviteUser}
          createStarterPolicy={createStarterPolicy}
          mutateUser={mutateUser}
          saveRole={saveRole}
          replaceRole={replaceRole}
          removeRole={removeRole}
          setDefaultRole={setDefaultRole}
          refresh={() => void load()}
          openPolicy={() => setSection("access")}
        />
      ) : section === "access" ? (
        <div className="admin-stack">
          <section className="admin-card">
            <div className="admin-card-heading">
              <div>
                <h3>Human sign-in</h3>
                <p>{providerLabel(access)}</p>
              </div>
              {access && (
                <span className="model-badge installed">Configured</span>
              )}
            </div>
            {accessError && <p className="error">{accessError}</p>}
            <form onSubmit={saveProvider}>
              <label>
                Provider
                <select
                  value={provider}
                  disabled={busy}
                  onChange={(event) =>
                    setProvider(event.target.value as typeof provider)
                  }
                >
                  <option value="cloudflare-access">Cloudflare Access</option>
                  <option value="oidc">OpenID Connect</option>
                  <option value="github-oauth">GitHub</option>
                  <option value="magic-link">Email magic link</option>
                </select>
              </label>
              <label>
                Public HTTPS origin
                <input
                  required
                  type="url"
                  value={origin}
                  placeholder="https://localbase.example.com"
                  onChange={(event) => setOrigin(event.target.value)}
                />
              </label>
              {provider === "cloudflare-access" ? (
                <>
                  <label>
                    Team domain
                    <input
                      required
                      value={teamDomain}
                      placeholder="team.cloudflareaccess.com"
                      onChange={(event) => setTeamDomain(event.target.value)}
                    />
                  </label>
                  <label>
                    Application audience
                    <input
                      required
                      value={audience}
                      onChange={(event) => setAudience(event.target.value)}
                    />
                  </label>
                </>
              ) : (
                <>
                  {access?.provider.kind === "direct" && (
                    <div className="key-list">
                      {access.provider.registrations.map((registration) => (
                        <article className="key-card" key={registration.id}>
                          <div className="admin-card-heading">
                            <div>
                              <strong>{registration.name}</strong>
                              <p>
                                {registration.id} ·{" "}
                                {registration.kind === "oidc"
                                  ? registration.issuer
                                  : registration.kind === "github-oauth"
                                    ? "GitHub OAuth"
                                    : "Email magic link"}
                              </p>
                            </div>
                            <button
                              type="button"
                              disabled={busy}
                              onClick={() => editRegistration(registration)}
                            >
                              Edit
                            </button>
                          </div>
                          {confirming ===
                          `remove-registration:${registration.id}` ? (
                            <div className="inline-confirmation">
                              <p>
                                Remove this registration? Removing the last one
                                disables browser access.
                              </p>
                              <button
                                type="button"
                                disabled={busy}
                                onClick={() =>
                                  void removeRegistration(registration.id)
                                }
                              >
                                Confirm
                              </button>
                              <button
                                type="button"
                                onClick={() => setConfirming("")}
                              >
                                Cancel
                              </button>
                            </div>
                          ) : (
                            <button
                              className="danger"
                              type="button"
                              disabled={busy}
                              onClick={() =>
                                setConfirming(
                                  `remove-registration:${registration.id}`,
                                )
                              }
                            >
                              Remove
                            </button>
                          )}
                        </article>
                      ))}
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => editRegistration(undefined)}
                      >
                        Add registration
                      </button>
                    </div>
                  )}
                  <label>
                    Registration ID
                    <input
                      required
                      pattern="[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?"
                      maxLength={64}
                      disabled={
                        busy ||
                        (access?.provider.kind === "direct" &&
                          access.provider.registrations.some(
                            ({ id }) => id === registrationId,
                          ))
                      }
                      value={registrationId}
                      placeholder="google"
                      onChange={(event) =>
                        setRegistrationId(event.target.value)
                      }
                    />
                  </label>
                  <label>
                    Sign-in name
                    <input
                      required
                      maxLength={64}
                      value={registrationName}
                      placeholder="Google"
                      onChange={(event) =>
                        setRegistrationName(event.target.value)
                      }
                    />
                  </label>
                  {provider === "oidc" && (
                    <label>
                      Issuer
                      <input
                        required
                        type="url"
                        value={issuer}
                        placeholder="https://identity.example.com"
                        onChange={(event) => setIssuer(event.target.value)}
                      />
                    </label>
                  )}
                  {provider !== "magic-link" && (
                    <label>
                      Client ID
                      <input
                        required
                        value={clientId}
                        onChange={(event) => setClientId(event.target.value)}
                      />
                    </label>
                  )}
                  {provider === "oidc" && (
                    <label className="toggle">
                      <input
                        type="checkbox"
                        checked={publicClient}
                        onChange={(event) =>
                          setPublicClient(event.target.checked)
                        }
                      />
                      Public client with PKCE
                    </label>
                  )}
                  {provider !== "magic-link" &&
                    (provider === "github-oauth" || !publicClient) && (
                      <label>
                        Client secret
                        <input
                          required
                          type="password"
                          autoComplete="new-password"
                          value={clientSecret}
                          placeholder="Re-enter to save"
                          onChange={(event) =>
                            setClientSecret(event.target.value)
                          }
                        />
                      </label>
                    )}
                </>
              )}
              <details>
                <summary>Provider-wide permissions</summary>
                <p className="hint">
                  A configured policy replaces these permissions. Keep CLI
                  access available as the recovery path.
                </p>
                <PermissionPicker
                  value={accessPermissions}
                  onChange={setAccessPermissions}
                  disabled={busy}
                />
              </details>
              <button className="primary-action" disabled={busy} type="submit">
                {provider === "cloudflare-access"
                  ? "Save provider"
                  : "Save registration"}
              </button>
            </form>
          </section>
          <section className="admin-card">
            <div className="admin-card-heading">
              <div>
                <h3>Email delivery</h3>
                <p>
                  {emailDelivery
                    ? `${emailDelivery.host}:${emailDelivery.port} · ${emailDelivery.from}`
                    : "SMTP is not configured"}
                </p>
              </div>
              {emailDelivery && (
                <span className="model-badge installed">Configured</span>
              )}
            </div>
            <form onSubmit={saveEmailDelivery}>
              <label>
                SMTP host
                <input
                  required
                  value={smtpHost}
                  placeholder="smtp.example.com"
                  onChange={(event) => setSmtpHost(event.target.value)}
                />
              </label>
              <label>
                Port
                <input
                  required
                  type="number"
                  min="1"
                  max="65535"
                  value={smtpPort}
                  onChange={(event) => setSmtpPort(event.target.value)}
                />
              </label>
              <label>
                Transport security
                <select
                  value={smtpSecurity}
                  onChange={(event) =>
                    setSmtpSecurity(event.target.value as typeof smtpSecurity)
                  }
                >
                  <option value="starttls">STARTTLS</option>
                  <option value="tls">TLS</option>
                </select>
              </label>
              <label>
                From address
                <input
                  required
                  type="email"
                  value={smtpFrom}
                  onChange={(event) => setSmtpFrom(event.target.value)}
                />
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={smtpPasswordAuth}
                  onChange={(event) =>
                    setSmtpPasswordAuth(event.target.checked)
                  }
                />
                SMTP username and password
              </label>
              <label>
                Username
                <input
                  required={smtpPasswordAuth}
                  disabled={!smtpPasswordAuth}
                  autoComplete="username"
                  value={smtpUsername}
                  onChange={(event) => setSmtpUsername(event.target.value)}
                />
              </label>
              <label>
                Password
                <input
                  type="password"
                  autoComplete="new-password"
                  required={smtpPasswordAuth}
                  disabled={!smtpPasswordAuth}
                  value={smtpPassword}
                  placeholder={
                    emailDelivery?.authentication === "password"
                      ? "Re-enter to replace credentials"
                      : ""
                  }
                  onChange={(event) => setSmtpPassword(event.target.value)}
                />
              </label>
              <div className="admin-actions">
                <button
                  className="primary-action"
                  disabled={busy}
                  type="submit"
                >
                  Save SMTP
                </button>
                {emailDelivery && (
                  <button
                    className="danger"
                    disabled={busy}
                    type="button"
                    onClick={() => void disableEmailDeliveryConfig()}
                  >
                    Disable
                  </button>
                )}
              </div>
            </form>
            {emailDelivery && (
              <div className="admin-actions">
                <label className="compact-field">
                  Test recipient
                  <input
                    type="email"
                    value={smtpTestRecipient}
                    onChange={(event) =>
                      setSmtpTestRecipient(event.target.value)
                    }
                  />
                </label>
                <button
                  type="button"
                  disabled={busy || !smtpTestRecipient}
                  onClick={() => void testEmailDelivery()}
                >
                  Send test
                </button>
              </div>
            )}
          </section>
          <section className="admin-card">
            <h3>Access policy</h3>
            <p className="hint">
              JSON roles and identity bindings. At least one binding must retain
              <code> access:manage</code>.
            </p>
            <label htmlFor="access-policy-json">Policy JSON</label>
            <textarea
              id="access-policy-json"
              value={policy}
              disabled={busy || !access}
              spellCheck={false}
              placeholder='{"roles":[{"name":"admin","permissions":["access:manage"]}],"bindings":[{"kind":"email","role":"admin","email":"owner@example.com"}],"defaultRole":null}'
              onChange={(event) => setPolicy(event.target.value)}
            />
            <div className="admin-actions">
              <button
                disabled={busy || !access || !policy.trim()}
                onClick={() => void savePolicy()}
              >
                Save policy
              </button>
              {policyConfigured && confirming !== "clear-policy" && (
                <button
                  className="danger"
                  disabled={busy}
                  onClick={() => setConfirming("clear-policy")}
                >
                  Clear policy
                </button>
              )}
            </div>
            {confirming === "clear-policy" && (
              <div className="inline-confirmation">
                <p>Use provider-wide permissions instead?</p>
                <button disabled={busy} onClick={() => void clearPolicy()}>
                  Confirm clear
                </button>
                <button onClick={() => setConfirming("")}>Cancel</button>
              </div>
            )}
          </section>
        </div>
      ) : (
        <div className="admin-stack">
          <section className="admin-card">
            <h3>Create API key</h3>
            {keysError && <p className="error">{keysError}</p>}
            <form onSubmit={createKey}>
              <label>
                Name
                <input
                  required
                  maxLength={128}
                  value={keyName}
                  onChange={(event) => setKeyName(event.target.value)}
                />
              </label>
              <label>
                Expires after days <span className="muted">(optional)</span>
                <input
                  type="number"
                  min="1"
                  max="3650"
                  value={keyExpiry}
                  onChange={(event) => setKeyExpiry(event.target.value)}
                />
              </label>
              <details>
                <summary>Scopes · {keyPermissions.length} selected</summary>
                <PermissionPicker
                  value={keyPermissions}
                  onChange={setKeyPermissions}
                  disabled={busy}
                />
              </details>
              <button className="primary-action" disabled={busy} type="submit">
                Create key
              </button>
            </form>
          </section>
          <section className="admin-card">
            <div className="admin-card-heading">
              <div>
                <h3>Keys</h3>
                <p>{keys.length} total</p>
              </div>
              <button disabled={busy} onClick={() => void load()}>
                Refresh
              </button>
            </div>
            <div className="key-list">
              {keys.map((key) => {
                const status = apiKeyStatus(key);
                const active = status === "Active";
                return (
                  <article className="key-card" key={key.id}>
                    <div className="admin-card-heading">
                      <div>
                        <strong>{key.name}</strong>
                        <p>
                          {key.prefix}… · {status}
                        </p>
                      </div>
                      <span
                        className={`model-badge ${active ? "installed" : ""}`}
                      >
                        {status}
                      </span>
                    </div>
                    <p className="key-id">{key.id}</p>
                    <details>
                      <summary>{key.scopes.length} scopes</summary>
                      <KeyScopeEditor
                        apiKey={key}
                        busy={busy || !active}
                        save={(scopes) =>
                          void mutateKey(key, "set-scopes", scopes)
                        }
                      />
                    </details>
                    {active && (
                      <div className="admin-actions">
                        <button
                          disabled={busy}
                          onClick={() => setConfirming(`rotate:${key.id}`)}
                        >
                          Rotate
                        </button>
                        <button
                          className="danger"
                          disabled={busy}
                          onClick={() => setConfirming(`revoke:${key.id}`)}
                        >
                          Revoke
                        </button>
                      </div>
                    )}
                    {active && confirming.endsWith(`:${key.id}`) && (
                      <div className="inline-confirmation">
                        <p>
                          {confirming.startsWith("rotate")
                            ? "The current secret will stop working immediately."
                            : "This key will stop working immediately."}
                        </p>
                        <button
                          disabled={busy}
                          onClick={() =>
                            void mutateKey(
                              key,
                              confirming.startsWith("rotate")
                                ? "rotate"
                                : "revoke",
                            )
                          }
                        >
                          Confirm
                        </button>
                        <button onClick={() => setConfirming("")}>
                          Cancel
                        </button>
                      </div>
                    )}
                  </article>
                );
              })}
              {!keys.length && !keysError && (
                <p className="hint">No API keys.</p>
              )}
            </div>
          </section>
        </div>
      )}
    </div>
  );
}

function RolePicker({
  roles,
  value,
  onChange,
  disabled,
}: {
  roles: readonly AccessRole[];
  value: readonly string[];
  onChange: (roles: string[]) => void;
  disabled?: boolean;
}) {
  return (
    <div className="role-picker">
      {roles.map((role) => (
        <label className="permission-option" key={role.name}>
          <input
            type="checkbox"
            disabled={disabled}
            checked={value.includes(role.name)}
            onChange={(event) =>
              onChange(
                event.target.checked
                  ? [...value, role.name]
                  : value.filter((name) => name !== role.name),
              )
            }
          />
          <span>
            <strong>{role.name}</strong>
            {role.description && <small>{role.description}</small>}
          </span>
        </label>
      ))}
    </div>
  );
}

function PeopleAndRoles({
  policy,
  users,
  roles,
  loadState,
  busy,
  error,
  verifiedEmail,
  inviteEmail,
  inviteRoles,
  sendInviteEmail,
  canEmailInvite,
  inviteUrl,
  roleName,
  roleDescription,
  rolePermissions,
  confirming,
  setInviteEmail,
  setInviteRoles,
  setSendInviteEmail,
  setRoleName,
  setRoleDescription,
  setRolePermissions,
  setConfirming,
  inviteUser,
  createStarterPolicy,
  mutateUser,
  saveRole,
  replaceRole,
  removeRole,
  setDefaultRole,
  refresh,
  openPolicy,
}: {
  policy: AccessPolicy;
  users: readonly ManagedUser[];
  roles: readonly AccessRole[];
  loadState: "loading" | "loaded" | "error";
  busy: boolean;
  error: string;
  verifiedEmail?: string;
  inviteEmail: string;
  inviteRoles: readonly string[];
  sendInviteEmail: boolean;
  canEmailInvite: boolean;
  inviteUrl: string;
  roleName: string;
  roleDescription: string;
  rolePermissions: readonly Permission[];
  confirming: string;
  setInviteEmail: (email: string) => void;
  setInviteRoles: (roles: string[]) => void;
  setSendInviteEmail: (send: boolean) => void;
  setRoleName: (name: string) => void;
  setRoleDescription: (description: string) => void;
  setRolePermissions: (permissions: Permission[]) => void;
  setConfirming: (value: string) => void;
  inviteUser: (event: FormEvent) => Promise<void>;
  createStarterPolicy: (event: FormEvent) => Promise<void>;
  mutateUser: (
    action:
      "replace-user-roles" | "enable-user" | "disable-user" | "remove-user",
    email: string,
    roles?: readonly string[],
  ) => Promise<void>;
  saveRole: (event: FormEvent) => Promise<void>;
  replaceRole: (role: AccessRole) => Promise<void>;
  removeRole: (name: string) => Promise<void>;
  setDefaultRole: (name: string | null) => Promise<void>;
  refresh: () => void;
  openPolicy: () => void;
}) {
  return (
    <div className="admin-stack people-management">
      {error && (
        <p className="error admin-span" role="alert">
          {error}
        </p>
      )}
      {loadState === "loading" ? (
        <section
          className="admin-card admin-span empty-state-card"
          aria-live="polite"
        >
          <h3>Loading access policy</h3>
          <p className="hint">Checking roles and managed users…</p>
        </section>
      ) : loadState === "error" ? (
        <section className="admin-card admin-span empty-state-card">
          <h3>Access policy unavailable</h3>
          <p className="hint">
            No changes are available until the current policy can be read.
          </p>
          <button disabled={busy} onClick={refresh}>
            Try again
          </button>
        </section>
      ) : !policy ? (
        <section className="admin-card admin-span empty-state-card">
          <h3>Set up roles</h3>
          <p className="hint">
            This creates an admin role for your verified email and a default
            member role with chat access. Model management is not granted by
            default.
          </p>
          <form onSubmit={createStarterPolicy}>
            <label>
              Your verified sign-in email
              <input
                required
                type="email"
                readOnly
                disabled={!verifiedEmail}
                value={verifiedEmail ?? ""}
                placeholder="Verified email unavailable"
              />
            </label>
            {!verifiedEmail && (
              <p className="hint">
                This provider did not supply a verified email. Use Advanced JSON
                or the CLI to bind the first administrator.
              </p>
            )}
            <div className="admin-actions">
              <button
                className="primary-action"
                disabled={busy || !verifiedEmail}
                type="submit"
              >
                Create roles
              </button>
              <button type="button" disabled={busy} onClick={openPolicy}>
                Advanced JSON
              </button>
            </div>
          </form>
        </section>
      ) : (
        <>
          <section className="admin-card">
            <h3>Invite a person</h3>
            <p className="hint">
              Their verified sign-in email claims this invitation on first
              login.
            </p>
            <form onSubmit={inviteUser}>
              <label>
                Email
                <input
                  required
                  type="email"
                  autoComplete="email"
                  value={inviteEmail}
                  onChange={(event) => setInviteEmail(event.target.value)}
                />
              </label>
              <fieldset>
                <legend>Roles</legend>
                <RolePicker
                  roles={roles}
                  value={inviteRoles}
                  onChange={setInviteRoles}
                  disabled={busy}
                />
              </fieldset>
              <label className="toggle">
                <input
                  type="checkbox"
                  disabled={busy || !canEmailInvite}
                  checked={sendInviteEmail && canEmailInvite}
                  onChange={(event) => setSendInviteEmail(event.target.checked)}
                />
                Send invitation email
              </label>
              {!canEmailInvite && (
                <p className="hint">
                  Configure SMTP under Browser access to send invitations.
                </p>
              )}
              <button
                className="primary-action"
                disabled={busy || !inviteRoles.length}
                type="submit"
              >
                Create invitation
              </button>
              {inviteUrl && (
                <div className="secret-once">
                  <strong>Invitation link</strong>
                  <code>{inviteUrl}</code>
                  <button
                    type="button"
                    onClick={() =>
                      void navigator.clipboard
                        .writeText(inviteUrl)
                        .catch(() => undefined)
                    }
                  >
                    Copy link
                  </button>
                </div>
              )}
            </form>
          </section>
          <section className="admin-card">
            <div className="admin-card-heading">
              <div>
                <h3>People</h3>
                <p>{users.length} managed users</p>
              </div>
              <button disabled={busy} onClick={refresh}>
                Refresh
              </button>
            </div>
            <div className="key-list">
              {users.map((user) => (
                <ManagedUserCard
                  key={user.id}
                  user={user}
                  roles={roles}
                  busy={busy}
                  currentUser={isCurrentManagedUser(user.email, verifiedEmail)}
                  confirming={confirming}
                  setConfirming={setConfirming}
                  mutate={mutateUser}
                />
              ))}
              {!users.length && <p className="hint">No managed users.</p>}
            </div>
          </section>
          <section className="admin-card admin-span">
            <div className="admin-card-heading">
              <div>
                <h3>Roles</h3>
                <p>{roles.length} roles</p>
              </div>
              <label className="compact-field">
                Default for eligible sign-ins
                <select
                  disabled={busy}
                  value={policy.defaultRole ?? ""}
                  onChange={(event) =>
                    void setDefaultRole(event.target.value || null)
                  }
                >
                  <option value="">No default role</option>
                  {roles.map((role) => (
                    <option key={role.name} value={role.name}>
                      {role.name}
                    </option>
                  ))}
                </select>
              </label>
            </div>
            <div className="role-grid">
              {roles.map((role) => (
                <RoleEditor
                  key={role.name}
                  role={role}
                  busy={busy}
                  confirming={confirming}
                  setConfirming={setConfirming}
                  save={replaceRole}
                  remove={removeRole}
                />
              ))}
            </div>
          </section>
          <section className="admin-card admin-span">
            <h3>Create a role</h3>
            <form onSubmit={saveRole}>
              <div className="role-fields">
                <label>
                  Name
                  <input
                    required
                    pattern="[a-z][a-z0-9-]*"
                    maxLength={64}
                    value={roleName}
                    placeholder="video-creator"
                    onChange={(event) => setRoleName(event.target.value)}
                  />
                </label>
                <label>
                  Description
                  <input
                    maxLength={256}
                    value={roleDescription}
                    placeholder="Can create and inspect videos"
                    onChange={(event) => setRoleDescription(event.target.value)}
                  />
                </label>
              </div>
              <PermissionPicker
                value={rolePermissions}
                onChange={setRolePermissions}
                disabled={busy}
              />
              <button className="primary-action" disabled={busy} type="submit">
                Create role
              </button>
            </form>
          </section>
        </>
      )}
    </div>
  );
}

function ManagedUserCard({
  user,
  roles,
  busy,
  currentUser,
  confirming,
  setConfirming,
  mutate,
}: {
  user: ManagedUser;
  roles: readonly AccessRole[];
  busy: boolean;
  currentUser: boolean;
  confirming: string;
  setConfirming: (value: string) => void;
  mutate: (
    action:
      "replace-user-roles" | "enable-user" | "disable-user" | "remove-user",
    email: string,
    roles?: readonly string[],
  ) => Promise<void>;
}) {
  const [assigned, setAssigned] = useState<string[]>([...user.roles]);
  const forgetButton = useRef<HTMLButtonElement>(null);
  useEffect(() => setAssigned([...user.roles]), [user.roles.join(",")]);
  const removalKey = `remove-user:${user.id}`;
  const removesOwnManagement =
    currentUser && !assignedRolesGrantAccessManagement(assigned, roles);
  return (
    <article className="key-card">
      <div className="admin-card-heading">
        <div>
          <strong>{user.email}</strong>
          <p>Added {new Date(user.createdAt).toLocaleDateString()}</p>
        </div>
        <span
          className={`model-badge ${user.status === "active" ? "installed" : ""}`}
        >
          {user.status}
        </span>
      </div>
      <RolePicker
        roles={roles}
        value={assigned}
        onChange={setAssigned}
        disabled={busy || user.status === "disabled"}
      />
      <div className="admin-actions">
        <button
          disabled={
            busy ||
            user.status === "disabled" ||
            removesOwnManagement ||
            assigned.join(",") === user.roles.join(",")
          }
          onClick={() =>
            void mutate("replace-user-roles", user.email, assigned)
          }
        >
          Save roles
        </button>
        <button
          disabled={busy || (currentUser && user.status === "active")}
          onClick={() =>
            void mutate(
              user.status === "disabled" ? "enable-user" : "disable-user",
              user.email,
            )
          }
        >
          {user.status === "disabled" ? "Enable access" : "Disable access"}
        </button>
        <button
          className="danger"
          disabled={busy || currentUser}
          ref={forgetButton}
          aria-expanded={confirming === removalKey}
          onClick={() => setConfirming(removalKey)}
        >
          Forget record
        </button>
      </div>
      {currentUser && user.status === "active" && (
        <p className="hint">
          The signed-in account cannot disable or forget itself, and must retain
          an assigned role with access:manage.
        </p>
      )}
      {confirming === removalKey && (
        <div
          className="inline-confirmation"
          role="group"
          aria-live="polite"
          aria-labelledby={`forget-user-${user.id}`}
        >
          <p id={`forget-user-${user.id}`}>
            Forget this user and linked identity? This is not revocation: they
            may regain access through a default role or another policy binding.
            Use Disable to block access.
          </p>
          <button
            disabled={busy}
            onClick={() => void mutate("remove-user", user.email)}
          >
            Confirm forget
          </button>
          <button
            onClick={() => {
              setConfirming("");
              forgetButton.current?.focus();
            }}
          >
            Cancel
          </button>
        </div>
      )}
    </article>
  );
}

function RoleEditor({
  role,
  busy,
  confirming,
  setConfirming,
  save,
  remove,
}: {
  role: AccessRole;
  busy: boolean;
  confirming: string;
  setConfirming: (value: string) => void;
  save: (role: AccessRole) => Promise<void>;
  remove: (name: string) => Promise<void>;
}) {
  const [description, setDescription] = useState(role.description);
  const removeButton = useRef<HTMLButtonElement>(null);
  const [permissions, setPermissions] = useState<Permission[]>([
    ...role.permissions,
  ]);
  useEffect(() => {
    setDescription(role.description);
    setPermissions([...role.permissions]);
  }, [role.description, role.permissions.join(",")]);
  const removalKey = `remove-role:${role.name}`;
  const changed =
    description !== role.description ||
    permissions.join(",") !== role.permissions.join(",");
  return (
    <article className="key-card">
      <strong>{role.name}</strong>
      <label>
        Description
        <input
          maxLength={256}
          disabled={busy}
          value={description}
          onChange={(event) => setDescription(event.target.value)}
        />
      </label>
      <details>
        <summary>{permissions.length} permissions</summary>
        <PermissionPicker
          value={permissions}
          onChange={setPermissions}
          disabled={busy}
        />
      </details>
      <div className="admin-actions">
        <button
          disabled={busy || !changed}
          onClick={() =>
            void save(
              accessControlRoleSchema.parse({
                name: role.name,
                description,
                permissions,
              }),
            )
          }
        >
          Save role
        </button>
        <button
          className="danger"
          disabled={busy}
          ref={removeButton}
          aria-expanded={confirming === removalKey}
          onClick={() => setConfirming(removalKey)}
        >
          Remove
        </button>
      </div>
      {confirming === removalKey && (
        <div
          className="inline-confirmation"
          role="group"
          aria-live="polite"
          aria-labelledby={`remove-role-${role.name}`}
        >
          <p id={`remove-role-${role.name}`}>
            Remove this role? Assigned or policy-bound roles cannot be removed.
          </p>
          <button disabled={busy} onClick={() => void remove(role.name)}>
            Confirm remove
          </button>
          <button
            onClick={() => {
              setConfirming("");
              removeButton.current?.focus();
            }}
          >
            Cancel
          </button>
        </div>
      )}
    </article>
  );
}

function KeyScopeEditor({
  apiKey,
  busy,
  save,
}: {
  apiKey: ApiKey;
  busy: boolean;
  save: (scopes: Permission[]) => void;
}) {
  const [scopes, setScopes] = useState<Permission[]>([...apiKey.scopes]);
  useEffect(() => setScopes([...apiKey.scopes]), [apiKey.scopes.join(",")]);
  return (
    <div className="key-scope-editor">
      <PermissionPicker value={scopes} onChange={setScopes} disabled={busy} />
      <button
        disabled={busy || scopes.join(",") === apiKey.scopes.join(",")}
        onClick={() => save(scopes)}
      >
        Save scopes
      </button>
    </div>
  );
}
