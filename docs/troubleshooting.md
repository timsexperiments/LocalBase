# Troubleshooting

Start with these:

```sh
local-base doctor        # hardware, configuration and fit
local-base status        # service and gateway readiness
local-base logs --level error
local-base diagnostics   # a redacted ZIP to attach to a bug report
```

## Memory refusals

| Response | Meaning | What to do |
| --- | --- | --- |
| `422 model_too_large` | This model can never fit on this machine | Use a smaller model. Retrying won't help |
| `503 insufficient_memory` | Not enough memory is free right now | Close other apps, wait for idle models to be evicted, or use a smaller model |
| `503` with `memory_pressure` or `memory_emergency` in the logs | The system ran short while running, so LocalBase evicted or stopped runtimes | Retry after `Retry-After`. Run less at once |

See [why a model was refused](model-refusals.md) and [the memory reserve](memory-reserve-guide.md). `local-base models recommend` lists what fits. `--bypass-memory-check` on `serve` starts despite memory warnings, and can make the machine swap or kill processes, so use it only when you understand that risk.

**On hosts with an NVIDIA GPU:** memory admission and placement for NVIDIA aren't qualified on real hardware yet. Rejection logs say which pool refused.

**A model list with `degraded: true`, or `X-Localbase-Warning` on `/v1/models`:** a read behind the list failed, usually because the database stayed busy. The list is still given, without the figures that needed that read and with `unknown` naming the models a `fit` filter can't place, and the log has a `models.read-failed` event naming the step. Try again in a moment.

## Downloads

A failed install prints a reason. See [install failures](install-failures.md).

## Chat requests

`400 remote_media_not_allowed` means a content part named a web address. Send the content itself: `image_url.url` must be a `data:` URL, and audio and files must hold base64, an id or a `data:` URL. A client that replays its history has to replace an address in an earlier message too. See [API details](api-details.md#image-and-video-inputs).

## A busy database

`The database is busy: the gateway or another command is writing to it` means something else held the database for longer than the 5 seconds a command waits. Try again in a moment. If it keeps happening, stop the gateway, or whatever else is writing, and try again. With `--json`, the error has the reason `database_busy`.

## Queues

`429 inference_queue_full` and `429 inference_queue_timeout` mean too many requests are waiting for one model type. Retry after `Retry-After`, or raise `--inference-queue-capacity` and `--inference-queue-timeout-ms` on `serve`. See [operations](operations.md).

## Ports

- **The gateway won't start.** Something else may hold port 2273: `lsof -nP -iTCP:2273 -sTCP:LISTEN`. Change it with `local-base configure` or `--port`.
- **A runtime won't start.** It may be a runtime port; see the [port table](configuration.md#default-ports).
- **`Refusing to serve as root`.** A gateway runs the backends it downloads and reads model files from anywhere, so run it as an ordinary user. In a container that holds nothing else, `local-base serve --allow-root` goes ahead.
- **Another gateway is using the data directory.** Each root allows one gateway at a time. Use a different `--root` for a second one.

## Sign-in

- **No way to sign in.** No provider is configured, which is the default on a fresh install. On this computer, run `local-base access login-link` and open the link. For a phone or another computer, follow [authentication](authentication.md).
- **"This account has no access here".** You signed in, and this LocalBase gives that address nothing, so you were not signed in. The page gives one next step, to run on the computer holding the data directory: `local-base access policy starter --email <address>` if there is no policy yet, or `local-base access users invite --email <address> --roles <role>` if there is. With several providers, it adds `--issuer <issuer>`.
- **OIDC errors after sign-in.** The redirect URI at the provider must be exactly `<origin>/oidc/callback`, and `--origin` must be the exact HTTPS origin people visit, with no path or trailing slash.
- **Locked out.** The local CLI still works: use `local-base access` from the machine that holds the data directory.
- **API key refused in the browser.** That's expected: the browser accepts only sign-in sessions.

## Service

- **macOS: the binary exits with code 137 right after an upgrade.** It was probably copied over in place. [Replace it by rename](upgrade.md).
- **Linux: the service stops when you log out.** Run `loginctl enable-linger "$USER"`. See [running as a service](install.md#running-as-a-service).
- **Logs on Linux** are in the user journal, as well as `local-base logs`.
