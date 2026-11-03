# Host and admission API

Two read-only routes describe the machine and predict what a model request would meet. They start and stop nothing. An unknown figure is `null`, never 0. These are `/_localbase/` routes, so they aren't part of the stable `/v1` API.

## `GET /_localbase/host`

Needs `models:read`.

```json
{"host": {
  "name": "Kitchen Mac", "nameSource": "setting",
  "memory": {
    "total": {"bytes": 68719476736, "source": "system"},
    "available": {"bytes": 42949672960, "source": "system"},
    "pressure": "healthy",
    "localBase": {"bytes": 5368709120, "source": "mixed", "runtimes": [
      {"runtime": "llm", "modelId": "…", "bytes": 3221225472, "source": "measured",
       "plannedBytes": 4294967296, "measuredBytes": 3221225472}]},
    "other": {"bytes": 20401094656, "source": "derived"}
  },
  "activity": {
    "llm": {"configured": true, "inFlight": 1, "queued": 0},
    "stt": {"…": "…"}, "tts": {"…": "…"}, "image": {"…": "…"}, "video": {"…": "…"},
    "runs": {"active": 0}, "busy": true
  }
}, "capturedAt": 1793000000000}
```

| Field | Meaning |
| --- | --- |
| `memory.total.source`, `memory.available.source` | `system` or `unavailable` |
| `memory.localBase.source` | `measured`, `planned`, `mixed` or `none` |
| `runtimes[].source` | `measured`, or `planned` while starting or where the platform can't sample it, which is Linux today |
| `memory.other` | Total minus available minus LocalBase, clamped at 0. `null` if total or available is unknown. Includes what the system holds |
| `memory.pressure` | `healthy`, `constrained`, `critical` or `null`. It's the state admission refuses on, from available memory against the reserve, not a macOS pressure API |
| `activity.busy` | True when any type has a request in flight or queued, or a detached run hasn't finished |

`memory` covers the system pool. On a discrete GPU, VRAM isn't in it.

**Computer name:** `PUT /_localbase/computer-name` with `{"name": "…"}` sets it (`models:manage`, at most 64 characters). An empty name goes back to the machine's. It stays out of logs and diagnostics.

## `GET /_localbase/models/{id}/admission`

Needs `models:read`. It says what a request would meet now, using the real admission path. It returns `modelId`, `runtime`, `admitted`, `requestedBytes`, `reserveBytes`, `availableBytes`, `pool`, `demandSource` and `refusal`.

- **`refusal`** is the refusal's own status, code, message and `Retry-After`, plus `permanent`.
- **`availableBytes`** counts this runtime and any idle one that recovery could stop, so it can exceed what's free now.
- **A model not on disk** answers `admitted: false` with `model_install_consent_required`. For a chat model its figures are then `null`.
- **`?assumeInstalled=1`** answers the memory question alone for a model not yet on disk. `demandSource` says where the figure came from (`estimated`, `measured`, `feed`, `observed` or `derived`), and an estimate can change once the file can be read.
- **A video refusal** has `jobFailure: true` and `status: null`, because the request is accepted and the job then fails.

**It doesn't promise readiness:** a model can pass this check and still fail to start.
