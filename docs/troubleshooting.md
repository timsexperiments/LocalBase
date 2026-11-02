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
| `503 insufficient_memory` | Not enough memory is available to start the model right now | Close other apps, wait for idle models to be evicted, or use a smaller model |
| `503` with `memory_pressure` or `memory_emergency` in the logs | The system ran short while running, so LocalBase evicted or stopped runtimes | Retry after `Retry-After`. Run less at once |

`local-base models recommend` lists what fits. `--bypass-memory-check` on `serve` starts despite memory warnings, and can make the machine swap or kill processes, so use it only when you understand that risk.

**On hosts with an NVIDIA GPU:** memory admission and placement for NVIDIA aren't qualified on real hardware yet. Rejection logs say which pool refused.

## Queues

`429 inference_queue_full` and `429 inference_queue_timeout` mean too many requests are waiting for one model type. Retry after `Retry-After`, or raise `--inference-queue-capacity` and `--inference-queue-timeout-ms` on `serve`. See [operations](operations.md).

## Ports

- **The gateway won't start.** Something else may hold port 2273: `lsof -nP -iTCP:2273 -sTCP:LISTEN`. Change it with `local-base configure` or `--port`.
- **A runtime won't start.** It may be a runtime port; see the [port table](configuration.md#default-ports).
- **Another gateway is using the data directory.** Each root allows one gateway at a time. Use a different `--root` for a second one.

## Sign-in

- **`/app/login` returns `ui_access_denied`.** No provider is configured, which is the default on a fresh install. Run `local-base access show`, then follow [authentication](authentication.md).
- **OIDC errors after sign-in.** The redirect URI at the provider must be exactly `<origin>/oidc/callback`, and `--origin` must be the exact HTTPS origin people visit, with no path or trailing slash.
- **Locked out.** The local CLI still works: use `local-base access` from the machine that holds the data directory.
- **API key refused in the browser.** That's expected: the browser accepts only sign-in sessions.

## Service

- **macOS: the binary exits with code 137 right after an upgrade.** It was probably copied over in place. [Replace it by rename](upgrade.md).
- **Linux: the service stops when you log out.** Run `loginctl enable-linger "$USER"`. See [running as a service](install.md#running-as-a-service).
- **Logs on Linux** are in the user journal, as well as `local-base logs`.
