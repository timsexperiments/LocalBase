import { useEffect, useState, type FormEvent } from "react";
import { z } from "zod";
import {
  permissionSchema,
  type Permission,
} from "../domains/auth/authorization";
import { browserAccessPolicySchema } from "../domains/auth/browser-policy";
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
    : `OpenID Connect · ${config.provider.registrations.length} ${config.provider.registrations.length === 1 ? "registration" : "registrations"}`;
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

export function AuthManagement({
  connection,
}: {
  connection: Connection | null;
}) {
  const [section, setSection] = useState<"access" | "keys">("access");
  const [access, setAccess] = useState<AccessConfig>(null);
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [accessError, setAccessError] = useState("");
  const [keysError, setKeysError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [secret, setSecret] = useState("");
  const [confirming, setConfirming] = useState("");
  const [provider, setProvider] = useState<"cloudflare-access" | "oidc">(
    "cloudflare-access",
  );
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
  const [keyName, setKeyName] = useState("");
  const [keyExpiry, setKeyExpiry] = useState("");
  const [keyPermissions, setKeyPermissions] = useState<Permission[]>([
    "inference:chat",
    "models:read",
  ]);

  function hydrate(config: AccessConfig) {
    setAccess(config);
    setPolicy(config?.policy ? JSON.stringify(config.policy, null, 2) : "");
    if (!config) return;
    setProvider(config.provider.kind);
    setOrigin(config.origin);
    setAccessPermissions([...config.permissions]);
    if (config.provider.kind === "cloudflare-access") {
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
          { kind: "oidc" }
        >["registrations"][number]
      | undefined,
  ) {
    setRegistrationId(registration?.id ?? "");
    setRegistrationName(registration?.name ?? "");
    setIssuer(registration?.issuer ?? "");
    setClientId(registration?.clientId ?? "");
    setPublicClient(registration?.clientAuthentication === "none");
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
      setAccessError("");
    } else {
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
            : "upsert-oidc",
        ...(provider === "cloudflare-access"
          ? {
              provider: { kind: provider, teamDomain, audience },
            }
          : {
              registration: {
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
        action: "remove-oidc",
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
      const parsed = browserAccessPolicySchema.parse(JSON.parse(policy));
      await (
        await post("/_localbase/access-management", {
          action: "apply-policy",
          policy: parsed,
        })
      ).body?.cancel();
      setNotice("Policy saved. Restart LocalBase to apply it.");
      await load();
    } catch (error) {
      setAccessError(
        error instanceof Error ? error.message : "Access policy update failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function clearPolicy() {
    setBusy(true);
    setNotice("");
    setAccessError("");
    try {
      await (
        await post("/_localbase/access-management", {
          action: "clear-policy",
        })
      ).body?.cancel();
      setConfirming("");
      setNotice("Policy cleared. Restart LocalBase to apply the change.");
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
      {section === "access" ? (
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
                  {access?.provider.kind === "oidc" && (
                    <div className="key-list">
                      {access.provider.registrations.map((registration) => (
                        <article className="key-card" key={registration.id}>
                          <div className="admin-card-heading">
                            <div>
                              <strong>{registration.name}</strong>
                              <p>
                                {registration.id} · {registration.issuer}
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
                          {confirming === `remove-oidc:${registration.id}` ? (
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
                                setConfirming(`remove-oidc:${registration.id}`)
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
                        (access?.provider.kind === "oidc" &&
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
                  <label>
                    Client ID
                    <input
                      required
                      value={clientId}
                      onChange={(event) => setClientId(event.target.value)}
                    />
                  </label>
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
                  {!publicClient && (
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
                {provider === "oidc" ? "Save registration" : "Save provider"}
              </button>
            </form>
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
              placeholder='{"roles":{"admin":["access:manage"]},"bindings":[{"role":"admin","match":{"kind":"email","email":"owner@example.com"}}]}'
              onChange={(event) => setPolicy(event.target.value)}
            />
            <div className="admin-actions">
              <button
                disabled={busy || !access || !policy.trim()}
                onClick={() => void savePolicy()}
              >
                Save policy
              </button>
              {access?.policy && confirming !== "clear-policy" && (
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
                <p>Use provider-wide permissions after the next restart?</p>
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
