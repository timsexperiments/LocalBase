# Privacy

LocalBase has **no telemetry by default**. Your prompts and outputs stay on your machine, and nothing is sent to the project.

## What it contacts

| Destination | When | What it reveals |
| --- | --- | --- |
| Hugging Face | When you install or search for a model | Your IP address and the model you asked for. Your token, if you set one |
| GitHub and the FFmpeg build hosts | When a runtime is first needed | Your IP address and which runtime |
| `raw.githubusercontent.com` | Fetching the catalog feed, about once a day while running | Your IP address. Turn it off with `local-base models feed use off` |
| Your identity provider, or your SMTP server | Only if you configure sign-in | What sign-in needs |
| Your OTLP collector | Only if you configure it | See below |

## What OTLP export sends

Export is off until you set an endpoint. When on, it sends operational logs and traces to **your** collector: request timing, status codes, model IDs and runtime events. Prompts, responses, credentials, baggage and arbitrary headers are never exported.

## What is stored, and where

Everything is under the data directory, `~/.local/share/local-base` by default, which is created readable by your user only (0700).

| What | Where | Notes |
| --- | --- | --- |
| Settings | `local-base.db` | **Your Hugging Face token and OTLP headers, if you set them, are stored here unencrypted.** The file itself is currently 0644, so the 0700 data directory is what keeps other users out. Keep those permissions |
| API keys | `local-base.db` | Stored as hashes. The secret is shown once |
| Browser sign-in secrets | `ui-access.json` (0600) | Provider secrets |
| Logs | `logs/` | Redacted, with no prompts or responses. Rotated at 10 MiB, five archives |
| **Detached run checkpoints** | `runs/<id>.json` (0600) | **Model output in plaintext.** Prompts aren't stored. Deleted when dismissed, or 30 minutes after the run ends |
| Custom voices | `voices/` (0700, files 0600) | The reference audio you added |
| Models and runtimes | `models/`, `bin/runtimes/` | Downloaded files |

**Chat history is stored in your browser**, not on the server. Clearing your browser's site data removes it.

## Diagnostics bundles

`local-base diagnostics` creates a redacted ZIP on your machine and sends nothing. It holds hardware details, configuration shape, model names and recent log events, and never `runs/`. Look through it before sharing it.

## Removing your data

`local-base uninstall --yes` removes the data directory. See [uninstall](install.md#uninstall). After a rollback to an older version, delete `runs/*.json` by hand, as [the upgrade guide](upgrade.md#roll-back) explains.

If you expose LocalBase to other people, you're responsible for their data under the laws that apply to you.
