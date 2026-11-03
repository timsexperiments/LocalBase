# Why a model install failed

A failed install says why. `local-base models install` prints `Reason: …` after its message, and adds `error.reason` to its JSON error. The install operation carries the same value in `operation.error.reason`.

| Reason | When | What to do |
| --- | --- | --- |
| `network` | Connection refused or cut off, a short body, HTTP 429 or 5xx | Check your connection, then retry |
| `timeout` | A deadline or client timeout, HTTP 408 or 504 | Retry |
| `disk_full` | The free-space check, or the disk filled while writing | Free up space under the data directory |
| `checksum_mismatch` | Wrong hash or size, or a `Content-Length` that disagrees with the catalog, such as an HTML page answered with 200 | Retry. If it repeats, check for a proxy or captive portal, then report it |
| `gated_or_auth_required` | HTTP 401 or 403 | Accept the model's terms at its source and set a token: `HF_TOKEN`, or an auth profile for a private host |
| `not_found` | HTTP 404 or 410 | The file moved. Update your catalog, or report it for a built-in model |
| `interrupted` | Cancelled, or the gateway stopped | Install again |
| `unknown` | Anything else, including other local file errors and failures from before reasons existed | Check `local-base logs --level error` |
