# Privacy

LocalBase has **no telemetry by default**. Your prompts and outputs stay on your machine, and nothing is sent to the project.

## What it contacts

| Destination | When | What it reveals |
| --- | --- | --- |
| Hugging Face | When you install or search for a model | Your IP address and the model you asked for. Your token, if you set one |
| GitHub and the FFmpeg build hosts | When a runtime is first needed | Your IP address and which runtime |
| `raw.githubusercontent.com` | Fetching the catalog feed, about once a day while running | Your IP address. Turn it off with `local-base models feed use off` |
| `api.github.com` | Only if you run `local-base upgrade --check`, or turn on the daily check, which is off by default | One `GET` with `Accept` and `User-Agent: local-base`, so your IP address. No version, machine ID, usage or token |
| Your identity provider, or your SMTP server | Only if you configure sign-in | What sign-in needs |
| Your OTLP collector | Only if you configure it | See below |

## What OTLP export sends

Export is off until you set an endpoint. When on, it sends operational logs and traces to **your** collector: request timing, status codes, model IDs and runtime events. Prompts, responses, credentials, baggage and arbitrary headers are never exported.

## What is stored, and where

Everything is under the data directory, `~/.local/share/local-base` by default, which is created readable by your user only (0700). **Nothing is encrypted at rest**, and copies of the database hold its secrets. See [where secrets live](secrets.md).

| What | Where | Notes |
| --- | --- | --- |
| Settings | `local-base.db` (0600) | **Your Hugging Face token and OTLP headers, if you set them, are stored here in plain text** |
| API keys | `local-base.db` | Stored as hashes. The secret is shown once |
| Browser sign-in secrets | `ui-access.json` (0600) | Provider secrets |
| Logs | `logs/` | Redacted, with no prompts or responses. Rotated at 10 MiB, five archives |
| **Usage totals** | `local-base.db` | **On by default.** Hourly counts, tokens and durations per key or person, route, model and status, kept 30 days. Never prompts, replies, IP addresses or request bodies. See [usage and activity](usage.md), which also shows how to turn it off or clear it |
| **Backups** | Where you write them, on this computer | **A backup holds secrets in plain text.** It's written only to this computer, and nothing uploads it. Keep it private. See [backup and restore](backup.md) |
| **Detached run checkpoints** | `runs/<id>.json` (0600) | **Model output in plaintext.** Prompts aren't stored. Deleted when dismissed, or 30 minutes after the run ends |
| Custom voices | `voices/` (0700, files 0600) | The reference audio you added |
| Models and runtimes | `models/`, `bin/runtimes/` | Downloaded files |

**Chat history is stored in your browser**, not on the server, and only if you turn it on in Settings. Each person's is kept separately. It stays on this device after you sign out and comes back when you sign in, and others signing in on this browser don't see it. It isn't encrypted, so someone with access to this browser's files could read it, and while saving is on, chat names also appear in your browser's history. **Clear saved chats on this device** in Settings removes yours, and clearing your browser's site data removes everyone's. Chats saved before this was added are no longer shown; Clear removes them too.

## Diagnostics bundles

`local-base diagnostics` creates a redacted ZIP on your machine and sends nothing. It holds hardware details, configuration shape, model names and recent log events, and never `runs/`. Look through it before sharing it.

## Removing your data

`local-base uninstall --yes` removes the data directory. See [uninstall](install.md#uninstall). After a rollback to an older version, delete `runs/*.json` by hand, as [the upgrade guide](upgrade.md#roll-back) explains.

If you expose LocalBase to other people, you're responsible for their data under the laws that apply to you.
