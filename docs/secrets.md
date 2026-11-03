# Where secrets live

**Nothing is encrypted at rest.** The protection is file permissions: a data directory is private to the account that runs LocalBase, with directories at 0700 and files at 0600. Anyone who can read as that account can read these.

| Secret | Where | Stored as |
| --- | --- | --- |
| Hugging Face token | `local-base.db` | Plain text |
| OTLP headers | `local-base.db` | Plain text |
| API keys | `local-base.db` | SHA-256 only. The key is shown once and can't be recovered |
| Browser sessions, magic links | `local-base.db` | SHA-256 of the token only |
| OIDC client secret | `ui-access.json` | Plain text |
| SMTP password | `email-delivery.json` | Plain text |
| Auth-profile header values, for private downloads | `credentials/auth-profiles.json` | Plain text |
| CSRF key | `runtime/ui-csrf.key` | Raw key |
| Instance and service tokens | `runtime/gateway.lock/owner.json`. The service token is also in `service/manifest.json` and the launchd plist or systemd unit | Plain text |

**Copies of the database hold all of its plain-text secrets.** That includes `local-base.db.bak-*` files and any backup you make, so protect them the same way.

## What the permissions don't protect against

- The same account, or root.
- A backup of the disk.

## Older data directories

`serve` tightens an older data directory when it starts, except `models/` and `bin/`, which hold no secrets. A failure logs the warning `root.permissions-failed` and doesn't stop startup. Only `serve` does this, so a loose copy stays loose until the next start, and a symlinked database keeps its target's permissions.

**After a rollback to TypeScript 0.1.0:** it creates the database under your umask, usually 0644, and doesn't tighten anything. The 0700 data directory still keeps other users out.
