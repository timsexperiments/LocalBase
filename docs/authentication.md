# Authentication and authorization

LocalBase separates browser users from machine clients.

| Caller              | Credential                                            | Configuration           |
| ------------------- | ----------------------------------------------------- | ----------------------- |
| Browser user        | Cloudflare Access JWT, OIDC, or GitHub OAuth session  | `local-base access ...` |
| Machine client      | Scoped LocalBase API key                              | `local-base keys ...`   |
| Local administrator | Read and write access to the LocalBase data directory | Local CLI               |

The browser UI does not accept API keys. LocalBase does not provide an
unauthenticated LAN mode or a shared-link bypass. Keep the gateway bound to
loopback and publish it through an authenticated HTTPS proxy when remote access
is required.

## Choose a browser provider

Cloudflare Access is the shortest setup when a Cloudflare Tunnel already
publishes LocalBase. LocalBase verifies the Access application token at the
origin, including its signature, issuer, audience, and expiry.

```bash
local-base --non-interactive --json access cloudflare \
  --team-domain "$LOCALBASE_ACCESS_TEAM_DOMAIN" \
  --audience "$LOCALBASE_ACCESS_AUDIENCE" \
  --origin "$LOCALBASE_PUBLIC_ORIGIN" \
  --permissions 'inference:chat,models:read,models:manage'
local-base --non-interactive --json restart
```

Use direct OpenID Connect when the operator does not want Cloudflare Access.
LocalBase uses Authorization Code with PKCE and stores an opaque, HTTP-only
session cookie. Register `${LOCALBASE_PUBLIC_ORIGIN}/oidc/callback` as the exact
redirect URI at the provider.

```bash
local-base --non-interactive --json access oidc add \
  --id company \
  --name "Company sign-in" \
  --issuer "$LOCALBASE_OIDC_ISSUER" \
  --client-id "$LOCALBASE_OIDC_CLIENT_ID" \
  --client-secret-env LOCALBASE_OIDC_CLIENT_SECRET \
  --origin "$LOCALBASE_PUBLIC_ORIGIN" \
  --permissions 'inference:chat,models:read,models:manage'
local-base --non-interactive --json restart
```

The variable named by `--client-secret-env` must exist in the command
environment. LocalBase stores the secret privately and never returns it from
the CLI, management endpoint, or browser UI. Use `--public-client` only for a
provider registration that explicitly permits public clients.

The examples grant `models:manage` intentionally. Omit `--permissions` to use
the safer defaults, which allow chat and model discovery only.

Each registration has a stable ID and display name. Repeat the command with a
different ID to add another provider; using an existing ID updates it. With
multiple registrations, `/app/login` presents a provider picker. Automation can
list and remove registrations with `access oidc list` and
`access oidc remove ID`.

OIDC discovery keeps this integration provider-neutral. Google, Microsoft
Entra, Okta, Auth0, Keycloak, and other standards-compliant OIDC providers use
the same command and callback.

GitHub uses its OAuth web flow rather than OIDC. Create a GitHub OAuth app with
`${LOCALBASE_PUBLIC_ORIGIN}/github/callback` as its callback URL, then configure
it without putting the secret in process arguments:

```bash
: "${LOCALBASE_GITHUB_CLIENT_ID:?required}"
: "${LOCALBASE_GITHUB_CLIENT_SECRET:?required}"

local-base --non-interactive --json access github add \
  --id github \
  --name GitHub \
  --client-id "$LOCALBASE_GITHUB_CLIENT_ID" \
  --client-secret-env LOCALBASE_GITHUB_CLIENT_SECRET \
  --origin "$LOCALBASE_PUBLIC_ORIGIN"
```

GitHub and OIDC registrations share the provider picker. LocalBase uses the
immutable numeric GitHub account ID as the subject and accepts only the
verified primary email for email-based policy bindings. Use `access github
list` and `access github remove ID` for automation.

LocalBase does not consume SAML assertions directly. Put a SAML provider behind
Cloudflare Access or an identity broker that exposes OpenID Connect to
LocalBase. This keeps one verified identity contract inside the gateway.

### Email sign-in

Email delivery and sign-in-token issuance belong to the identity provider.
LocalBase deliberately does not send login mail or issue email tokens.

For Cloudflare Access, configure its
[one-time PIN provider](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/one-time-pin/)
and allow the user's address in the Access application policy. For direct
OpenID Connect, select a provider that supports passwordless email sign-in.
LocalBase receives the resulting verified OIDC identity in the same way as any
other OIDC login.

## Limit browser permissions

Provider permissions apply to every verified browser user unless a local access
policy is configured. A policy maps verified identities to named roles. Email
bindings only match provider-verified email claims.

```json
{
  "roles": [
    {
      "name": "admin",
      "permissions": [
        "access:read",
        "access:manage",
        "keys:read",
        "keys:manage",
        "models:read",
        "models:manage"
      ]
    },
    { "name": "user", "permissions": ["inference:chat", "models:read"] }
  ],
  "bindings": [
    {
      "kind": "email",
      "role": "admin",
      "email": "owner@example.com"
    },
    {
      "kind": "email-domain",
      "role": "user",
      "domain": "example.com"
    }
  ],
  "defaultRole": null
}
```

For direct OIDC, set `LOCALBASE_POLICY_ISSUER` to the configured OIDC issuer.
For GitHub, use `https://github.com`.
For Cloudflare Access, set it to `https://<team-domain>`. Then apply and test
the checked-in policy:

```bash
: "${LOCALBASE_POLICY_ISSUER:?required}"

local-base --non-interactive --json access policy apply \
  --file ./localbase-access-policy.json
local-base --non-interactive --json access policy test \
  --issuer "$LOCALBASE_POLICY_ISSUER" \
  --subject "$TEST_SUBJECT" \
  --email owner@example.com
```

A valid policy must contain at least one binding whose role includes
`access:manage`, but LocalBase cannot prove that the bound identity belongs to
a current administrator. Test the intended administrator identity before
closing the current session. The local CLI remains available to an administrator who can access
the LocalBase data directory.

## Create machine credentials

Give each application its own key and only the permissions it needs.

```bash
local-base --non-interactive --json keys create \
  --name reconciliation-worker \
  --scopes 'inference:chat,models:read'
```

The command returns the secret once. Store that value in the application's
secret manager. Key listings, scope changes, and revocation return metadata
only. Rotation returns a new secret once.

```bash
local-base --non-interactive --json keys list
local-base --non-interactive --json keys scopes key_ID \
  --scopes 'inference:chat,inference:embeddings,models:read'
local-base --non-interactive --json keys rotate key_ID
local-base --non-interactive --json keys revoke key_ID
```

Scope changes, rotation, and revocation apply to the next request without a
restart. Provider and access-policy changes require a restart.

Keys that administer LocalBase need explicit management scopes. Ordinary
inference keys do not receive them.

```bash
local-base --non-interactive --json keys create \
  --name automation-admin \
  --scopes 'access:read,access:manage,keys:read,keys:manage,models:read,models:manage'
```

Key creation is a bootstrap operation, not a replay-safe declaration. Names are
labels and are not unique. Do not run `keys create` on every deployment. Create
the key once, store its returned secret, and use its stable key ID for later
scope changes, rotation, or revocation.

## Configure LocalBase in CI

Keep provider settings in normal CI variables and provider secrets in the CI
secret store. Commit the access-policy JSON beside the deployment code. Run the
job on the target LocalBase host as the same user that owns its launchd or
systemd user service. A self-hosted runner or an SSH deployment step can do
this. Select the intended data directory with `LOCALBASE_ROOT`.

The job can then configure browser authentication without prompts:

```bash
set -eu

: "${LOCALBASE_ROOT:?required}"
: "${LOCALBASE_PUBLIC_ORIGIN:?required}"
: "${LOCALBASE_OIDC_ISSUER:?required}"
: "${LOCALBASE_OIDC_CLIENT_ID:?required}"
: "${LOCALBASE_OIDC_CLIENT_SECRET:?required}"

local-base --non-interactive --json access oidc add \
  --id company \
  --name "Company sign-in" \
  --issuer "$LOCALBASE_OIDC_ISSUER" \
  --client-id "$LOCALBASE_OIDC_CLIENT_ID" \
  --client-secret-env LOCALBASE_OIDC_CLIENT_SECRET \
  --origin "$LOCALBASE_PUBLIC_ORIGIN" \
  --permissions 'inference:chat,models:read'

local-base --non-interactive --json access policy apply \
  --file ./localbase-access-policy.json

local-base --non-interactive --json restart
curl --fail --silent --show-error \
  --retry 30 --retry-all-errors --retry-delay 1 --max-time 2 \
  http://127.0.0.1:2273/health/ready >/dev/null
```

`restart` returns when the service manager reports a running or starting
process. The bounded readiness request above is the deployment gate. Change
the URL when the persisted gateway listener uses a different loopback port.

Every finite `--json` command emits one JSON document to standard output and
uses exit code `0`, `1`, or `2` for success, operational failure, or invalid
input. Progress and diagnostics go to standard error.

## Administer through the gateway

The local CLI is the primary setup and recovery path. After authentication is
working, authorized callers can use the browser page at `/app?panel=admin` or
the management endpoints:

- `GET` and `POST /_localbase/access-management`
- `GET` and `POST /_localbase/api-keys`

Direct provider management uses `upsert-oidc`, `upsert-github`, and
`remove-registration`. Reads return every registration with client secrets
redacted.

The gateway checks `access:read`, `access:manage`, `keys:read`, or
`keys:manage` for each operation. Provider responses omit client secrets.
API-key creation and rotation return a new secret once.

Use `local-base access show --json` to inspect non-secret provider state. If a
provider is misconfigured, correct it with the local CLI and restart LocalBase.
`local-base access disable` removes browser sign-in entirely; it does not enable
anonymous browser access.
