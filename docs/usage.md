# Usage and activity

LocalBase keeps hourly totals of what your apps ask it to do, so you can see which key is busy, which model, how many tokens, how often requests fail, and how slow they are.

**Counts only, stored on this computer, never prompts or replies; turn off or clear any time.**

Recording is on by default, because it is local and holds no content. Open **Usage** from Settings or Admin, or run:

```bash
local-base usage                              # the last 7 days
local-base usage --range 30d --by key --by model
local-base usage --json
```

## What is stored

One row per hour, key or person, route, model and status:

- Request counts, input and output token totals, and how many requests reported tokens
- Durations: a sum, the time spent queued, and a latency histogram
- The start of the hour, which is the only timestamp

**Never stored:** prompts, replies, request or response bodies, file names, key material, headers, IP addresses, user agents, emails, names, request ids, a timestamp for any one request, or error messages. A person who signed in through a browser is an opaque hash. A key's label is looked up when you read usage, and is not copied into the table.

Nothing here leaves your computer. There is no network call and no telemetry, and if you have set up OpenTelemetry, this table is not added to what it exports. It is also left out of the diagnostics bundle.

Percentiles are approximate, within about 12%, because only hourly totals are kept. There are no cost estimates: a local model has no per-token price.

## What counts

| What happened | Counts as | In the error rate |
| --- | --- | --- |
| A successful request | a request | No |
| Refused for its body, its permissions, a full queue or a model that cannot fit (4xx) | `refused` | Yes |
| The gateway's deadline expired while a request's body was arriving (408) | `refused` | Yes |
| The client went away (499) | `cancelled` | No |
| A server or backend failure (5xx) | `failed` | Yes |
| A stream that fails after it started, though its status was 200 | `failed` | Yes |
| A detached run, or a request from Chat or the Studio | once, by how it ended | By how it ended |
| A video job | once, when it ends | By how it ended |
| A request with no valid key | not counted; the logs have it | |
| Listing models, administration, pages and health checks | not counted | |

## Who sees what

| Who | Sees | Can clear and change settings |
| --- | --- | --- |
| Any key or signed-in person | Their own usage | No |
| `models:manage` or `system:read` | Everything | No |
| `system:manage` or `access:manage` | Their own, or everything if they also hold one of the above | Yes |
| The CLI, on the computer running LocalBase | Everything | Yes |

A key asking for more than its own gets a 403, not an empty answer. The default key from `local-base configure` and the keys from **Connect an app** cannot clear anything, on purpose: a key handed to an editor should not be able to erase records. The first-run sign-in link can, since it has every permission.

## Keeping it, and clearing it

Totals are kept for 30 days by default.

```bash
local-base usage retention 90      # 1 to 365 days
local-base usage recording off     # nothing new is counted; what exists stays
local-base usage clear --yes       # remove every total
local-base usage clear --yes --before 2026-10-01T00:00:00Z
```

With a gateway running on the root, these go through it, so what it holds in memory goes too. If it doesn't answer, they refuse and change nothing. With no gateway running, they write to the database directly.

The API is `GET` and `POST /_localbase/usage`, described at `/openapi.json`.

Reading from the CLI with a gateway running can be up to 30 seconds behind, and a crash can lose up to the last 30 seconds of counts. The page and the API include what hasn't been written yet.
