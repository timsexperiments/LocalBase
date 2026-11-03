# LocalBase in Go: design

Status: draft for review. Branch `rewrite/go`. Nothing here is merged or deployed.

The TypeScript/Bun implementation on `main` is the specification. It has run in
production, its tests encode real incidents, and its on-disk data belongs to a
live install. This rewrite keeps what that history proved necessary and drops
what only existed because of how the first version grew.

## 1. Goals and non-goals

Goals:

- One static binary (`localbase`, installed as `local-base`) that serves the
  OpenAI-compatible API, the admin API, the browser UI and the CLI.
- Take over an existing `~/.local/share/local-base` root in place, with no data
  loss and a working rollback to the TypeScript binary.
- Keep the HTTP contracts that existing clients use: t3-opencode,
  naya-device-worker, decision clients and the browser.
- Far less code. The TypeScript tree is about 60k lines of production code and
  58k lines of tests, without the UI. The target is under 15k lines of Go
  production code for full parity, and the UI in a few thousand lines of
  templates.

Non-goals:

- New inference features during the rewrite.
- Multi-model process pools. One active model per modality stays the rule.
- Windows.

## 2. Essential and accidental complexity

Line counts are production code, without tests.

### Essential: the reasons LocalBase exists

| Concern | Why it's essential | TS size |
| --- | --- | --- |
| Backend supervision | Start, health-check, restart with backoff, drain and stop llama-server, whisper-server, tts-server and sd-server. Capture their stderr. Kill the process tree on shutdown. | supervisor, launcher, speech-supervisor: ~2.4k |
| Memory admission | A 64 GB unified-memory Mac can't hold everything. Admission order is observed > measured > feed > estimate. Reserves, the pressure monitor and emergency stop are needed. Switch preflight credits a releasing runtime at min(projection, footprint) (#355). Decision models cap the per-slot context at 8192 (#353). The GPU working-set cap applies to the sum of resident runtimes (#283). | memory-*, admission-demand, launch-plan: ~3.9k |
| Per-modality FIFO queues | Bounded waiting room (16 waiters, 60 s), ordering across model switches, 429 `inference_queue_full`/`inference_queue_timeout`, cancellation. | inference-queue, modality-admission: ~450 |
| OpenAI-compatible proxying | Chat (SSE), embeddings, systemone, audio, images and videos, with the error envelope, the 4096 `max_tokens` default, structured-output schema limits and the tool-calling gate. | most of serve.ts |
| Catalog | Pinned, checksum-verified artifacts. A user catalog in `catalog.d/`, a remote feed, Hugging Face discovery. | catalog*, models/: ~8.9k |
| Auth | API keys with scopes. Browser identity through Cloudflare Access, OIDC (live uses Google) and magic link. Roles, bindings, users and sessions. | auth/: ~3.9k |
| Durable state | SQLite: config, keys, auth, observed memory, runtime capabilities, voices. | db, manager.ts: ~1.8k |
| Observability | Redacted JSONL with rotation. OTLP export that fails quietly (#358). Bounded service stderr (#357). | observability/: ~3.8k |
| Service management | launchd/systemd units, start/stop/status, the smoke check. | service/: ~2.9k |

### Accidental: complexity this rewrite removes

1. **God files.** `serve.ts` is 5.6k lines. It holds routing, auth, per-route
   handlers, queues, telemetry and lifecycle. `runtime-reconciler.ts` (1.6k)
   and `supervisor-factory.ts` (1.3k) exist mostly to wire those pieces
   together. In Go, each route gets a handler of about 50–150 lines, and one
   `runtime.Manager` replaces the reconciler, registry and factory.
2. **Runtime validation at internal boundaries.** Zod parses values that
   other LocalBase code built a moment earlier. Go's types replace that.
   Validation stays only at real boundaries: HTTP bodies, catalog JSON,
   database rows and config files.
3. **Three TTS backends.** These are cold `llama-tts` per request, warm
   `tts-server`, and Kokoro, which ships a bundled Bun runtime. Keep
   `tts-server` only. Kokoro is dropped (§11).
4. **The `/app/api/*` mirror and the UI's JSON client.** The React app calls a
   mirror of every API route under `/app/api` with a cookie session, and
   re-parses every response. With a server-rendered UI, page handlers call the
   same Go services directly. The mirror goes away, along with roughly 1k
   lines of `client.ts` and its tests.
5. **Service ownership leases.** `ownership.ts` (900 lines), the
   `/_localbase/instance` lease endpoint, root-initialization locks and owner
   files all exist to tell a foreground `serve` apart from a managed one across
   processes. Replace them with one `flock` on `<root>/runtime/gateway.lock`
   held for the process lifetime, plus the service manager's own status.
6. **The pending-restart fingerprint for declarative config: kept (M5).**
   The plan was to drop the `config_activation` table and the "acknowledge
   static settings" handshake. M5 kept them: they are about 150 lines, the
   fingerprint is the TS one, and keeping them means `pendingRestart`, `config
   plan --detailed-exit-code` and `config apply --wait` behave the same on
   either binary, and each reads the other's record during cutover and
   rollback. `configure` and `config apply` record a pending restart when the
   listener or memory policy changes; a managed gateway clears it when it is
   ready with exactly those settings.
7. **The OpenTelemetry SDK.** It brings 11 packages, a diag-echo filter, and
   batch processors whose failures needed their own reporter. In Go, a small
   OTLP/HTTP exporter (logs and traces only, hand-encoded protobuf, the one
   protocol TS supports) uses a bounded queue per signal, one goroutine each,
   a 5 s deadline, and the #358 outage-reporting rules built in (M5).
8. **Interactive prompts** (`@inquirer`). Every command already accepts
   `--non-interactive` with flags. Go keeps the flags and drops the prompts.
   `configure` without flags prints what to pass.
9. **Release machinery inside the app.** Runtime package receipts,
   hard-link-rejecting publication helpers and multi-format archive extraction
   (zip, tar.gz, binary) can be simpler. Downloads are still checksum-pinned
   and extracted to `bin/runtimes/<family>/<sha256>/`, but a single
   `archive/tar` plus `archive/zip` path replaces fflate and tar-stream.
10. **Response validation for structured output.** The TS code compiles the
    JSON schema with ajv to check limits, then validates non-streaming
    responses. Keep the limits check and the
    `structured_output_validation_failed` 502, using a Go validator of about
    300 lines for the documented subset only. Don't add a general JSON Schema
    library.
11. **Duplicate logs.** Console mirroring plus a file sink plus launchd
    stderr needed #357's suppression logic. In Go, slog writes once to the
    JSONL sink. Stderr only carries crashes and early-startup text.

## 3. Target architecture

```
go/
  cmd/localbase/main.go     CLI entry: subcommand dispatch with flag.FlagSet
  internal/
    root/       root dir, marker, paths, the gateway flock
    store/      SQLite (modernc), embedded drizzle migrations, typed queries
    catalog/    built-in (embedded) + catalog.d + feed; ModelSpec; lookup
    auth/       permissions, principals, API keys, browser sessions, OIDC, CF Access, policy
    memory/     host snapshot (darwin/linux), estimates, measurements, admission controller
    runtime/    launch plans, supervisor, per-modality manager + FIFO queue
    gateway/    HTTP router, OpenAI handlers, admin handlers, error envelope
    web/        server-rendered UI: templates, static assets (htmx, CSS, small JS)
    obs/        slog JSONL sink (redaction, rotation), OTLP exporter, stderr capture
    service/    launchd / systemd unit rendering and control
  contract/     black-box contract suite (runs against TS or Go by URL)
  tools/        build helpers (tailwind fetch/verify)
```

Each package owns its data and exposes a few concrete types. Interfaces exist
only where tests need a fake: `memory.HostProvider`, `runtime.Process` and
`auth.Clock`. There's no DI container. `cmd/localbase` wires the packages in
`serve`.

Dependency direction runs one way, so there are no cycles:
`gateway, web → runtime → memory → catalog → store → root`, and
`gateway, web → auth → store`.

### Dependencies

Go standard library first. Every addition needs a reason:

| Module | Why |
| --- | --- |
| `modernc.org/sqlite` | Pure-Go SQLite, so `CGO_ENABLED=0` still produces a static binary and cross-compiling to linux/amd64 is trivial. |
| `internal/third_party/norm` (vendored, not a module) | Unicode NFC/NFKC for voice names (TS `String.prototype.normalize`). A copy of the Go go1.26.0 toolchain's vendored `golang.org/x/text/unicode/norm` (x/text v0.32.0, BSD-3-Clause, Unicode 15.0). Provenance and edits in its README; `PINS` and `norm_pins_test.go` fail on drift. |
| (none else) | Dropped after review (§11): purego (Metal working set comes from `llama-server --list-devices`) and a TOML library (a small reader like TS's `toml.ts`). |

Not added: an HTTP router (Go 1.22+ `ServeMux` patterns cover every route), a
CLI framework, a logging library, the OTel SDK, a JSON Schema library, a JWT
library (RS256/ES256 verification with `crypto` and JWKS is about 150 lines),
or an ORM.

### Request path

```
net/http → gateway.Router (ServeMux, method+path patterns)
  → middleware: request id, recover, access log (obs), body limits
  → auth.Resolve(principal) → route permission check (401 / 403)
  → handler: decode + validate body → runtime.Manager.Acquire(ctx, modality, modelID)
       Acquire = FIFO queue wait (bounded, 60 s) → ensure model running
                 (switch preflight → stop old → admission → start → health)
                 → lease (held until the response body completes)
  → reverse proxy to the backend (SSE flushed per chunk) → release lease
```

`runtime.Manager` holds one `modalitySlot` per modality (llm, stt, tts, image,
video). Each slot has the FIFO waiter list, the current supervisor, an
active-lease count and the resolved slot capacity. The TS reconciler loop
becomes synchronous code under the slot's mutex. Requests change the model,
and a config change does the same through `Manager.Apply(config)`.

### Memory admission (`internal/memory`)

This is a direct port. The formulas carry incident history, so the Go code
reproduces them byte for byte and the table tests are ported from TS:

- `Estimate*` covers llm, stt, tts, image and video (memory-estimate.ts), with
  GGUF geometry read from the file header (`gguf.go`).
- `Measurements` is the checked-in record table, embedded as JSON generated
  once from `memory-measurements.ts`.
- `Demand(launch)` applies the order observed (macOS footprint, exact sha256
  and shape, plus max(512 MiB, 10%)) > measured > feed > estimate.
- `Controller` holds reservations (pending or resident, by pid). `Reserve`,
  `CheckAdmission(releasing)` with min(projection, footprint) credit,
  `Release`, the pressure hysteresis (3 normal samples to recover) and the GPU
  working-set cap on the sum of resident runtimes.
- Switch preflight for media models follows the TS supervisor factory: a
  catalog image or stt model whose files are missing is still sized from the
  catalog (primary artifact, `expectedSizeBytes`) and fit-checked before the
  install check, so too little memory answers 503 `insufficient_memory` (422
  `model_too_large` when it can never fit, §13.6) and leaves the model
  inactive; tts has no switch preflight in TS. Go once
  skipped the check for image models without an `imageRuntime` profile, which
  a full contract run exposed (realvis-xl-v5.0 needs 15.5 GB above the 9.6 GiB
  reserve on a 64 GB host). `TestMediaAdmissionGolden`
  (`tools/golden/media-admission.ts`) pins the demand and the verdict around
  each model's threshold.
- Idle-eviction recovery (`runtime.IdleRecovery`, TS
  `admitModelWithIdleRecovery`): a model refused for memory, short of a
  permanent shortfall, claims the idle runtimes of the other modalities
  (running, no lease, nothing queued). If admission passes with them
  credited, they are stopped, the host is sampled until the model is
  admitted on uncredited memory (10 s, every 100 ms), and the switch runs
  again. A retry refused for memory again answers the first refusal. Without
  it, a full run had TS stop whisper and the embedding model and answer 409
  while Go answered 503.
- Pressure-driven stops (`runtime.PressureMonitor`, TS
  `MemoryPressureMonitor` with serve's `applyElevatedMemoryPressure`). The
  host is sampled every 2 s without overlapping samples. The thresholds are
  the TS ones: critical below the reserve (9.6 GiB on a 64 GB host),
  constrained below 1.25 × the reserve, and 3 normal samples to recover;
  constrained after critical stays critical, and an unknown sample keeps the
  state and resets the count. Every constrained sample stops the idle
  runtimes (running, no lease, nothing queued), so requests in flight keep
  theirs. On entering critical, queued work gets 503 `service_unavailable`
  for "Inference" (TS `InferenceQueueUnavailableError`) and every runtime
  stops, active ones included, as TS `evictAllRuntimes` does. Each state
  change logs `runtime.memory-pressure-changed` (warn, error, or info on
  recovery). Failing video jobs (a constrained sample that found nothing
  idle outside the 10 s reclaim grace, or critical) is wired as
  `PressureMonitor.FailVideo` for M6. `TestMemoryPressureGolden`
  (`tools/golden/memory-pressure.ts`) runs six sample sequences through the
  real TS controller and monitor and checks the Go state, recovery count,
  action, stops and events at each sample. `TestPressureStopsRuntimes` drives
  a fake sampler into constrained and critical with a real LLM and STT
  runtime on the fake backend. Before this, Go only stopped the LLM on
  critical, every 5 s, and stopped nothing on constrained.
- `runtime.memory-admission-rejected` (TS supervisor and reconciler). Every
  memory refusal carries TS's diagnostics (`AdmissionError.Attributes`):
  reason, pool, measured, reserve, pending, requested and effective
  available bytes ("unavailable" for the unsampled GPU working set), the
  sample time and age, the provider's pressure reading, the safety state,
  the recovery count and the demand confidence.
  `TestAdmissionDiagnosticsGolden` (`tools/golden/admission-diagnostics.ts`)
  replays nine scenarios against the real TS controller. The event is logged
  when a start is refused (no stage; the component is the launched server),
  when a switch preflight is refused (stage `switch-preflight`, `from_model`,
  `to_model`), and after the previous model stopped (stage `post-stop`). The
  post-stop case is the TS settle and rollback, also ported here: after the
  previous model stops, admission is sampled for up to 10 s (every 100 ms);
  if memory never settles, the previous model becomes active again,
  `model.switch-rolled-back` is logged with `reload`, and it reloads once
  memory admits it (LLM: at once). Go previously started the new model
  immediately, so a slow page return left nothing loaded.
- `HostProvider`: darwin uses `host_statistics64` free+inactive (vm_stat
  free + speculative + inactive, since vm_stat prints speculative pages apart),
  `hw.memsize`, `kern.memorystatus_vm_pressure_level` and the Metal working
  set. Linux uses `/proc/meminfo` MemAvailable plus PSI. NVIDIA VRAM is not
  admitted yet: an NVIDIA GPU is detected (`nvidia-smi`) and reported, and
  admission uses the system pool (§19.3).
- The observed-peak sampler runs `footprint -p <pid>` every 5 s on darwin, or
  reads VmHWM on Linux, and writes `model_observed_memory`.

## 4. Persistence and parity of on-disk state

### Root layout (unchanged)

```
<root>/.localbase-root.json   marker {version, root, rootHash, identity}; read, never rewritten
<root>/local-base.db          SQLite, same schema and the same __drizzle_migrations journal
<root>/catalog.d/*.json       user catalog entries, same format
<root>/models/{llm,stt,tts,image,video}/   artifacts, same filenames
<root>/bin/runtimes/<family>/<sha256>/      managed runtime packages, same pins
<root>/voices/<id>.wav        custom voice references (0700 dir, 0600 files)
<root>/logs/events.jsonl(.1-.5) and service-stderr.log(.1)
<root>/cache/{catalog-feed,hf}
<root>/ui-access.json         browser provider config (secrets stay in this 0600 file)
<root>/gateway-listener.json  last bound host:port
<root>/runtime/gateway.lock   flock
<root>/service/manifest.json  service definition record
```

### Database

The Go binary embeds the same `drizzle/*.sql` files and `meta/_journal.json`.
Drizzle's journal row is `(hash = sha256(file bytes), created_at = journal
"when")`. The Go migrator applies missing migrations in order, splits on
`--> statement-breakpoint`, writes byte-identical journal rows, and refuses
to start if the journal has rows this build doesn't know (a newer TS binary
was there). So:

- Go opens a TS-created database with no migration.
- TS opens a Go-touched database, because the journal still matches its build.
  **Rollback is replacing the binary.**
- Connections set `PRAGMA busy_timeout = 5000` and `foreign_keys = ON` (#359).
  Writes use `BEGIN IMMEDIATE`, and the journal mode stays rollback (no WAL),
  matching TS.
- New schema changes after cutover are added as new drizzle-format SQL files
  with journal entries, so the format stays one lineage. Once TS is retired,
  the journal format can be frozen as is.

Data covered: `config` (one row, id `default`; selections are JSON arrays),
`api_keys` (sha256 hex of the raw `lb_…` key, scopes as a JSON array in
canonical order, ISO timestamps), all `auth_*` tables, `auth_browser_sessions`
(epoch ms), `model_runtime_capabilities`, `model_observed_memory` and
`tts_custom_voices`.

### Catalog JSON

The `modelSpec` format is read as is: built-in entries (embedded), `catalog.d/`
entries, and feed v2 documents. Strict decoding (`DisallowUnknownFields`)
matches Zod's `.strict()`. The built-in catalog is copied from
`src/catalog/models/**` at build time by `go generate`, so both
implementations read one source until TS retires. When #368 lands, its
"former entries become user entries" rule is ported as is.

## 5. Parity matrix

Keep = same contract. Simplify = same client-visible contract, less machinery.
Drop = removed, with the reason.

### HTTP routes

| Route | Decision | Notes |
| --- | --- | --- |
| `GET /health` | Keep | Public liveness. |
| `GET,HEAD /health/ready` | Keep | 200 `ready` + modalities / 503 `unready`. |
| `GET /v1/models` | Keep | Same entry shape: `context_length`, `context_budget`, `memory`, `capabilities` and `architecture`. Visibility is filtered by permission. |
| `GET /v1/models/{model…}` | Keep (#362) | Percent-decoded, the rest of the path is the ID, 404 `model_not_found`, 405 with `Allow: GET`. |
| `POST /v1/chat/completions` | Keep | SSE passthrough. The lease is held until the body completes. 4096 default `max_tokens` (`LOCALBASE_DEFAULT_MAX_TOKENS`). Tool gate. JSON-schema limits. Decision models are rejected with 400 `unsupported_model_capability`. |
| `POST /v1/embeddings` | Keep | Embedding-only models. |
| `POST /v1/systemone` | Keep | `inference:decision`. Per-slot context capped at 8192. |
| `POST /v1/audio/transcriptions`, `/translations` | Keep | Multipart to whisper-server. |
| `POST /v1/audio/speech` | Simplify | Warm `tts-server` only. Same request and WAV response contract. |
| `POST /v1/images/generations`, `/edits` | Keep | Same input validation limits (2048 px per side, 4.2 MP, 20/8 MiB). |
| `POST /v1/videos`, `GET /v1/videos/{id}`, `/content`, `/cancel` | Keep, milestone 6 | Job manager, AVI→MP4 through the pinned converter. Last to port. |
| `GET,POST /_localbase/model-management` | Keep | Install, uninstall, enable, disable and activate. Install returns 202. |
| `GET,POST /_localbase/model-discovery` | Keep | HF search, file choice. |
| `GET /_localbase/models`, `/_localbase/models/{id}` | Keep | Metadata. Never starts runtimes or hashes files. |
| `GET,POST /_localbase/api-keys` | Keep | create, rotate, revoke and set-scopes. |
| `GET,POST /_localbase/access-management` | Keep | Providers, policy, users. |
| `GET,PUT,DELETE /_localbase/hf-token` | Keep | |
| `/_localbase/voices…` | Keep (#366) | Same limits: 3–30 s, 12 MiB, 32 voices, consent attestation. WAV decode and resample in Go. |
| `GET /_localbase/instance` | Keep | Identity proof by instance token (guards against PID reuse); either CLI uses it during cutover and rollback. The lease and handshake around it are dropped (§2.5). |
| `/app/api/*` mirror | Drop | Only the React UI used it (§2.4). Browser sessions on `/v1/*` keep working for UI-originated fetches such as chat streaming. |
| `/`, `/app`, `/app/login*`, `/app/logout`, `/app/session` | Simplify | Server-rendered pages (§6). `/app/session` stays for expiry probing. |
| `/oidc/callback`, `/magic-link/callback` | Keep | |
| `/github/callback` | Drop (§11) | Not used by the live install. |
| Unmatched routes | Keep | 404 `route_disabled`, plus `route_family`/`route_segments` log hints (#362). |

Cross-cutting behavior that stays:

- `Authorization: Bearer` or `x-api-key` (`--auth-mode`).
- The `LOCALBASE_API_KEY` environment credential has every permission.
- API keys are ignored on browser requests (those with `Origin` or
  `Sec-Fetch-Site`).
- 401 `invalid_api_key` with `WWW-Authenticate: Bearer`, and 403
  `insufficient_permissions`.
- Unread request bodies are drained before an error reply (#279, #305).
- Long silent inference keeps the connection open (#277).
- A client cancel doesn't kill a healthy llama-server (#276).
- A failed switch restores the previous model (#296).
- Queue 429s carry `Retry-After: 1`.
- `Server-Timing` on non-streaming responses, and `x-localbase-request-id`.
- W3C `traceparent` is forwarded to backends.

### CLI

| Command | Decision | Notes |
| --- | --- | --- |
| `serve` | Keep | `--root`, `--host`, `--port`, `--ctx-size`, `--parallel`, `--no-auth`, `--auth-mode`, `--install-missing`, `--inference-queue-capacity`, `--inference-queue-timeout-ms`, `--bypass-memory-check`, `--json` lifecycle events. |
| `start`, `stop`, `restart`, `status` | Simplify | launchd/systemd through `internal/service`. The plist keeps stderr → `logs/service-stderr.log`, bounded (#357). There's no lease handshake. |
| `init` | Keep | Creates the root marker and default config. |
| `configure` | Simplify | Flags only, no prompts. |
| `config validate/plan/apply/show` | Keep | Same TOML v1 document, validation messages, plans and pending-restart record (§2.6). `--restart auto` restarts the managed service. |
| `models catalog/list/show/install/recommend` | Keep | |
| `models search/add hf:/remove-entry` | Keep | |
| `models feed status/refresh/use` | Keep | |
| `keys list/create/revoke/rotate/scopes` | Keep | Same JSON envelope. |
| `voices list/add/remove` | Keep | |
| `access show/cloudflare/oidc/disable/policy/users` | Keep | |
| `access magic-link`, `access email` | Keep (§11) | |
| `access github` | Drop (§11) | |
| `logs` | Keep | `--follow`, `--level`, `--since`, `--limit`, `--runtime`, `--request-id`. |
| `doctor`, `diagnostics` | Simplify | One report struct. The bundle drops paths and content, as today. |
| `smoke` | Keep (#356, #360) | Temp key, a 4-token chat or one systemone question, then the key is deleted. `--switch-to`. |
| `reset`, `uninstall` | Keep | `--yes` required. |
| Global `--json` envelope `{ok,data}` / `{ok:false,error}`, exit codes 0/1/2 | Keep | |

### On-disk artifacts

| Artifact | Decision |
| --- | --- |
| `.localbase-root.json` | Keep: read and validated. Initialization locks are dropped. |
| `local-base.db` + journal | Keep, byte-compatible (§4). |
| `config_activation` table | Keep: written and acknowledged as TS does (§2.6). |
| `catalog.d/`, `cache/catalog-feed`, `cache/hf`, `catalog-feed.json` (feed URL setting) | Keep. |
| `bin/runtimes/<family>/<sha>/` + receipts | Keep the layout and pins. The receipt is rewritten in the same JSON shape. |
| Legacy flat `bin/llama-server` etc. | Drop, since nothing reads them now. Left on disk. |
| `voices/` | Keep. |
| `logs/events*.jsonl` | Keep the format: the same event names and fields, so `logs` from either binary reads both. |
| `service/manifest.json`, launchd plist | Simplify: the same service label (`com.localbase.gateway.<rootHash>`), so cutover replaces the unit in place. |
| `ui-access.json`, `gateway-listener.json` | Keep. |
| `runtime/gateway.lock/owner.json` | Keep: the same lock-directory protocol (pid and stale detection), so the TS and Go gateways exclude each other on one root. |

## 6. Frontend

### Architecture

The UI is server-rendered by the Go binary:

- `html/template` with one layout and a partial per component. Templates and
  assets are compiled in with `embed`.
- Page handlers live in `internal/web`. They call the same services the API
  uses (`runtime.Manager`, `catalog`, `store`, `auth`) and never go through
  HTTP to themselves.
- Responses are full pages for navigation (real URLs, working Back button,
  linkable details), or fragments when the request has `HX-Request`.
  `hx-boost` on the shell gives SPA-like navigation without client routing.
- Auth: the same browser principal as the API. The resolution is cookie
  session (OIDC or magic link) or `Cf-Access-Jwt-Assertion`, then policy and
  roles. Each handler checks the same permission strings as the matching API
  route, so the UI can't do more than the API allows.
- CSRF: forms are `POST` with a per-session token. htmx sends it through
  `hx-headers` on `<body>`. `Origin` is checked on non-GET requests.

### htmx or small JS

| Feature | Mechanism |
| --- | --- |
| Navigation, tabs, filters, search-as-you-type, install/enable/default actions, key and access admin, HF token, details panels | htmx (`hx-get`/`hx-post`, `hx-target`, `hx-push-url`) |
| Install progress, "Running now" memory bar | htmx polling (`hx-trigger="every 2s"`) that stops when the operation ends. No SSE extension needed. |
| Picker sheet, dialogs | Native `<dialog>` plus 20 lines of JS for open/close and focus return |
| Chat token streaming | `chat.js` (~200 lines): `fetch` to `/v1/chat/completions` with `stream:true` and the session cookie, then read the SSE body. It renders escaped text plus a tiny markdown subset (code fences, lists, tables) by DOM construction, never with `innerHTML`. The htmx SSE extension doesn't fit, because the stream is a POST response tied to one request. |
| Conversation history | Page memory by default. Optional `localStorage` text-only history, as today. |
| Attachments | `<input type=file>` read in `chat.js` (text ≤128 KiB each / 256 KiB total, images ≤5 MiB for vision models) |
| Mic dictation, voice recording | `audio.js` (~150 lines): `MediaRecorder`/`AudioContext` → 16 kHz (dictation) or 24 kHz (voice) mono WAV encoder, then POST |
| Image, video and speech playback | Native `<img>`, `<video>`, `<audio>` |
| Decision lab question builder | htmx form rows (add/remove question fragments) and a server-rendered result |
| Sign-in expiry | `session.js` (~60 lines) plus `htmx:responseError` hooks. A LocalBase 401 shows the sign-in notice and keeps the draft in `sessionStorage`. An opaque redirect, CORS failure or `cf-access` HTML response is reported as "edge sign-in expired, reload" rather than "gateway unreachable" (#363/#364). `/app/session` is probed before claiming the gateway is down. |

The total JavaScript budget is htmx (~50 KB min, ~16 KB gzip) plus under
15 KB of our own unminified modules. There's no bundler. Each module is a
plain ES module served with a content hash in its URL.

htmx is vendored as `internal/web/static/vendor/htmx-2.x.y.min.js` with its
version and sha256 in `internal/web/static/vendor/PINS`. A Go test re-hashes
the embedded file against `PINS`.

### Tailwind build pipeline

- `tools/tailwind.sh` downloads the Tailwind **standalone CLI** (v4, pinned
  version) for the host platform into `go/.cache/`, verifies its sha256 from
  `tools/tailwind.pins`, and runs it over `internal/web/templates/**` and
  `internal/web/static/js/**`. It writes
  `internal/web/static/app.css` (minified).
- **`app.css` is committed.** Tailwind is an approved, pinned build step, but
  committing its output keeps `go build` and `go install` free of the Tailwind
  binary and the network, which keeps CI and contributor builds one command.
  CI runs `tools/tailwind.sh` and fails if
  `git diff --exit-code internal/web/static/app.css` shows a change.
- StyleX was considered and rejected. Its styles are authored in JS/TS,
  compiled by a bundler plugin and applied with `stylex.props()` at render
  time, which needs a JS rendering layer (in practice React), not Go
  `html/template` with htmx.
- Theme: the existing tokens (canvas, surface, text, muted, border, accent,
  error) become CSS custom properties in `@theme`. Dark mode follows
  `prefers-color-scheme`, matching today. A manual toggle is optional later.
- Responsive rules enforced in review and by the contract suite's HTML checks:
  no horizontal scroll from 320 px, 44 px minimum touch targets (`min-h-11`),
  visible focus rings inside scroll containers (#289, #290), and
  `env(safe-area-inset-*)` padding for the iPhone home indicator.

### Page map

Navigation is a bottom tab bar on phones (Chat, Models, Lab, Running, More)
and a sidebar at ≥1024 px (§22.1).

| URL | Page | Parity source | Notes |
| --- | --- | --- | --- |
| `/app` | Chat | app.tsx | Streaming, attachments, model picker sheet (recent chat-capable models only, never decision or embedding models), steer and stop, reasoning disclosure, tool calls (image, video, speech) |
| `/app/models` | **Library** (Shelf) | model-management.tsx, design v3 | Installed models, filter All / Chat / Other tasks, disk and memory summary, API defaults |
| `/app/models/get` | **Get models** | model-discovery.tsx | Guided entry: "What do you want to do?" (from Compass), then catalog and HF search. Family detail with the qualified file first and variants on disclosure. Install progress. |
| `/app/models/running` | **Running now** | design v3 | Shared-memory bar (other apps / models / available), loaded and idle per runtime, unload only when idle |
| `/app/models/{id}` | Model details | model-details.tsx | Provenance, files, capabilities with sources, memory figure and fit, actions |
| `/app/lab` | Model lab | studio.tsx | Image, video, speech (voices), transcription, embeddings |
| `/app/lab/decision` | Decision lab | decision-lab.tsx | Question schema builder |
| `/app/voices` | My voices | voice-profiles.tsx | Record or upload, consent, preview, rename, delete |
| `/app/settings` | Settings | | Dictation model, history opt-in, HF token |
| `/app/admin` | Access & API keys | auth-management.tsx | Providers, policy, users, keys (secret shown once) |
| `/app/login`, `/app/logout` | Sign-in | access.ts | Provider buttons, magic-link form |

UI copy names no models. Model names appear only from data (catalog cards,
running state, the selected model).

Simplified or dropped in the UI:

- Chat auto-compaction (compaction.ts) is kept in the chat UI: older turns fold
  into a model-written summary at 75% of the context. The API never compacts
  (§11). **Keep.**
- Block-wise progressive markdown rendering for very long chats (#297)
  becomes chunked append only. Revisit if long chats get slow on the phone.
  **Simplify.**
- The visibility-aware polling helper is replaced by htmx's
  `hx-trigger="every 2s [document.visibilityState=='visible']"`. **Simplify.**

### Walking-skeleton page

`/app/models` (Library) is server-rendered from the real catalog and DB, with
filter chips through htmx, light and dark, and the 320 px and 44 px rules. It
is reachable with `--no-auth` (loopback only, please). M0 had a
`serve --dev-ui-login` flag for phone testing; M3 removed it in favor of the
real sign-in (OIDC, Cloudflare Access, magic link). M0 also
renders Running now (a live shared-memory bar, polled every 3 s while the page
is visible), a read-only model details page, and a placeholder Get models
page.

htmx is pinned to 2.0.10, the maintained 2.x line. htmx 4.0.0 shipped in
August 2026; evaluate it once the UI has enough surface to justify a
migration. The UI runs under a strict CSP (`script-src 'self'`,
`style-src 'self'`, no inline styles or eval), so htmx runs with
`allowEval: false` and conditional triggers live in `session.js`.

## 7. Migration and cutover

1. **Shadow (now through milestone 5).** Run Go on a scratch root and a
   scratch port, with a copy of live data (`cp` of the DB, symlinked model
   files). The contract suite runs against both binaries. The live service
   isn't touched.
2. **Side-by-side on live data, read-only for the operator.** The Go binary
   runs on a spare port against a *copy* of the live root, with live model
   files mounted read-only by symlink, and serves real traffic from one test
   client (for example a t3-opencode profile).
3. **Cutover (coordinated with the LocalBase operator thread).** Back up
   `local-base.db` with the SQLite backup API. Stop the launchd job. Install
   the Go binary as `~/.local/bin/local-base` (mv-replace, never in-place
   `cp`). Run `local-base restart`, which rewrites the plist under the same
   label. Run `local-base smoke`, including a decision question.
4. **Rollback.** mv the previous TS binary back and `local-base restart`. The
   DB journal is identical, so TS starts. Go-only schema additions aren't
   allowed until TS is retired.
5. **Retire TS** after two weeks with no rollback. Then the `drizzle/` SQL
   files move under `go/internal/store/migrations` and become Go's own.

## 8. Testing strategy

- **Ported unit tests (Go table tests).** These are the TS suites with the
  most incident value:
  - launch-plan (decision cap #353, parallel slots, KV type, context budget
    reasons)
  - memory-estimate and measurements (every calibrated record reproduces its
    `minVramGb`)
  - admission-demand (observed > measured > feed > estimate)
  - memory-controller (#355 credit cap, pending reservations, the working-set
    sum, hysteresis)
  - gguf-metadata (geometry, SWA, recurrent state)
  - inference-queue (FIFO across switches, capacity and timeout)
  - route-dispatch (#362 decoding and hint families)
  - authorization matrix
  - API key resolution
  - structured-output schema limits
  - catalog strict decoding against every built-in JSON
  - drizzle journal compatibility
- **Black-box contract suite (`go/contract`).** `go test ./contract
  -args -base-url=… -root=…` starts or targets any gateway by URL. It's one
  suite for both implementations. The harness launches the TS gateway (`bun
  run src/cli.ts serve`) or the Go binary on a scratch root and port, seeds
  keys directly into the scratch DB, and asserts response status, headers
  and JSON shape: `/health`, `/health/ready`, `/v1/models`,
  `/v1/models/{id}`, 401, 403, 404 `route_disabled`, 405, chat (with a fake
  llama-server or a real 1 GB model), embeddings, systemone errors, queue
  429s and key admin. A shape comparator ignores volatile fields (ids,
  timestamps, measured memory).
- **Fake backends.** `internal/runtime/fakebackend` is a tiny Go HTTP server
  that speaks llama-server's `/health`, `/props`, `/v1/chat/completions`
  (streaming) and `/v1/embeddings`. The supervisor launches it as a real
  process, so supervision, health, restart and kill paths are tested without
  models. It replaces most of the 4.3k-line `serve.test.ts` and the 2.3k-line
  gateway fixture.
- **Real-backend smoke.** This test is opt-in (`LOCALBASE_REAL_BACKEND=1`).
  It uses qwen2.5-coder-1.5b (1.1 GB) and the managed llama-server package
  copied to a scratch root.
- **UI.** Template render tests assert key content and the absence of
  model-name copy in generic strings. A Playwright-free phone check uses the
  T3 device preview at 320/390/1280 px in light and dark during review.

## 9. Milestones

| # | Scope | Exit check |
| --- | --- | --- |
| M0 | DESIGN.md, `go/` module, root/config/DB loading, `/health`, `/v1/models`, API-key auth, LLM supervisor + admission + chat proxy, contract harness, Library page | This PR |
| M1 | Full LLM path: embeddings, systemone, queues, switch preflight and rollback, `/v1/models/{id}`, `/health/ready`, observed-peak sampler, JSONL logging, `/_localbase/models` | Contract suite green against both implementations for LLM routes |
| M2 | CLI: init, configure, keys, models catalog/list/show/install (downloads + checksum), doctor, logs, smoke; service start/stop/status on launchd | `smoke` passes on a scratch root, and a launchd unit on a scratch label |
| M3 | Browser auth (OIDC, CF Access, magic link?), policy, users, sessions, admin API; UI: Chat, Library, Get models, Running now, admin | Phone review on a LAN port |
| M3.5 | Model import and backend selection (§12): backend registry, logical models with platform variants, best-effort detection, per-device overrides, custom adapters, HF/URL/local imports with private header profiles | The spec's acceptance checks pass as end-to-end tests on scratch roots |
| M4 | STT (whisper.cpp), TTS (CrispASR adapter, §12.8), custom voices, images (+edits, LoRA); Lab pages, voices page | Contract suite for media routes |
| M5 | Catalog feed, HF discovery, `config` TOML, diagnostics, OTLP exporter | Feature-complete except video |
| M6 | Video jobs + converter; Linux/systemd qualification | Full contract suite on both platforms |
| M7 | Shadow on live-data copy, cutover with operator, rollback drill | Rollback rehearsed once |

## 10. Milestone 0 status

What this branch contains, and what each part was checked against.

| Area | State | Evidence |
| --- | --- | --- |
| Root, marker, lock | Done | The marker and canonical path match TS (symlinks resolved through the deepest existing ancestor, so `/tmp` becomes `/private/tmp`). The lock uses the TS lock-directory protocol. |
| SQLite + migrations | Done | Byte-identical drizzle journal on a fresh DB. A backup copy of the live DB opens with zero migrations applied. |
| Catalog | Done | All 76 built-in entries and the live `catalog.d` decode strictly. Layering and trust have table tests. |
| LLM launch plan, estimate, measured demand, context budget, argv | Done | Golden test: 90 cases generated by running the TS code (`tools/golden/llm-plans.ts`), 70 of them with geometry read from real GGUF headers (hybrid qwen35, decision, embedding, qwen2). All match exactly. |
| Memory controller | Done | Table tests for admission, permanent shortfall, GPU working-set sum (#283), switch preflight credit capped at footprint (#355), hysteresis, and `vm_stat`/`footprint` parsing. |
| LLM supervisor + FIFO queue | Done | A fake llama-server runs as a real process. Covered: cold start, drain before switch, FIFO across a switch, queue full and timeout, failed switch restores the previous model (#296), preflight rejection keeps the current model, crash then restart on the next request. Passes `-race` repeatedly. |
| `/health`, `/health/ready`, `/v1/models`, `/v1/models/{id}`, `/v1/chat/completions` | Done | The contract suite passes against TS and Go. The differential test finds `/v1/models` identical on matching fixtures, apart from the measured peak's value. Real inference through both with qwen2.5-coder-1.5b (1.1 GB). |
| JSONL events | Done | Schema 2, the same field names, rotation at 10 MiB with 5 archives. OTLP export and the full TS event schema on read came with M5 (§14.2). |
| UI: Library, Running now, details | Done | Rendered and exercised in a browser at 320, 390 and 1280 px, light and dark: no horizontal scroll, 44 px targets, htmx filter and search with URL history, live polling under CSP, edge-expiry probe. |
| CLI | Partial | `serve`, `keys create/list`, `version`. Everything else is M2. |

Deliberate differences the contract suite found and kept:

- `GET /v1/chat/completions` returns 405 with `Allow: POST`. TS returns 400 "Invalid
  JSON payload." because it never checks the method.
- TS refuses to start `serve` when the active LLM's files are missing. Go starts,
  serves `/v1/models`, and reports the missing file when a request needs the
  model, which matches how every other modality already behaves. `--install-missing`
  arrives with downloads in M2.
- SSE validation is reduced to framing and JSON parsing, plus the `model` rewrite
  and an `upstream_error` event for malformed data. TS also validates each
  chunk's schema and the finish/[DONE] ordering. Revisit if a backend ever
  emits a malformed stream in practice.
- Crash recovery is lazy: an exited backend is cleared, and the next request
  starts it again. TS restarts in the background with bounded backoff.

### M1 (done 2026-10-19)

| Area | Evidence |
| --- | --- |
| Capability detection and resolution (GGUF, whisper, image checkpoints, runtime `/props` probe) | Golden over all 76 catalog entries and the real model files (`tools/golden/capabilities.ts`). |
| Memory figures for every kind (STT, TTS, image, video estimates; measurement table embedded from TS) | Golden over all 76 entries (`tools/golden/catalog-memory.ts`); `/v1/models` side by side with five kinds selected. |
| `/v1/embeddings`, `/v1/systemone`, chat validation, tool-schema anchoring, structured-output checks and validation | 40 rejection cases compared side by side (status, type, code, param); real embeddings through both. |
| `/_localbase/models` (+ query parameters) | Side by side over the list, two details and eleven query variants. |
| `inference.completed` and admission events, `Server-Timing`, reasoning normalization, upstream error envelopes, 409 install consent | Gateway tests over the fake backend. |

New deliberate difference: an overflowing prompt answers OpenAI's `400
context_length_exceeded` (param `messages`) instead of relaying llama-server's
`exceed_context_size_error` (§11, decision 4). Feed peaks and OTLP spans land
with M5. Health reports speech, image and video as configured but not
admitting until M4.

### M2 (done 2026-10-19)

| Area | Evidence |
| --- | --- |
| CLI framework: globals anywhere, `{ok,data}` envelopes, exit codes 0/1/2, `--no-x` flags | Unit tests; every CLI contract case compares exit codes and envelopes. |
| `init`, `configure` (flags and `--config` TOML), `keys *`, `models catalog/recommend/list/show/install`, `doctor`, `logs`, `smoke`, `reset`, `uninstall` | CLI contract: each command through both binaries on twin scratch roots; `smoke` against both gateways with real inference; `logs` over both gateways' files. |
| Downloads: resumable, size- and sha256-checked, sharing the TS `.checksums.json` cache and lock | File identity matches the TS cache records (read-only check against the live cache). |
| TOML without a library | Conformance cases generated from Bun's parser. |
| Service: launchd/systemd definitions, manifest, status rules, start/stop/restart | The plist renders byte-for-byte like the live one (fingerprint matches its manifest). A real launchd agent on a scratch root and label: start, status (Go and TS), restart, stop, uninstall, all cleaned up. |
| Log events valid under the TS schema | The TS `logs` command reads every event the Go gateway wrote. |

Kept rather than dropped: `/_localbase/instance` (identity proof by instance token), so either CLI can verify a gateway during cutover and rollback. Moved to later milestones: `diagnostics` and `config validate/plan/apply/show` (M5), `voices` (M4), `access` (M3).

Disk note: the machine had about 57 GB free when this work started, below the
90 GB floor. This branch adds about 180 MB of `node_modules` (needed to run TS
for the contract suite) and an 80 MB Tailwind CLI in `go/.cache`. Scratch roots
symlink model files rather than copying them.

### M3 (in progress)

| Area | Evidence |
| --- | --- |
| Browser sign-in: CF Access (RS256 JWKS), OIDC (discovery, PKCE, nonce, aud/azp), magic links, sessions, `/app/api` forwarding with same-origin rules | End-to-end tests against a fake TLS identity provider; JWS verification for every allowed algorithm; the live `ui-access.json`, policy and users load in Go (read-only, on a DB copy). |
| Policy, revision, users, invitations | The policy revision hash is identical to TS. A shared `collate` package reproduces ICU root order, which TS uses to sort policies and catalog names. |
| Admin APIs: `/_localbase/access-management`, `/_localbase/api-keys`, `/_localbase/hf-token` | A 95-step differential script gets the same status and body from both gateways. |
| `access` CLI (all but `github add`) | A 60-step twin-root session matches TS envelopes and exit codes. CLI errors are redacted like TS. |
| Root operation lease (`<rootHash>.operation.lock`) | Same on-disk format; Go and TS exclude each other in both directions. Service start hands it to the gateway it starts. Serve, configure, status, stop, reset, uninstall and the admin APIs hold it. |

Remaining for M3: UI pages (Chat with streaming, attachments, picker and auto-compaction; Library; Get models; Running now; admin for access, keys and the HF token; settings) and the phone review.

## 11. Decisions (2026-10-19)

1. **Kokoro TTS: dropped.** Speech uses the warm `tts-server` only. Kokoro
   catalog entries are listed but report `unsupported_runtime`; Kokoro can come
   back later as its own feature.
2. **Sign-in: OIDC, Cloudflare Access and magic link.** GitHub OAuth is dropped
   (`/github/callback` and `access github add`; existing GitHub registrations
   stay in `ui-access.json` for rollback and can be listed and removed). Magic-link mail uses `net/smtp`.
3. **Linux NVIDIA-only features (S2V video, whisper PCI matching): deferred.**
   The modality slot and the platform probe stay open for them, and Pixelator
   is available to test NVIDIA work when they are added.
4. **Compaction.** The chat UI compacts automatically, as today. The API never
   rewrites a conversation: an overflowing prompt gets OpenAI's
   `400 context_length_exceeded` error, as OpenAI and OpenRouter do.
5. **Binary name: `local-base`**, unchanged, so the launchd unit, scripts and
   rollback stay as they are.
6. **No purego.** The user wants as few dependencies as possible. Free memory
   comes from `vm_stat`, and the Metal working set from the managed
   llama-server's `--list-devices` output, read once and cached. The 0.75×
   heuristic and `LOCALBASE_GPU_WORKING_SET_GB` remain the fallback.
7. **TOML without a library.** TS already has its own small TOML reader and
   writer (`src/utils/toml.ts`); Go ports that subset too. The only third-party
   module stays `modernc.org/sqlite`.

## 12. Model import and backend selection (M3.5)

The spec is `docs/model-import-and-backend-selection.md` (copied unchanged
from the main checkout). Its acceptance checks are this milestone's test list.
The requirements that supersede earlier preferences (2026-10-20):

- **Automatic choice by default, explicit choice always available.** Detection
  is best-effort and may be wrong. The backend field may be blank, and every
  registered backend stays selectable, with warnings instead of disabled
  options.
- **Selection is per model, per device.** It is never part of an exported
  model definition.
- **Facts only.** The catalog and product text carry source-derived facts.
  Memory figures state their assumptions.

### 12.1 Records

| Record | Where | Contents |
| --- | --- | --- |
| Backend definition | built-in (embedded) and `<root>/backends.d/<id>.json` (custom) | `schema: "localbase.backend/v1"`, `id`, `engine`, `version`, `adapter` (interface id and version), `tasks`, `formats`, `architectures` (supported, known-unsupported), `quantizations`, `companions` per task, `platforms` (os/arch/accelerator), `runtimeVersion` constraint, launch description |
| Logical model | catalog (`variants`) or `imported_models` (DB) | `id`, name, tasks, capabilities, provenance; one or more `variants` |
| Variant | inside a model | `id`, artifacts with roles (`weights`, `projector`, `tokenizer`, `vae`, `text-encoder`, `voice`, ...), and facts: `architecture`, `format`, `precision`, `contextLength`, `dimensions`, `backends` (compatible backend ids and version ranges) |
| Fact | everywhere a property can be unknown | `{value, source}` with source `pinned-metadata`, `inspected`, `declared` or `verified-runtime`; a missing fact is `unknown`, and conflicting facts become `unknown` with both recorded |
| Artifact source | inside an artifact | `kind` (`huggingface`, `url`, `local`), origin, immutable revision (HF commit), expected size and `sha256` with `hashTrust` (`authoritative`, `user`, `observed`), optional `authProfile` id |
| Auth profile | `<root>/credentials/auth-profiles.json` (0600, private) | `id`, `origins` (scope), header names and secret values; model records hold only the id |
| Backend selection | `model_backend_selections` (DB) | `modelId`, `deviceId`, `mode` (`auto`/`manual`), `backendId` (nullable), `variantId`; detection results are recomputed, never stored over a manual choice |

Auth profile values are **plaintext at rest**: the file is mode 0600 in a
0700 directory, readable by the account LocalBase runs as and by anyone with
its backups. That matches the minimal-dependencies policy (no keychain or
secret-service library). A profile named `huggingface` may list CDN origins
that also receive the Hugging Face token; otherwise the token goes to the
Hugging Face endpoint only. Every route, page, export and log shows profiles
as ids, origins and header names only (`/_localbase/auth-profiles`, `auth-profiles list`). The
`diagnostics` bundle (M5) must leave out `credentials/`, the `model_backend_selections` rows
and the device hash; `config show` reads none of them.

Legacy catalog entries (the TS `modelSpecObjectSchema`) load as a logical
model with one variant whose facts come from the entry, marked
`pinned-metadata`. New entries can declare `variants` directly. `deviceId` is
a hash of the platform UUID (`IOPlatformUUID`, `/etc/machine-id`), so a root
copied to another machine starts in automatic mode there.

The new tables are not a drizzle migration: TS rejects any database whose
`__drizzle_migrations` rows differ from its own journal
(`validateMigrationJournal`), which would break binary-swap rollback. Go
keeps a small extension schema instead: `go_schema` records one version, and
its statements are idempotent `CREATE TABLE IF NOT EXISTS`. TS ignores tables
it does not know, so a rollback keeps working; TS just does not see imported
models or selections.

### 12.2 Detection and routing

Host facts are OS, architecture, accelerators (Metal on Apple Silicon; CUDA
when `nvidia-smi` lists a device; Vulkan when a Vulkan loader and device are
present; always CPU), and each backend's installed version.

For each variant and backend the detector reports `supported`, `likely`,
`unknown` or `incompatible`, with reasons. It checks:

- the task;
- the format, which comes from file headers, not the extension;
- the architecture, which is `unknown` when metadata is missing (not
  incompatible);
- the quantization;
- required companions, so an incomplete bundle lists its missing files;
- platform and accelerator;
- the runtime version.

A GGUF file is not evidence of support: the architecture key must match the
backend's list.

Routing rules are a versioned, explicit table (`routing/v1`): per platform
and accelerator, an ordered backend preference. For example, Apple Silicon
prefers llama.cpp-Metal (or an MLX backend, when one is registered and
measured), and Linux prefers llama.cpp-CUDA when CUDA is present, then
Vulkan. The recommendation is the first variant/backend pair rated
`supported` or `likely` that keeps every requested capability. Otherwise the
selection is blank. A faster backend never drops a capability.

### 12.3 Selection and overrides

- **Automatic mode** recomputes on each read, so the result can be null.
- **Manual mode** keeps the chosen id, even if that backend is unregistered
  later; it is then shown as unavailable, and nothing is substituted.
- **Reset to automatic** deletes the manual row.
- **Another variant:** when a manual backend needs a different variant, the
  selection names it and reports its download size. Nothing is downloaded or
  swapped implicitly.

At launch, the planner resolves model → selection → variant → backend:

- **No backend:** `409 backend_not_selected` ("Select a backend to run this
  model").
- **Backend not registered:** `409 backend_unavailable`.
- **Missing files:** `409 model_assets_missing`, listing them.
- **Otherwise:** the adapter prepares the launch. Memory admission, integrity
  checks and process limits run as for curated models.

An override flagged incompatible still launches, and its real startup error
is reported. There is never a silent CPU or model fallback.

### 12.4 Adapters and custom backends

Built-in adapters implement Go interfaces (`Inspect`, `Prepare`, `Launch`,
`Health`, `Cancel`, `Unload`) per task. A custom backend is registered
explicitly (`local-base backends add --file def.json`, or the UI). It names
an existing executable and the `process-openai/v1` adapter interface:
LocalBase starts the executable with templated arguments (`{model}`,
`{port}`, `{host}`, `{ctx}`), waits for its health path, and proxies the
OpenAI-compatible routes for the declared tasks. Memory admission uses the
definition's declared estimate rule, which must state its assumptions.

Downloads never register, install or run anything: repository code (`*.py`,
`trust_remote_code`, custom pipelines) is ignored, and only weights and data
files are fetched.

### 12.5 Acquisition

- **Sources:** a Hugging Face repo or file (revision resolved to a commit
  through the API), a direct `https://` or `http://` URL, or a local file.
- **Staging:** `models/.staging/` with resumable ranges, then an `inspect`
  step, then an atomic publish.
- **Hashes:**
  - With a trusted hash (catalog, HF LFS `sha256`, or one the user typed), a
    mismatch blocks publication.
  - With no trusted hash, the computed hash is recorded as
    `hashTrust: "observed"`, which proves identity only. It is never shown as
    publisher verification.
- **Custom sources** show the trust warning from the spec. Plain HTTP adds a
  separate insecure-transport warning and needs an explicit `--allow-http`
  or a checkbox.
- **Headers:**
  - Auth-profile headers are sent only to origins in the profile.
  - A redirect to another origin drops them, as it drops the HF token
    outside `huggingface.co`.
  - `Range`, `Host`, `Content-Length`, `Transfer-Encoding`, `Connection` and
    `Accept-Encoding` belong to the downloader and are rejected in profiles.
- **Redaction:** logs, errors, progress and exports redact header values and
  the query values of URLs from custom sources.

### 12.6 Surfaces

- **CLI:**
  - `models import <source>` with `--file`, `--revision`, `--sha256`,
    `--auth-profile`, `--backend <id|auto>`, `--variant`, `--allow-http` and
    `--yes`.
  - `models backend <model>`: show the selection, `--set <id>`, `--auto`.
  - `models inspect <model>`.
  - `backends list|show|add|remove`.
  - `auth-profiles add|list|remove`. Header values come from environment
    variables or stdin, never from argv.
- **API:** `/_localbase/model-imports` (resolve, start, progress),
  `/_localbase/backends`, `/_localbase/model-backend` (get/set/reset).
- **UI:**
  - The Get models import form shows source, warnings, variants, size and the
    backend select.
  - Model settings show the backend select, findings and Reset to automatic.
  - Backends has a list and an Add backend form.
  - Recommended backends are listed first, and every option shows its finding
    as a label.
- **OpenAI-compatible routes:** unchanged.

### 12.7 Facts-only content

- **Text:** catalog and UI text are limited to license, revision,
  architecture, precision, context, dimensions, sizes, hashes, tasks,
  companions and platform limits.
- **No rankings:** `codingScore` is not shown, and a lint test rejects
  quality words in catalog text (the same rule as TS PR #368).
- **Memory estimates:** they are labelled with their assumptions, for example
  "weights plus KV cache at 8,192 tokens, estimated" or "measured peak on this
  device". Weight size is never shown as a VRAM minimum.

### 12.8 Engine set (proposal; revises §11.1)

| Engine | Tasks | Platforms / accelerators | Status here |
| --- | --- | --- | --- |
| llama.cpp | chat, vision, embeddings, decisions | macOS Metal; Linux Vulkan and a managed CUDA build; CPU | Integrated. The Mac and Linux pins should match (today: b11671 on the Mac and a different Linux pin); the CUDA package is new work. |
| stable-diffusion.cpp | images, video | Metal, Vulkan | Integrated in TS (M4/M6 in Go). Exact launch profile per architecture. |
| CrispASR | TTS now (Kokoro, Qwen TTS); ASR after qualification | Mac, Linux | TTS Mac/Linux tests done (audit 2026-10-20). Parakeet, Qwen-ASR, Whisper and Vulkan are documented upstream but not qualified here. macOS arm64 is pinned in M4 (§13.3); Linux is not. |
| whisper.cpp | ASR | Metal, Vulkan | Transition exception until CrispASR ASR is qualified. |
| ONNX Runtime (CPU) | Kokoro | CPU | Transition exception; replaced by CrispASR Kokoro once pinned. |

This replaces §11.1 ("no Kokoro, tts-server only"). Kokoro comes back
through CrispASR, and `tts-server` and `llama-tts` become transition
backends for Qwen TTS until CrispASR is pinned. MLX is not in the core set.
It can be registered as an adapter when a measured advantage justifies it,
and the routing table already allows it on Apple Silicon. CPU, Metal, Vulkan
and CUDA are accelerator builds of one engine, not separate engines. None of
this claims that every backend/model pair is tested: each built-in
definition lists only what is qualified as `supported`, and anything else
documented upstream is `likely` or `unknown`.

### 12.9 Plan and status

| Step | Contents |
| --- | --- |
| 1 | `internal/backends`: definitions, registry (built-in plus `backends.d`), host probe, routing table, detector |
| 2 | `internal/models`: logical models, variants, facts; legacy catalog adapter; file inspection (GGUF, safetensors header, ONNX magic, split sets, companions) |
| 3 | Go extension schema (`go_schema`), selection store, device id |
| 4 | `internal/acquire`: sources, auth profiles, origin-scoped client, redaction, staging, hash trust, publish |
| 5 | Launch planning through the selection, with errors; `process-openai/v1` adapter |
| 6 | CLI, admin API, htmx pages |
| 7 | End-to-end tests for every acceptance check (fake HF and URL servers, fake backends, scratch roots) |

#### Status (2026-10-20)

Steps 1–7 are implemented, with the operator review (OP1–OP5) folded in.
`contract/import_e2e_test.go` has one subtest per check; it runs the real
binary on scratch roots and ports against fake Hugging Face, CDN, vendor and
backend servers, and scans every CLI output, API response, UI page, log,
export and the database for three planted secrets.

| Check | End-to-end subtest | Unit tests |
| --- | --- | --- |
| AC1 Metal/MLX variant on Apple Silicon, CUDA/Vulkan on Linux, by rule | `AC1 the logical model picks this platform's variant and backend by rule` | `backends.TestPlatformVariants` |
| AC2 extension is not evidence; incomplete multi-file model names missing files | `AC2 a GGUF extension is not evidence; ...` | `backends.TestExtensionIsNotEvidence`, `TestNoRatingFromNames` |
| AC3 unknown metadata leaves the selection blank; all backends selectable | `AC3 unknown metadata leaves the backend blank ...` | `backends.TestSelectionModes` |
| AC4 manual choices survive reinspection and restart; reset; removed backend not replaced | `AC4 manual selections survive reinspection and restart`, `AC4 a removed backend is kept as unavailable ...` | `backends.TestSelectionModes`, `store.TestBackendChoicesPersist` |
| AC5 overrides get real startup errors without bypassing checks; imports finish without a backend | `AC5 overrides report real startup errors ...`, `AC5 an override never bypasses memory admission`, `AC3 ...; AC5 imports finish without a backend` | |
| AC6 public and authenticated HF imports resolve an immutable revision | `AC6 public and authenticated Hugging Face imports ...` | `acquire.TestHuggingFaceImport` |
| AC7 direct URL warning; private headers kept out of progress, errors, exports, redirects | `AC7 a direct URL shows the custom-source warning ...`, `OP2 ...` | `acquire.TestDirectURLImport` |
| AC8 observed hash for unpinned URLs; trusted mismatch blocks publication | `AC8 an unpinned URL records an observed hash ...` | `acquire.TestDirectURLImport` |
| AC9 custom adapters through the same versioned interfaces; downloads never execute code | `AC9 a custom adapter declares support ...`, `AC9 downloading weights never executes repository code or adds a backend` | `backends.TestCustomDefinitionRules` |
| AC10 OpenAI-compatible interfaces unchanged | `AC10 OpenAI-compatible routes are unchanged`; the TS/Go contract suite | |
| OP1 device identity stays local; export, import elsewhere, automatic there | `OP1 exports carry no selection or device hash; ...` (exported bytes scanned for the device id, the hashed machine id, selection fields and secrets; the second root has zero selection rows) | |
| OP2 profiles redacted everywhere (planted secrets) | `OP2 no output, page, log, export or database contains a planted secret` | `acquire.TestDirectURLImport` |
| OP3 HF token off CDN origins unless the `huggingface` profile lists them; anonymous CDN works; HTTPS→HTTP refused | `OP3 the Hugging Face token stays off CDN origins ...`, `OP3 an HTTPS to plain-HTTP redirect is refused, even with --allow-http, and no header crosses it` (direct URL and HF→http CDN hop) | `acquire.TestHFTokenCDNScope`, `TestRedirectDowngradeRefused` |
| OP4 ratings never from names; user sentences plus machine codes; findings table | `OP4 models inspect --json has a findings table ...`, `AC2 ...` | `backends.TestNoRatingFromNames` |
| OP5 CrispASR listed as not installed, never automatic until pinned (M4: automatic only where pinned, and only for speech) | `OP5 CrispASR is automatic only where pinned, and only for speech` (backends list, a Kokoro speech model, a GGUF whisper model it is never chosen for) | `backends.TestCrispASRWithoutPin` |
| Rollback stays a binary swap | `rollback: TS opens the database Go wrote` | `store.TestBackendChoicesPersist` |

HTTPS→HTTP redirects are refused anywhere in a redirect chain (no consent
path): the source chose HTTPS, and a downgrade mid-transfer is what an
attacker would cause. Plain HTTP sources need explicit consent up front. `--allow-http` covers
only a source that is plain HTTP from the start; it never permits a
downgrade. `LOCALBASE_DOWNLOAD_CA_FILE` adds PEM trust anchors for
downloads (networks with their own CA, and the e2e TLS servers); certificate
validation stays on.

Also checked by hand: the import, model settings and backend pages at 390 px
in light and dark on a scratch gateway. Not done here, by design: live
deployment, CrispASR pins and adapters (M4), a managed CUDA package, and
qualification of any new backend/model pair.

## 13. M4: speech, voices and images

Stacked on M3.5 (`rewrite/go-m4` on `rewrite/go-model-import`). The
OpenAI-compatible routes keep the TS contract: the same validation, error
codes and response shapes, checked by the TS/Go contract suite. Internals use
the M3.5 backend registry: every modality resolves model → selection →
backend → adapter, with the same explicit launch errors.

### 13.1 Runtime

One generic supervisor (`runtime.Service`) replaces the TS per-modality
supervisor factory for STT, TTS and images. Each holds one resident backend
process for one model at a time, with:
- a FIFO queue with capacity and timeout (as for the LLM);
- memory admission before start, with demand from the backend's estimate
  rule, stated with its assumptions;
- a model switch: stop, then start; no rollback onto another model;
- an unexpected exit clears the slot, and the next request restarts it;
- startup failures of chosen or imported backends are reported as
  `backend_startup_failed`.

### 13.2 Adapters

| Adapter | Backends | Contract |
| --- | --- | --- |
| `whisper-server/v1` | whisper.cpp (Metal, Vulkan) | `POST /inference` multipart (translation is a flag) |
| `crispasr/v1` | CrispASR | `--server --backend kokoro\|qwen3-tts`, `POST /v1/audio/speech` (WAV, 24 kHz mono), `GET /health` |
| `tts-server/v1` | tts-server (transition) | TS warm-server protocol |
| `sd-server/v1` | stable-diffusion.cpp | OpenAI-compatible images API of the pinned sd-server |

### 13.3 CrispASR

The 2026-10-20 TTS screen ran the CrispASR v0.8.43 release assets:
- `crispasr-macos-arm64.tar.gz`, sha256 `5c13fbbc…`, 18,012,924 bytes;
- `crispasr-linux-x86_64-cuda.tar.gz`, sha256 `abeb01ea…`, 225,358,330
  bytes, plus pinned CUDA runtime wheels.

It loaded Kokoro 82M F16 (with a voice GGUF) and Qwen3-TTS 0.6B Q8_0 (with
its 12 Hz tokenizer), on CPU and on Metal or CUDA, through its HTTP server.
Every output was a valid WAV; the quality audit is still pending.

M4 pins the macOS arm64 asset as a managed package, so `crispasr-metal` and
`crispasr-cpu` become installable on Apple Silicon and automatic routing may
pick them for speech. Linux stays unpinned until the CUDA library set is
packaged, so `crispasr-cuda` and `crispasr-cpu` there are manual-only,
custom-registered builds. **CrispASR speech recognition stays unqualified**:
whisper.cpp keeps transcription until CrispASR ASR is measured here, and
CrispASR's ASR architectures stay `likely`.

The definitions enforce that split with `automaticTasks: ["speech"]`: routing
picks a backend automatically only for models whose tasks are all listed, so
a GGUF whisper import goes to whisper.cpp even though `crispasr-metal` comes
first in the routing order. CrispASR transcription stays selectable by hand,
with the `not-chosen-automatically` warning.

The managed package is installed on first use (`runtime.EnsureBinary`):
- the archive is checked against its pinned size and sha256 and extracted
  with path checks;
- the Misaki dictionary is rebuilt from two pinned hexgrad/misaki files (gold
  first, then silver entries gold lacks) and must reproduce its recorded
  sha256 `9c44a4b8…`;
- a TS-compatible `.managed-binaries.json` receipt is written.

An installed package with a missing or damaged dictionary is rebuilt, and
the adapter refuses to start without it (builtin G2P mispronounces common
words). The server runs as the screen did: builtin-only G2P, no punctuation
model, network blocked by dead proxies, working directory the model
directory.

The Kokoro catalog model gains a `crispasr-f16` variant (pinned
`cstr/kokoro-82m-GGUF` F16 weights and the `af_heart` voice GGUF), qualified
on `crispasr-metal` and `crispasr-cpu`. `af_bella` is not in the pinned voice
revision, so it answers `voice_not_supported` on this variant. `onnx-cpu`
becomes manual-only.

**Provenance (product decision, open):** CrispASR marks synthesized speech
with an inaudible AI-content watermark and, in its own WAV files, a C2PA
manifest. Measured on the pinned build (2026-10-20): it watermarks raw PCM
responses even with every opt-out, and honors the opt-outs only for WAV. The
gateway therefore asks CrispASR for WAV and keeps only its samples. With
marking on (the default) the output carries the watermark but not the C2PA
manifest, because LocalBase writes its own WAV header and PCM can hold none.
The single switch is `local-base speech marking on|off` (the Go setting
`speech.provenance-marking`, stored in `go_settings`); off passes
`--no-watermark --no-c2pa --no-spoken-disclaimer
--accept-marking-responsibility` and restarts the speech server on the next
request. `TestSpeechKokoroCrispASR` detects the watermark when on and its
absence when off. tts-server output is not marked. Qwen3-TTS through CrispASR needs a reference voice and a consent
attestation and is deferred; Qwen TTS stays on tts-server.

### 13.4 Plan

| Step | Contents |
| --- | --- |
| 1 | `runtime.Service`, a generic supervisor with an adapter interface; LLM untouched |
| 2 | STT: whisper adapter, `/v1/audio/transcriptions` and `/translations` (multipart validation, response formats) |
| 3 | TTS: CrispASR adapter (Kokoro, Qwen TTS) and tts-server; `/v1/audio/speech` (WAV/PCM, streaming formats) |
| 4 | Custom voices: `/_localbase/voices` CRUD and audio; `voices` CLI |
| 5 | Images: sd-server adapter, `/v1/images/generations` and `/edits`, LoRA |
| 6 | UI: Lab pages (speak, transcribe, images) and the voices page |
| 7 | Contract cases TS/Go for every route; real small-model runs (whisper tiny, Kokoro) on scratch roots |

### 13.5 Status

| Step | State | Evidence |
| --- | --- | --- |
| 1 | Done | `runtime.TestService*` (queue, admission, switch, crash recovery, close) |
| 2 | Done | `TestDifferentialTranscriptionRejections`; `TestTranscriptionReal` (whisper tiny.en, identical text on TS and Go in every format) |
| 3 | Done for Kokoro via CrispASR and the tts-server adapter | `TestDifferentialSpeechRejections` (same status, code, param and message as TS); `TestSpeechKokoroCrispASR` (automatic `crispasr-metal`, WAV/PCM/SSE, whisper hears the input); `runtime.TestEnsureBinaryInstallsPinnedRelease` (offline: checksums, extraction, dictionary rebuild); `backends.TestCrispASRWithoutPin` |
| 4 | Done | `voices.TestNormalizeAudioGolden` (15 recordings and every rejection reproduce the TS normalizer byte for byte: `tools/golden/voices.ts`); `voices.TestNameRulesGolden` (NFC, NFKC key, JS whitespace and lowercase); `TestDifferentialVoices` (same stored sha256 on both gateways, every rejection, permission and method error); `TestCLIVoices` |
| 5 | Done | `TestDifferentialImageRejections` (generation and edit validation, permissions, content checks); `TestImageReal` (DreamShaper 8 through sd-server: generation and edit are byte-identical on TS and Go); validator and tuning-block unit tests |
| 6 | Done | `/app/lab` (image, speech, transcription) and `/app/voices` (record or upload, consent, preview, rename, delete), exercised in a browser against real models at 320 and 1280 px; `web` handler tests (API errors shown escaped, results bound to their owner, cross-site posts refused) |
| 7 | Done | Every M4 route has differential TS/Go cases (transcription, translation, speech, voices, image generation and edits, readiness, retry semantics). Real runs on scratch roots: whisper tiny.en (identical transcripts on both), Kokoro through CrispASR (heard back by whisper; marking on and off), Qwen3-TTS through tts-server with a reference and a cloned custom voice on both (heard back by whisper), DreamShaper 8 generation and edit (byte-identical on both). Step 7 also found and fixed: health/readiness now report the speech and image services (TS lists every configured modality as admitting; video waits for M6), media queues use the TS defaults (16 waiting, 60 s) and the serve flags, the Qwen TTS projector role was lost in the legacy variant, and routing preferred CrispASR for Qwen3-TTS, which its adapter does not run yet (tts-server is now preferred; Kokoro still routes to CrispASR). |

`serve` now honours `--stt-*`, `--tts-*` and `--image-*` host and port
options and `--no-stt`, `--no-tts` and `--no-image` (TS defaults: TTS 8092,
images 8090). The TTS server restarts when the custom voice set changes,
since it loads voices at launch. `TestSpeechKokoroCrispASR` needs
`LOCALBASE_CONTRACT_CRISPASR` (the unpacked release, the dictionary and the
pinned Kokoro files) and is skipped in CI.

Custom voices (step 4) keep the TS store: `<root>/voices/<id>.wav` (0600 in
a 0700 directory) plus the `tts_custom_voices` row, so a rollback to TS sees
the same voices. Recordings are decoded and resampled in Go with the TS
arithmetic (float32 samples and kernels, float64 sums, JavaScript rounding),
so the same upload stores the same bytes and checksum on either gateway. A
voice resolves for speech only while its recording matches its checksum.
Voice names need Unicode NFC and NFKC, which the standard library does not
export; `internal/third_party/norm` is the Go toolchain's own vendored copy
of `x/text/unicode/norm` (BSD, Unicode 15), so go.mod still lists only
modernc.org/sqlite. One deliberate difference: a multipart body TS cannot
parse makes it answer 500 `gateway_error`; Go answers 400
`validation_failed`.

Images (step 5) keep the TS launch profile (`-m` or the diffusion component
arguments, generation defaults, `--diffusion-fa`, `--prediction`; 30 s
startup, 180 s from 10 GiB of catalog weights), the `<sd_cpp_extra_args>`
tuning block byte for byte (LoRA, negative prompt, seed, strength, steps,
guidance), and the edit upload checks (PNG/JPEG structure, 2048 px and
4.2 MP limits). As in Bun, an uploaded file's type comes from its filename
extension, not the part header. A media model becomes active when it is
requested, before its files are checked (TS activateModel), so a request for
a selected but missing model changes the default for later requests on both.

Retry semantics are listed in §13.6.

Contract fixtures: launched gateways run offline (dead proxies, loopback
exempt), so a missing runtime or model fails instead of downloading, and
`bin/runtimes` fixtures merge instead of nesting.

### 13.6 Status changes for the cutover checklist

Live clients (naya-device-worker, t3-opencode) retry on 503 with
`Retry-After`. The contract: **transient failures keep 503 (or 429) with
`Retry-After`**, as TS answers them; **failures that need someone to act
answer their real 4xx without it**. `gateway.TestRuntimeErrorRetrySemantics`
pins the mapping and `TestDifferentialRetrySemantics` checks it on both
gateways with a real sd-server.

| Class | Status, code | Retry-After | TS |
| --- | --- | --- | --- |
| A backend crashed or did not become healthy | 503 `service_unavailable` ("Image service is currently restarting…", the service named as in TS) | 5 | same |
| A chosen or imported backend failed to start (M3.5, Go only) | 503 `backend_startup_failed` | 5 | no equivalent |
| Memory pressure | 503 `insufficient_memory` | 5 | same |
| A model that can never fit here, even idle (chat, embeddings, decisions, images, speech, transcription) | 422 `model_too_large` | none | same |
| The same, for a video job | job `failed` with `insufficient_memory` | n/a | same: TS's job error codes have no `model_too_large`, and in both its message says to retry although that cannot help |
| Queue full or timed out | 429 `inference_queue_full`, `inference_queue_timeout` | 1 | same |
| Files not installed | 409 `model_install_consent_required` | none | same on the first request |
| Launch can never work as configured | 409 `backend_not_selected`, `backend_unavailable`, `model_assets_missing`, `backend_task_unsupported` | none | no equivalent (M3.5) |

Deliberate status changes to verify against the live clients at cutover:

1. **A repeated request after a permanent start failure.** TS keeps the failed
   runtime and answers 503 `service_unavailable` with `Retry-After: 5`; Go
   answers the real 409 every time. A retrying client now stops instead of
   retrying a model that cannot start. Transient start failures still answer
   503 with `Retry-After` on every attempt, as in TS.
2. **An unparseable multipart body** (voices, image edits, transcription):
   TS 500 `gateway_error`; Go 400 `validation_failed`.
3. **The wrong method on a JSON inference route** (for example `GET
   /v1/chat/completions`): TS 400; Go 405 with `Allow: POST`.
4. **Model management while a runtime is up** (M5): TS reconciles a running
   runtime eagerly when the active model changes, so the model it switched to
   stays referenced and `uninstall` answers 409 `conflict` until
   reconciliation settles; Go switches on the next request and uninstalls a
   model no runtime holds.
5. **Admin lockout protection** (M3 UI, operator decision). TS keeps it in
   its browser UI only. Go enforces it in `/_localbase/access-management`
   and `/_localbase/api-keys` for every caller, answering 409
   `would_lock_out_admins` with a message:
   - for a change that disables or forgets the caller's own user record;
   - for a change that removes `access:manage` from the caller, including
     an API key revoking itself or dropping `access:manage` from its own
     scopes;
   - for a change that leaves no active principal with `access:manage`
     (people through roles, bindings or the default role, provider-wide
     permissions without a policy, and active API keys).

   Where TS refuses for its own reason, such as clearing a policy that has
   managed users, TS's status stays. `local-base access …` and `local-base
   keys revoke` are the break-glass path: they apply the change and print
   a warning when it leaves nobody able to manage access
   (`TestCLIAccessRecovery`). Each admin change also logs
   `audit.access-changed` or `audit.api-key-changed` with the actor and
   target ids only (`TestAdminAuditRedaction`).

6. **A request cut off by a memory emergency** (operator decision). As in
   TS, critical memory stops every runtime, including ones serving active
   requests. TS answers such a request 502 `upstream_error`, or ends its
   stream with an `upstream_error` event. Go answers 503
   `service_unavailable` ("Inference service…") with `Retry-After: 5`. A
   stream that already started ends with a `service_unavailable` error
   event and closes. Chat, embeddings, images, transcription and speech
   all do this (`Lease.Interrupted`; `TestEmergencyStopAnswers503`,
   `TestEmergencyStopEndsStream`).
7. **An input over llama-server's physical batch size** (operator, after
   cutover, from the Clef Flash qualification).
   - **The trigger:** the decision runtime launches with
     `-c 8192 -b 8192 -ub 8192`. A longer input makes llama-server answer
     500 `server_error` "input (27634 tokens) is too large to process.
     increase the physical batch size (current batch size: 8192)".
   - **TS 0.1.0** passes that through as a 500 (read from the source).
   - **Go** answers 400 `invalid_request_error`, code
     `context_length_exceeded`. The message states the limit, for example
     "The input is about 27,634 tokens, but this decision model accepts up
     to 8,192." Without the numbers it says "The input is larger than this
     decision model accepts at once. Shorten it and try again." `param` is
     the input field: `state`, `input` or `messages`.
   - **Where it applies:** one relay serves decision, embeddings and chat,
     so all three map it. In practice, chat reaches the context limit first,
     because llama-server splits a causal prompt across batches; that case
     is the existing `exceed_context_size_error` → 400.
   - **Test:** `TestBatchOverflowIs400`, with the fake backend's
     `fake:batch` and `fake:batch-plain`.
8. **A chat request with an empty `messages` list** (operator observability
   audit, 2026-10-24, Linux). TS's schema accepts an empty array and passes
   it to llama-server, which answers 500 ("Jinja: No messages provided"),
   and TS relays that 500 (read from the source). Go answers 400
   `invalid_request_error`, code `validation_failed`, `param: messages`
   (`TestEmptyMessagesIs400`). The differential validation cases now send
   one message, so each still isolates its own field.
9. **Only a chat model becomes the active LLM** (#375, after cutover). TS
   persists any switched-to LLM as `activeLlmModel`, a decision or embedding
   model included. Go persists only chat models (§18). After a decision
   request, the model-management state therefore shows a different active
   model on each gateway. `TestDifferentialModelManagement` first activates
   the same chat model on both.

Operator-run steps for the cutover rehearsal, which no automated test
covers because they need a real service manager:

- **`config apply` restarting a launchd service.** Install the Go binary as
  a service under a throwaway launchd label pointed at a scratch root and
  port, never the live label or root. Then check that `config apply --file
  <doc with a new gateway.port>` (default `--restart auto`) restarts it
  (`activation: restarted`). Check that `--wait` returns once the restarted
  gateway acknowledges the new settings (`pendingRestart: false`), and that
  `config apply --restart never` followed by `local-base service restart`
  clears the pending restart. Remove the label afterwards.
- **Real OpenTelemetry Collector.** Run `go/tools/otelcol-check.sh`, which
  uses the pinned `otelcol` 0.163.1 and verifies its checksum, against the
  release candidate; see §14.3.
- **Chat turns only a real model can show.** An image turn with a real
  vision model, and a real chat model choosing a generation tool by itself,
  are covered only by the TS comparison (`go/tools/jstest`). No vision
  model or tool-calling chat model of 2 GB or less is installed for the
  automated checks. Run both on the release candidate with the user's real
  models.
- **Real iPhone over Cloudflare Access** (§15.4): no zoom when the
  composer is focused, the composer stays above the keyboard, Return adds a
  line on touch, VoiceOver, and dictation with the real microphone.

The Lab and My voices pages (step 6) call the API in-process as the signed-in
person (`gateway.Server.Call`), so they get exactly the API's validation,
permissions and errors without a JSON client. Generated media is kept per
person for 30 minutes (at most 16 results) and served from the UI's own
origin. Recording, file conversion and resampling to WAV happen in the
browser (`static/js/audio.js`), since the server decodes only WAV.
`TestServeForUI` serves the contract fixture with `--no-auth` on a test
port for checking the pages by hand.

**Difference by design: `--no-auth` is loopback-only** (2026-11-02). `serve
--no-auth` refuses a gateway host that is not loopback (127.0.0.0/8, `::1`,
`localhost`, or a name resolving only to those), however the host was set, and
checks the bound address again after listening. TS 0.1.0 allowed it. Backends
always bind loopback (`runtime.BackendHost`), and the flag is the only way to turn
authentication off. Tests: `TestHostIsLoopback`, `TestServeRefusesNoAuthBeyondLoopback`,
`TestNoAuthOnlyOnLoopback`.

**`--no-auth` is for local development only and must never be tunneled.** Binding
loopback does not make it private: any web page in a browser on the machine can send
it requests, a page can rebind its own name to 127.0.0.1, and a tunnel on the machine
connects from loopback. So a `--no-auth` gateway also answers 403
`no_auth_request_refused` to (`gateway.noAuthRefusal`):

- a `Host` that is not loopback or `localhost`, on every route (stops DNS rebinding,
  and a tunnel that keeps the public name);
- any proxy header: `Forwarded`, `X-Forwarded-*`, `X-Real-Ip`, `Cf-Connecting-Ip`,
  `Cf-Ray`, `Cf-Access-Jwt-Assertion`, on every route;
- on `/v1/`, `/_localbase/`, `/app/api/` and `/app/session`, an `Origin` other than
  this gateway's, or a `Sec-Fetch-Site` other than `same-origin` or `none`.

Clients that are not browsers send neither header and are unaffected. The gateway has
no CORS headers, so no other origin could use it properly before. A proxy that rewrites
`Host` and strips every marker still gets through: that is the limit of what a server
can see. With authentication on, none of this applies. Tests:
`TestNoAuthRefusesForeignRequests`, `TestNoAuthAllowsInProcessCalls`,
`TestAuthenticatedGatewayIgnoresTheNoAuthRules`, `TestLoopbackHost`.

**Not a status change, but clients should know:** transcription with no `language`
treats audio as English on every model, in TS and Go alike, and `language=auto`
detects it (§24).

## 14. M5: catalog feed, discovery, declarative config, diagnostics, OTLP

Stacked on M4 (`rewrite/go-m5` on `rewrite/go-m4`). M5 makes the Go gateway
feature-complete except video. Every route and command keeps the TS contract,
checked side by side; where TS talks to the network (Hugging Face, the feed,
an OTLP collector), goldens come from running the TS code against recorded
or local responses.

### 14.1 Plan

| Step | Contents | Evidence |
| --- | --- | --- |
| 1 | `/_localbase/model-management` (GET state; POST install, uninstall, enable, disable, activate; install answers 202 with an operation to poll) | Differential cases on twin scratch roots, a local download source for installs |
| 2 | Remote catalog feed: download, format/version/sha256 checks, per-entry validation, annotations, the `cache/catalog-feed` last-good copy, the 24 h schedule in `serve`, `models feed` CLI, `LOCALBASE_CATALOG_FEED_URL` | Feed goldens from `remote-feed.ts`; both gateways loading the same local feed |
| 3 | Hugging Face discovery: `models search`, `models add hf:…`, `models remove-entry`, `/_localbase/model-discovery` | Goldens from `huggingface.ts`/`discovery.ts` with a mocked fetch; Go against a local fake hub |
| 4 | `config validate/plan/apply/show` (declarative TOML) | CLI contract on twin roots |
| 5 | `diagnostics` (redacted ZIP bundle) | Bundle listing and redaction compared with TS; planted secrets never appear |
| 6 | OTLP export of traces and logs over `http/protobuf` (the only protocol TS supports), the OTEL_* settings, export-failure reporting, and the TS log redaction rules | A local collector decodes both gateways' exports for the same requests; protobuf encoding hand-written, no SDK |
| 7 | Contract sweep and real runs; DESIGN status | Full suite on both implementations |

OTLP stays dependency-free: the exporter encodes the few OTLP protobuf
messages it needs (resource, scope, spans, log records, attributes) directly.

### 14.2 Status

| Step | State | Evidence |
| --- | --- | --- |
| 1 | Done | `TestDifferentialModelManagement`: full state and every response on both gateways, an offline install polled to completion |
| 2 | Done | `catalog` goldens from `tools/golden/feed.ts` (the published `catalog/feed-v2.json` and v1 feed, crafted documents, tolerant annotation parsing, URL rules, all 76 built-in models' summaries, peaks and marks); `jsjson` goldens (JSON.stringify of parsed JSON); `TestCLIFeed` (status, use, refresh with ETag and failures, `models show` text and JSON, catalog fit and capability filters with feed peaks and marks applied); `TestFeedSchedulerReloadsCatalog` |
| 3 | Done | `hub` goldens from `tools/golden/hub.ts`: 37 recorded Hub responses replayed offline (14 repositories: GGUF with quants, a projector and shards, a decision model, whisper.cpp, SDXL, FLUX.1, an SDXL LoRA, LCM and v-prediction checkpoints, gated and missing repositories; searches); kind detection, file choices, every generated entry, searches, previews with fit and feed notes, and discovery match TS. `TestCLIHubGolden` (the TS command handlers' output lines, JSON, catalog files and errors, byte for byte), `TestDifferentialModelDiscoveryRejections`, `TestModelDiscoveryEndToEnd` |
| 4 | Done | `TestCLIConfigDeclarative` (twin roots: 40 invalid documents with the JSON envelope and the raw message before redaction compared, plans, `--detailed-exit-code`, apply with `--restart never`, `--wait` refused without a gateway, apply creating a fresh root, stdin, human output, and each CLI reading the other's pending restart), `TestCLIConfigForegroundGateway` (automatic restart refused next to a foreground gateway; unchanged apply with `--wait`), `TestDeclarativeGolden` (`tools/golden/config.ts`: 54 documents, their messages before redaction, normalized configurations, plans against saved, pending and absent configurations, and renderings; runs in CI), `TestStaticFingerprintMatchesTypeScript`, `TestPendingRestartLifecycle`, `TestDocumentKeyOrder` |
| 5 | Done | `TestCLIDiagnostics` (twin roots with planted secrets in log messages, errors, attributes and identifiers, a launch-shaped catalog patch, an invalid catalog file, a credentialed feed URL, OTLP environment headers and captured service stderr: the manifests match field for field, `logs/events.jsonl` byte for byte, the human output line for line, no secret reaches either archive; an uninitialized root; the output rules, with an existing file never replaced), `TestEventsGolden` (`tools/golden/logs.ts`: 50 lines, which ones the reader keeps and their redacted diagnostics form; runs in CI) |
| 6 | Done | `TestDifferentialOTLP` (each gateway exports to a local collector; for the same requests, an authenticated list, an unmatched route, missing and invalid credentials, and a real chat under a remote traceparent, the span trees match: names, kinds, flags, status, parentage and attributes, including the inference, backend client and backend start and load spans; every exported log record is a local event with the TS attribute mapping and its span correlation; request and inference records match; exporter headers are sent and never logged), `TestDifferentialOTLPFailures` (a failing collector is reported in the local log with the same events on both, never exported), `TestCLIOtelEnvironment` (doctor and configure report the effective OTEL_* settings; invalid variables fail both before anything is saved), `otlp` goldens from `tools/golden/otel.ts` (ratio sampler decisions, Retry-After handling, the outage reporter over a scripted outage) and collector tests in CI |
| 7 | Done | Full suite on both implementations with real models (qwen2.5-coder-1.5b chat through both exporters, whisper, Kokoro, Qwen3-TTS, DreamShaper) and Go-only in CI. The sweep added `TestResolveOtelGolden` (`tools/golden/otelconfig.ts`: 69 saved-setting and OTEL_* combinations, resolved or rejected with the TS CLI message; it found that WHATWG URLs drop default ports), `TestApplyDecision` (the TS apply cases as restart decisions; CI now runs the `cmd` tests), and a bounded shutdown against a hung collector. It also made the M3.5 AC5 acceptance step retry a transient `insufficient_memory` 503, as clients honouring Retry-After do: the 7 GB macOS runner's free memory moves with everything else on it, and admission stays on. Not covered by any test: `config apply` restarting a real launchd service (it would install one); the restart path is the service package's, already covered on a scratch label in M2. |

The feed brings three Go structures with it. `internal/jsjson` reproduces
JavaScript's JSON round trip (property order, number formatting), which the
feed checksum and the Misaki dictionary depend on. The catalog is now three
tiers (built-in, remote, user) with provenance (`Trust`, `Origin`, `Tier`,
`LayerPath`), and a gateway replaces its catalog in place when the hourly
check downloads a new feed. Feed peaks rank between checked-in measurements
and estimates in every memory figure and in admission (LLM and media), and
feed capability marks join the spec's own in capability resolution; both
apply only to an exact repository, revision and file match.

Diagnostics brought two reader changes. The log reader now applies the full
TS event schema (strict keys, identifiers, routes, attribute names and
values), so `logs` and diagnostics keep exactly the lines TS keeps. User
catalog files are reported with the TS wording, in the TS order (unreadable
files first, then entries), and record which fields a patch changed; a
wrong-typed field is worded as zod words it, while other validation failures
keep the Go wording.

OTLP brought three gateway changes that also show in the local log. Request
events carry the TS auth outcome (`disabled` on public routes, `missing` or
`invalid` instead of `unauthenticated`). Speech and transcription now record
`inference.completed` with an inference span, as TS does. The queue wait in
`Server-Timing` and inference telemetry ends when a request leaves the queue,
not when its model has finished loading (TS `queueWaitMs`).

M5 is complete: the catalog feed, Hugging Face discovery, declarative
configuration, diagnostics and OTLP export match TS. Still open before M6,
carried over from M3: the Chat, admin and settings pages, and a discovery
page over `/_localbase/model-discovery` (the API is done). The user's
decision on CrispASR's marking is also still open; marking stays on and
C2PA is not passed through until they decide.

### 14.3 Operator review follow-ups

1. **OTLP against a real collector.** `TestRealCollector` runs a pinned
   OpenTelemetry Collector, core distribution `otelcol` 0.163.1 for
   darwin/arm64 (`otelcol_0.163.1_darwin_arm64.tar.gz`, sha256
   `664047e6ffd98ffaaf90d282bb481ffad46b1e8456f0b2f65e42d627d665d8d3`),
   from `LOCALBASE_CONTRACT_OTELCOL` (or `go/tools/otelcol-check.sh`, which
   downloads and verifies it). It listens on a test port, never the
   live collector on 127.0.0.1:4318, and writes what it parsed with its
   file exporter. Both gateways' span trees and log records, as the
   collector parsed them, match, and the Go export is accepted with no
   reported failure. Partial success (rejected spans or log records, in a
   protobuf or JSON body) now feeds the #358 reporter, with the collector's
   message redacted: a deliberate difference, since TS ignores the body. 4xx
   and 5xx responses were already reported. #367: shutdown flushes while the
   file sink is open, the failure events land in `events.jsonl` after
   `gateway.stopped`, and nothing about OTLP reaches stderr
   (`TestOtelShutdownDiagnosticsStayInTheLog`).
2. **Catalog feed and #368.** Built against current main: a feed entry with
   a built-in id is ignored. #368 is not merged yet. Follow-up for when it
   lands:
   - port `src/catalog/former/` and adoption (installed or configured former
     entries written to `catalog.d/<id>.json` with the notes hint, recorded
     in `catalog.d/.former-built-ins`, adopted while the config row loads);
   - skip former ids from the feed, as built-in ones, so they never come
     back as community entries;
   - add the `pipelineTemplate` fallback in discovery (FLUX.1, FLUX.2 Klein
     9B, SDXL and SD1.5 encoders from the pinned former entries);
   - regenerate `tools/golden/feed.ts` and `tools/golden/hub.ts` against it
     (the FLUX.1-schnell, SDXL base, SDXL LoRA and LCM cases already
     exercise the fallback);
   - port `former-adoption.test.ts` as a contract test.
3. **Install operations across a restart.** Operations are recorded in
   `runtime/model-operations.json`. At shutdown an accepted install gets
   three seconds and is then interrupted. After a restart, an install that
   was running (interrupted or killed) reports `failed`, with "Interrupted
   when the gateway stopped. Install again to resume.", and installing
   again resumes the partial file (`TestInstallInterruptedByGatewayStop`,
   SIGTERM and SIGKILL). TS keeps operations in memory and waits for the
   install, so under launchd's stop deadline it loses them.
4. **Diagnostics.** The planted-secret scan now also covers download auth
   profiles, an imported model's signed URL, a custom voice's display name
   (the log carries its id) and `go_settings`.
5. **Hugging Face goldens.** The 37 recorded responses are pinned by sha256
   (`testdata/hub/PINS`, `TestFixturesPinned`), and the golden script
   refuses to fetch unless `LOCALBASE_HUB_RECORD=1`. The removed-entry
   template fallback gets its golden with #368 (item 2).

## 15. M3 UI pages: Chat, settings, admin, discovery

The operator's order: Chat first (the user relies on it from a phone), then
settings, admin and discovery, then a phone review, before M6. Design
research: `.design/research/chat/BRIEF.md` (decisions D01–D10).

### 15.1 Plan

| Step | Scope | Evidence |
| --- | --- | --- |
| 1 | Chat core: `/app` opens Chat, with a compact header and drawer on phones and a sidebar from 1024 px. Model picker (ready chat models first), composer, streaming through `/app/api/v1/chat/completions` as the signed-in person, Stop, reasoning, markdown, copy, jump to latest, a status region, and every empty, error and permission state. History stays on the device, in TS's key and schema | JS cores checked against the TS functions in bun (history, chat context, the think splitter); markdown fixtures, including unsafe input; handler tests; browser checks at 320–1600 px in both themes |
| 2 | Chat extras: attachments for vision models, dictation, send-now steering, compaction, generation settings, and generation tools | TS goldens for compaction and steering; browser checks |
| 3 | Settings: the account, chat history (keep or clear), generation defaults, the Hugging Face token | Handler tests; browser checks |
| 4 | Admin: API keys, browser users and roles, access | Permission tests against the API; browser checks |
| 5 | Discovery page over `/_localbase/model-discovery` (§14 step 3), following the Shelf brief | Hub fixtures; browser checks |
| 6 | Phone review: the iOS simulator and a real phone over a test port, with VoiceOver | Captures in both themes; the review notes |

The client code is plain ES modules under `static/js`, served from 'self'
under the existing CSP. There is no framework and no build step. Pure logic
(history, chat context, reasoning split, markdown parsing) is kept apart
from the DOM, so bun can test it against the TS functions
(`go/tools/jstest`).

### 15.2 Status

| Step | State | Evidence |
| --- | --- | --- |
| 1 | Done | `go/tools/jstest`, run by `TestChatJSAgainstTS`: the chat core against the TS functions on the same inputs. This covers the think splitter under every chunking, `chatContext` with and without compaction, device history in both directions (TS-written history loads, and the Go page writes what TS writes, rejects what TS rejects, and strips what zod strips), and `streamText`, including truncation, error events and tool-call deltas. Markdown matches react-markdown with GFM on 21 fixtures; unsafe links lose their link, raw HTML stays text, and an unterminated fence still renders. `internal/web` tests cover the `/app` redirect, every page state, the permission state, Settings, and inert model JSON. Browser checks on the UI fixture with a real model, at 320, 390 and 1280 px in light and dark: a full turn with markdown and code, Stop ("Stopped", "Interrupted", focus back in the composer), the drawer (focus moves in, Escape returns it), history saved and restored, the unready model (draft kept, picker opens), a 503 with Retry, and no page overflow with long words, URLs, code and tables (code scrolls inside its block) |
| 2a | Done | Send-now steering and compaction. `go/tools/jstest/compaction.test.ts` compares the token estimates, `overflowContext`, the learned context (same storage key and format), `promptBudget`, `planCompaction` (fits, over budget, tool overhead, one-turn and forced plans, an existing summary), the summarizer transcript, and `compact` (summary, earlier summary, empty summary → dropped, cancelled) against TS: the same request bodies and the same results. `phaseLabel` and `interruptReply` are compared too. The fake backend's overflow now uses llama.cpp's wording, and `fake:overflow` fails only while the request has more than four messages. Browser checks on the UI fixture with a real model (32,768-token slot): a seeded ~40k-token conversation with a larger learned context overflowed for real; the page learned 32,768, compacted two messages, retried and answered, and the summary was saved. "Compact now" folded the next turn (count 2 → 4) and showed its timer. Typing during a reply turns Send into "Send now (interrupts reply)"; sending kept the partial reply as "Interrupted" and streamed the new turn |
| 2b | Done | Attachments and dictation. `attachments.test.ts` compares `readAttachments` on 14 cases (text and code files, PNG/JPEG/WebP, an image on a text-only model, a fake image, PDF, empty, oversize, the 256 KiB text total, invalid UTF-8, NUL bytes, the four-file limit, the 16 MiB retained limit), the accept list, and the conversation and request checks against TS. `dictation.test.ts` compares `appendDictation` and the PCM16 WAV encoder byte for byte. The page gets each model's effective input modalities and, for people with `inference:transcription`, the dictation model (the active transcription model when installed, else the first installed and selected; TS takes the first). Browser checks on the UI fixture with real models: an image on the text-only model and a PDF are refused with TS's messages; a Markdown file was sent inline as TS sends it, the reply used it, and saved history kept only `attachmentsMissing`. Dictation ran the real pipeline (MediaRecorder, browser WAV conversion, whisper tiny.en) on a speech clip fed as the microphone; the text was added to the draft without sending and focus returned to the composer. Discard drops a recording. No vision model ≤ 2 GB is installed, so an image turn against a real vision model is unverified |
| 2c | Done | Generation tools, generation controls and imported models. `tools.test.ts` runs the Go and TS `runChat` against the same scripted gateway, with deterministic ids and blob URLs, in 23 cases: plain replies, tools off or without permission, image (with size, seed, steps, guidance, negative prompt and LoRA; the studio-only fields are not sent), speech (chosen voice, unknown voice), text-to-video job polling and cleanup, speech-to-video not offered, uninstalled models, malformed JSON, extra or blank arguments, too-long speech, unknown and unpermitted tools, a tool call without tools, the round limit, an empty image response (reported as malformed, as TS's schema does), fresh ids next to earlier calls, gateway errors and an ended session. Both must send identical requests and produce identical protocol messages, artifacts and errors. `generationTools`, catalog order and every parameter check are compared too. The page reads the gateway's model metadata from `/app/api/_localbase/models`, as TS does, and refreshes it after each reply; the server passes only the generation permissions the person holds. Imported chat models (M3.5) join the picker, ready when a variant has all its files (`TestChatOffersImportedModels`). Browser checks on the UI fixture: temperature and maximum tokens from the controls reach the request (a stale-closure bug that dropped one field was fixed). With the chat model marked as tool-calling and its replies scripted in the page, a `synthesize_speech` call ran on the real TTS model; the 1.44 s clip played inline with a download link, and the follow-up request carried the tool call and its result. The controls list the chat model and every tool model, with no overflow at 390 px. The installed small chat model can't call tools, so a model choosing a tool by itself is unverified here |
| 3 | Done | Settings: chat history (step 1); a note pointing to Chat's generation controls; the Hugging Face token, for people with `models:manage`; and the account (name, a plain list of what the person may do, sign out). **Decision:** there are no saved generation defaults. TS keeps generation settings in memory for the page ("They are not saved and reset when the page reloads"), so Settings points to Chat's controls instead of adding persistence TS doesn't have. The token card is server-rendered with htmx and goes through `/_localbase/hf-token` in-process as the person, so the API's permission and validation apply; it ports TS's messages (accepted for a name and role, rejected, unreachable, cleared, falling back to HF_TOKEN), and the page never shows a saved token. `TestSettingsHFToken` covers save (trimmed, the API gets exactly `action` and `token`, the response never echoes it), check, clear, a blank token, an API rejection, an unknown action, and no card or route without `models:manage`. Browser check on the UI fixture at 390 px: save (badge "Saved"; Hugging Face is unreachable through the fixture's dead proxy, so the check says so), Replace and Clear appear, clear returns to "Not set", no overflow |
| 4 | Done | Admin at `/app/admin` (§15.3, reviewed): People & roles, Browser access and API keys, ported from TS `auth-management.tsx`. It's server-rendered with htmx, and every read and change goes through the admin APIs in-process as the person. The page and its link need `access:read` or `keys:read`; each section is read-only without the matching `:manage`; the POST routes need `:manage` and the existing same-origin htmx guard. The API refuses self-lockout and last-manager changes (operator decision, commit 71bc72b8), and the page shows its message. A new or rotated key secret and an invitation link appear once, only in the response to the change that made them (`no-store`). Destructive actions (forget user, remove role or registration, clear policy, rotate, revoke) use a disclosure that names the consequence in TS's words. It opens and closes by keyboard; Escape closes it and returns focus to its button (a `<details>` instead of the planned server-rendered step: the same named confirmation, with no request). Provider fields follow the chosen provider, and hidden fields are disabled, so they're neither required nor sent. Secrets are never pre-filled. `TestAdminSections`, `TestAdminReadOnlyAccess`, `TestAdminPeopleActions`, `TestAdminAccessActions` and `TestAdminKeySecretOnce` cover gating, read-only views, the bodies sent, the self row, the API's lockout message, role and default-role changes against the current revision, and the one-time secret. Browser checks on the UI fixture at 390 and 1280 px, light and dark: Cloudflare provider saved (audit event recorded, no email), provider fields switched for OpenID Connect and public clients, policy applied through Advanced JSON, an invitation with its link shown once, a key created and rotated through its confirmation with the secret once and not after reload, the keyboard path through a confirmation, no overflow |
| 5 | Done | Get models is now discovery over `/_localbase/model-discovery`, as the person (TS `model-discovery.tsx`, Shelf D01–D04). Search is a GET with the query in the URL (D01: shareable, back button works); htmx swaps the results in place. Each card shows name, org, badges (gated, feed status, fit), facts, and a qualified catalog match first ("View catalog model →", D04); with a match, community files fold under "Use a community file instead". The recommended file comes first (D03): the Go port of `recommendedChoice`, `orderedChoices`, `lowQualityQuant` and `qualifiedCatalogMatch` matches the TS golden (`tools/golden/choice.ts`: 18 choice sets from the recorded Hub previews and edge cases, 15 quant labels, 40 repositories against the built-in catalog plus community look-alikes). Every file's details are rendered by the server and switched by a small script with no request; Save to catalog, Add and install, Show files and Install LocalBase recipe are htmx POSTs that redraw only their part. Errors a token can fix link to Settings. People without `models:manage`, which the API requires even for search, see TS's read-only notice instead of a search that would fail. `TestDiscoverSearch`, `TestDiscoverReadOnly` and `TestDiscoverActions` cover the bodies sent, the selection, low-bit and existing files, token hints, the catalog match, failed installs and unsafe card ids. The UI fixture now reads the recorded Hub (`HF_ENDPOINT`). Browser checks: "qwen coder" gives 20 cards, with five repositories matched to qualified catalog models; switching files updates the details and fit badge; Save to catalog added an entry with a link to it; an unrecorded repository's Show files reports the Hub error and offers Try again; no overflow at 320, 390 or 1280 px; light and dark. Disclosure markers were added to flex summaries (here, and Chat's reasoning, compaction and tool settings) |
| 6 | Done here; real iPhone checks with the operator | §15.4: screenshots at 390 px in both themes through a scratch edge proxy, edge sign-in expiry on Chat and htmx pages through a real cross-origin 302, the tab-resumed retry, and a real iOS Safari render on the Simulator. The keyboard, VoiceOver and the microphone need a real iPhone |

The phone keyboard (`interactive-widget`, the 16 px composer), VoiceOver,
and Enter on touch screens are left for the phone review (step 6).

### 15.3 Admin plan (for review before building)

Admin ports TS `auth-management.tsx` (brief D09) to `/app/admin`. It is
server-rendered with htmx. Every read and write goes through the existing
admin APIs in-process as the signed-in person: `/_localbase/access-management`
and `/_localbase/api-keys`, which the 95-step differential script already
holds to TS. The page adds no new permission logic of its own, apart from
item 5.

1. **Who sees what** (TS `administrationSections`). The Admin link and route
   need `access:read` or `keys:read`; without both, the route answers 403 and
   no link shows. Sections: "People & roles" and "Browser access" with
   `access:read`, "API keys" with `keys:read`. With only the `:read`
   permission a section is read-only (TS `ReadOnlyPeopleAndRoles` and
   `ReadOnlyAccess`, and key scopes as text). The forms render only with
   `access:manage` or `keys:manage`, and the POST routes need that same
   permission (`mutating`), so the API's check is never the only one.
2. **Requests.** Every POST uses the existing `mutating` guard: htmx only,
   `X-Localbase-UI`, same origin, `Sec-Fetch-Site` same-origin, and a 64 KiB
   form limit. Every state-changing htmx route (the `mutating` and
   `uploading` guards, so the Lab, voices, imports and Settings too) also
   needs a per-session CSRF token. It is an HMAC, under a per-root key
   (`runtime/ui-csrf.key`), of a random `lb_csrf` cookie (`HttpOnly`,
   `SameSite=Strict`, path `/app`). The layout puts it in htmx's
   `X-CSRF-Token` header. A cross-site form post, a post without the token,
   and another session's token are refused (`TestCSRF`). Each action maps one-to-one to a TS API call with the same
   JSON body. Policy edits carry the `expectedPolicyRevision` read with the
   page. A `policy_revision_conflict` shows "The policy changed elsewhere.
   Reload and try again." and re-reads the policy.
3. **Secrets.** A new or rotated API key secret, and an invitation sign-in
   link, appear exactly once: in the htmx response to the action that made
   them, in a "Copy this secret now. It will not be shown again" panel with
   Copy and Done. They are never stored server-side, never put in a URL or
   page state, never logged (the response is `Cache-Control: no-store`), and
   never re-rendered by a later fragment. OIDC and GitHub client secrets and
   the SMTP password are `type=password autocomplete=new-password` fields that
   are never pre-filled ("Re-enter to save"), as in TS.
   They also stay out of browser history: the fragments that show them carry
   `hx-history="false"` (and htmx keeps no history snapshots at all,
   `historyCacheSize: 0`). The Admin page is `Cache-Control: no-store`, which
   keeps it out of the back/forward cache, and `admin.js` blanks a shown
   secret on `pagehide`.
4. **Destructive actions** need a named confirmation in the same row: a
   disclosure that opens on the first click with the consequence in TS's
   words and a Confirm button. Escape or a second click closes it and keeps
   focus on its button (built as a `<details>`, with no request, instead of
   the server-rendered step first planned). This covers revoke key ("This key will stop working
   immediately."), rotate key, remove a sign-in registration ("Removing the
   last one disables browser access."), clear policy, remove role, and forget
   user (TS's "this is not revocation" text). Disable user and set default
   role act at once, as in TS.
5. **Self-protection.** TS enforces this only in the browser. Go enforces it
   in the UI handler too, which is stronger but the same rule. The signed-in
   person, matched by verified email as TS `isCurrentManagedUser` does, cannot
   disable or forget their own record, or save roles that leave them without
   `access:manage` (TS `assignedRolesGrantAccessManagement`). The API keeps
   its own rule: at least one reachable role must grant `access:manage`.
   **Decided in review (operator):** the rules live in the API, a deliberate
   divergence from TS. Whoever calls it, `/_localbase/access-management`
   refuses (409 `would_lock_out_admins`) a change that disables or forgets
   the caller's own record, or leaves the caller without `access:manage`.
   It also refuses a change that leaves nobody able to manage access. That
   covers a policy clear while the provider-wide permissions lack
   `access:manage`. API keys with `access:manage` count as managers, and
   `/_localbase/api-keys` applies the same rules (see §13.6, item 5). Where TS has its own refusal (for
   example, clearing a policy that has managed users), TS's error stays.
   `local-base access` is the recovery path: it applies the change and warns
   when nobody can manage access. Each successful admin change is logged as
   `audit.access-changed` or `audit.api-key-changed`, with the action, the
   actor kind and id (a hash for browser sessions) and the key id; never
   emails, names or secrets. `auth.Guard` models the resolution
   `ResolvePolicy` uses (`TestAccessLockout`); the differential script gives
   the provider-wide permissions `access:manage` before its policy clear, so
   both gateways still run the same steps.
6. **First setup.** "Set up roles" creates TS `starterAccessPolicy`: an admin
   role for the person's verified email, and a member role with chat and
   `models:read`. It needs a verified email; `--no-auth` and API-key
   principals have none, so the page says to use Advanced JSON or the CLI, as
   TS does. Advanced JSON is a policy textarea, validated by the API.
7. **Restart notices.** Provider and registration changes say "Saved. Restart
   LocalBase to apply browser access changes." Policy, user and key changes
   apply now, as the API reports (`restartRequired`).
8. **Layout.** Shelf list rows, one card per section. Below 1024 px the
   section switcher is a segmented control (`aria-pressed` buttons, as in
   TS); from 1024 px the sections can sit in columns. Permission pickers are
   fieldsets grouped as TS groups them (Inference, Models, Configuration, API
   keys, Browser access, Sessions), and show the permission ids. Status
   badges are text: Active, Expired, Revoked, disabled.

Evidence will come from handler tests against a fake API: each action's JSON
body, permission gating of routes and controls, a secret shown once and absent
from the next fragment, confirmations, and the self-protection rules. There
will also be a contract test against the real gateway: create, rotate and
revoke a key and invite a user through the page's routes, with the API state
compared after each step. Browser checks at 320 to 1280 px in both themes
round it out.

### 15.4 Phone review (step 6)

Scratch setup: the UI fixture (real small models, recorded Hub) on
127.0.0.1:24790, behind a scratch edge proxy on **127.0.0.1:24791** that
forwards like Cloudflare Access. The proxy can be switched to answer every
request with a cross-origin 302 to an Access login (`/__edge/expire`,
`/__edge/restore`), or to break the next N connections (`/__edge/drop?n=N`).
Screenshots are in `/tmp/lbgo-phone-review` and are not committed.

| Check | Result |
| --- | --- |
| Chat, history drawer, picker, Stop, Retry at 390 px, light and dark | Done (01–05). Stop leaves "Interrupted" and announces "Stopped". A broken connection shows "Could not reach the gateway…" with Retry, and Retry completed the turn |
| Edge sign-in expiry (#363/#364) in Chat | Done (06). With the proxy expired, a send gets the real cross-origin 302 and shows "Your sign-in expired. Reload to sign in again." with Reload. This is TS `gatewayFetch`, ported in 738d30fa; before that commit Chat showed a network error |
| Edge sign-in expiry on htmx pages | Done (07). A Get models search while expired shows the same notice with Reload (session.js probe) |
| Tab resumed retry | Done. With no recent resume, one broken read surfaces the error. Right after the tab becomes visible again, the broken read is retried after 400 ms and succeeds (433 ms; the proxy log shows the drop, then the success). Replies (POST) are not retried, as in TS |
| Real iOS Safari render | Done on the iOS Simulator (iPhone 17, iOS 27) through the scratch URL (08). The safe-area header sits below the status bar, and the composer stays above Safari's toolbar. `simctl` can't type, so focus, zoom and the keyboard were not exercised |
| Keyboard (no zoom on focus, composer above the keyboard, Return adds a line on touch), VoiceOver, microphone | Operator, on a real iPhone over Cloudflare Access |

## 16. M6: video jobs and the converter

Branch `rewrite/go-m6`, draft PR #374, stacked on #373.

### 16.1 Plan

1. Video jobs: `/v1/videos` create, status, content, cancel and delete; the
   job manager; the stable-diffusion.cpp video client and runtime; input
   validation; the pinned AVI→MP4 converter; memory admission, idle
   recovery and pressure for video.
2. The Lab video page and the chat `generate_video` tool against the Go
   routes.
3. Linux/systemd qualification. The Linux NVIDIA video target uses NVIDIA PCI
   pinning, which stays deferred with the other NVIDIA-only features (§11.3).

Every installed video model needs the 6 GB umt5 text encoder (about 10 GB in
total), above the 2 GB limit for test models. A real-model run waits for the
operator. Until then the success path is covered by the fake sd-server
video API, and the TS comparison covers everything up to the job's start.

### 16.2 Status

| Step | State | Notes |
| --- | --- | --- |
| 1 | Built | `internal/video` (job manager, sd-server client, converter), `gateway/video_*.go`, `runtime/video.go`. Details below |
| 2 | Done | Lab video section (`web/labvideo.go`, `static/js/video.js`) and the chat tool. Details below |
| 3 | Done | Linux CI job, service unit golden, and the systemd check in a local container (§16.4) |

Step 1:

- **Contract.** `TestVideoGolden` (`tools/golden/video.ts`) pins the
  create-request validation messages (22 cases), the native input
  validation (18 cases, including PNG and WAV checks), the qualification
  result and profile-mismatch message for each of the three catalog models
  with a runtime profile (11 cases each), and the job projections (16,
  including every failure code and memory message). Mutation checks fail
  each part.
- **Job manager** (TS `VideoJobManager`). One job at a time (429
  `video_job_busy`). The job's deadline comes from the catalog. Artifacts
  are limited to 64 MiB per job and 256 MiB in total, with at most 20
  finished jobs, kept for 15 minutes. Files are private, under
  `<root>/tmp/video-jobs`, and removed at start. The job holds the video
  runtime's lease until it finishes, and cancelling stops that runtime
  first. A stop that fails leaves the job running, so a later cancel can
  retry it. Memory pressure fails the active job (`FailVideo`).
- **Runtime.** `Service` runs `sd-server` in `vid_gen` mode with TS's
  arguments (`buildSdVideoServerArgs`) and its 180 s startup budget. Every
  catalog artifact is verified (size and sha256, through the checksum
  cache) before launch. Eligibility follows TS: an experimental model
  needs the opt-in, and a device the profile does not list gets
  `model_hardware_ineligible`. A memory refusal of a stopped video runtime
  logs stage `start-preflight`. Found and fixed: catalog video models named
  backends that don't exist (`stable-diffusion.cpp-apple-unified`), so none
  could select one. TS runs them on the Metal and Vulkan sd-server builds,
  and Go now does too.
- **Converter.** TS's pinned ffmpeg builds, in the Go runtime manifest.
  `TestFFmpegPinsMatchTS` (`tools/golden/ffmpeg.ts`) keeps them equal. The
  managed installer gained the `binary` format and `copy` support files
  (the license and notices). The command line, the 120 s deadline, the
  frame-count check and the `ftyp` check are TS's. Like Go's other
  backends, the converter runs in its own process group with no guardian
  process.
- **Diagnostics.** `video.job-terminal` and `video.job-containment-failed`
  as in TS. The JSONL writer now takes an `error_type`, for TS's typed
  errors. Go now also logs `backend.start-failed` with its cause for every
  runtime. It had no such event; the comparison run found it when a sparse
  wan2.1 failed its checksum silently.
- **Tests.**
  - `TestDifferentialVideoRejections` compares 44 requests that never
    start a job: validation, profile mismatches, unknown and non-video
    models, install consent, the multipart form, and the job routes for
    missing and malformed ids.
  - `TestDifferentialVideoJobFailure` starts the same job on both gateways
    (sparse wan2.1 weights); both accept it and end it failed with
    `video_generation_failed`.
  - `TestVideoJobLifecycle` (Go only, fake sd-server and converter) covers
    completion with the converted MP4 and its headers, the native body sent
    to the backend, busy, cancel stopping the runtime, a failed backend
    job, the memory-pressure failure, and delete.
  - Job manager unit tests cover owners, the deadline, shutdown, the
    artifact limits and eviction, the TTL, pressure, and the stop retry.
    They pass under `-race`.

Real FastWan run (2026-10-21, operator-approved memory window,
`TestVideoReal` with `LOCALBASE_CONTRACT_VIDEO_MODELS`): one
fastwan2.2-ti2v-5b-q6_k text job at the qualification bounds (480×832, 81
frames, 16 fps) per implementation, TS then Go. The weights were read
through read-only APFS clones of the live files, and each scratch gateway
was halted with its sd-server as soon as its job ended.

| | TS | Go |
| --- | --- | --- |
| Wall time (create to completed) | 156.8 s | 158.3 s |
| Peak sd-server footprint | 14.0 GiB | 14.0 GiB |
| Lowest available memory | 19.8 GiB | 20.4 GiB |
| Output | MP4, H.264, 480×832, 16 fps, 81 frames, 5.06 s, 839,465 bytes | the same |

The two MP4 files are byte-identical: the catalog pins the seed, and both
implementations run the same sd-server and ffmpeg pins. During the window
the live gateway answered `/health` on every probe, logged no memory
refusal or pressure change, and served no inference.

Step 2:

- **Lab video section.** The Lab lists text- and image-to-video models.
  Speech to video is left out: it needs portrait and audio inputs that
  neither TS's studio nor its chat tool collects. Each model's fixed
  profile is shown. For image-to-video models, an optional starting
  picture (PNG, JPEG or WebP) is scaled and center-cropped in the browser
  to the model's frame and sent as a PNG `input_reference` (TS studio
  `cover`). The job runs on `/v1/videos` as the signed-in person. The page
  polls it every 2 s with Cancel, then plays the MP4 through a Lab route
  that serves byte ranges, which iOS Safari needs. Failures show the
  API's message. `TestLabVideo` covers the model list, the options, the
  request at the profile, polling, playback with ranges and failures; the
  cross-site test covers the new POST routes.
- **Browser check** on the UI fixture, at 390 px light and 1280 px dark.
  A 900×600 JPEG becomes a 480×832 PNG with a preview. A model without
  files shows the API's install message. A wan2.1 job (sparse weights)
  shows "Starting the video runtime…" with Cancel, then "Video generation
  failed.", and polling stops. The fixture can't produce a video; the
  playing state is covered by the unit test, and the real output by the
  FastWan run.
- **Chat `generate_video`.** Ported in M3 step 2c (`tools.js`) and checked
  against TS's `runChat` on a scripted gateway. The routes it calls are now
  checked against TS by the contract suite.

### 16.3 Harness file-system guard (operator request)

The near miss in step 1 was the harness writing a placeholder through a
link onto a shared model filename, stopped only by a read-only clone. The
contract package now routes every write through `contract/fsguard_test.go`:

- **Scratch roots only.** Before each create, write, truncate, directory
  creation or removal, the real path is resolved (the entry itself, or its
  nearest existing ancestor). The write is refused unless it lands inside a
  registered scratch root: a harness gateway root (`scratchTemp`) or a
  test's `t.TempDir()` (`scratchDir`). The live root is never accepted as a
  scratch root.
- **No shared inodes.** Writing through a symlink, or to an existing file
  with more than one link, is refused.
- **Clones only.** Real model files enter a fixture only through
  `cloneModel`: an APFS clone (`cp -c`), or a read-only copy for files up
  to 64 MiB. The result is made read-only and checked to be a single-link
  regular file with its own inode. Every symlink the harness used to create
  is now a clone. After the fixture is built, `assertNoLinks` walks the
  whole root: a model file that is a link, a link that leaves the root, or
  any hard link fails the fixture.
- **Refusals are loud.** Call sites that used to ignore write errors use
  `must*` forms that panic on a refusal, so a refusal fails the test or the
  run.
- **Tests.** `TestFSGuard` tries each forbidden case: writes and removal
  outside a root, a `..` escape, writes through a directory symlink and a
  file symlink, a hard link under either name, the live root, a clone over
  an existing entry, a link passed off as a clone. It then shows the files
  outside are unchanged and that allowed writes still work.
  `TestNoUnguardedWrites` fails if any other file in the package calls the
  `os` write functions, `t.TempDir()` or `cp` directly.

Also found by the new Linux CI job: the M3.5 rule that CrispASR is
automatic only where LocalBase pins it was enforced only while it was
missing. A managed backend without a pinned package for the device
(CrispASR on Linux) is now manual-only even when installed, and its option
carries `not-chosen-automatically` (`TestUnpinnedManagedBackendIsManualOnly`).

### 16.4 Linux/systemd qualification (step 3)

Pixelator is not used for this: it is System One's GPU host, held by other
work. The qualification runs locally and is opt-in:
`tools/systemd-check.sh` (Docker and bun). It builds the linux/arm64
binary, then starts a disposable Debian container with systemd as PID 1:
`tools/systemd-check/Dockerfile`, base pinned to the `debian:bookworm-slim`
index digest, `--privileged --cgroupns=private`, cgroup v2, no published
ports, no GPU. Inside it, as a non-root user with lingering enabled, on a
scratch root, so the unit label is a scratch one, the script checks:

- `start` installs, enables and starts the unit, and `status` reports
  running and the gateway ready;
- the installed unit is byte-identical to TS's `renderSystemdDefinition`
  for the same service manifest (`tools/golden/systemd-unit.ts`, 577
  bytes), with the token and journal-only stderr;
- `config apply --restart never` (a new gateway port) records a pending
  restart; `restart` applies it, so the gateway answers on the new port,
  and the pending change is acknowledged;
- the gateway's output is in the user journal, and there is no stderr file
  on Linux (that file is the launchd path);
- `stop` leaves the unit inactive and disabled and the gateway gone; then
  `uninstall --yes` removes the unit, systemd no longer lists it, and the
  root is gone.

The container and the image are always removed. Result on 2026-10-21
(Docker Desktop 29.4.3, arm64): every check passes on three consecutive
runs. Two fixes to the script were needed. It waits for systemd-logind
before enabling lingering, and then starts the user manager itself:
lingering alone raced logind once. And it copies files into the home
directory, because `docker cp` cannot write into a tmpfs mount.

## 17. M7: cutover plan (for operator review)

The operator runs every live step. Nothing here has been run yet. Each step
that reads the live root waits for the operator's review of this plan.
Throughout, "live" means `~/.local/share/local-base`, `~/.local/bin/local-base`
and the `com.localbase.gateway.<hash>` launchd agent.

### 17.1 Preconditions

Each item is done, or waived on record by the operator.

1. **#368 (curated catalog, former entries).** Waiting on the user. Both
   paths are planned:
   - **Merged before cutover:** port it (former adoption, the feed skip,
     the discovery template fallback), regenerate the goldens, rerun the
     dual contract, and cut a new release candidate.
   - **Not merged:** cut over without it. Record the decision, and port it
     after cutover like any TS change during the two weeks (§17.3 step 7).
2. **Local iOS and accessibility checks on the Go UI** (operator,
   2026-10-21; these replace the real-iPhone gate). The tests run real iOS
   Safari on the Simulator, Apple's accessibility audit and axe-core in
   desktop Chrome. They check:
   - no zoom on focus;
   - the composer above the keyboard;
   - Return adding a line while Send submits;
   - microphone dictation end to end;
   - contrast, labels, hit areas and order.

   Results are in §17.9. Dictation waits for the GPU hold to clear.
3. **§13.6 reviewed against each live client:**

   | Client | Routes to check | Differences that can reach it |
   | --- | --- | --- |
   | naya-device-worker | `POST /v1/chat/completions` (3827 calls in the Oct 2026 logs), `GET /v1/models` (3348), `GET /v1/models/{id}` (13, logged as unmatched before #362), `POST /v1/systemone` (6). Its client allows only `{base}/models` and `{base}/chat/completions`, retries on 503 with `Retry-After`, and treats 499 as its own cancel | 1 (repeated request after a permanent start failure), 3 (wrong method on a JSON route), 6 (503 on a memory emergency) |
   | t3-opencode | `/v1/chat/completions` (streaming), `/v1/models` | 3, 6 |
   | decision clients | `/v1/systemone` | 3, 6 |
   | browser (Chat, Admin, Lab) | `/app/**`, `/_localbase/**` | 2 (unparseable multipart), 4 (model management while a runtime is up), 5 (Admin lockout, 409) |

   Each row is signed off as "no client relies on the TS behaviour". The
   transient-failure contract (503 or 429 with `Retry-After`) is unchanged,
   and 6 replaces a 502 with that contract.
4. **The open cutover checklist items (§13.6), on the release candidate:**
   - an image turn with a real vision model;
   - a real chat model choosing a generation tool by itself;
   - the real-collector check (`go/tools/otelcol-check.sh`, pinned
     otelcol 0.163.1);
   - `config apply` restarting a launchd service, under a throwaway label
     (folded into the rehearsal, §17.2 step 5).
5. **A green build:** CI on macOS and Linux, a full dual contract run, and
   `tools/systemd-check.sh`, all on the release-candidate commit.
6. **Version string: `0.2.0`** (operator decision). The `-go` suffix was
   dropped at the release candidate (2026-10-21).

### 17.2 Rehearsal on this Mac, against a copy of the live root

**The copy** goes to `/private/tmp/lbgo-shadow/root`. Method (b) was chosen
(operator decision, no downtime):

- **(a) Quiesced clone.** Briefly stop the live gateway (`local-base stop`),
  clone the whole root with `cp -c -R` (APFS, copy-on-write: the live files
  are only read), then `local-base start`. Downtime is seconds.
- **(b) Live, consistent.** No downtime. `sqlite3 ~/.local/share/local-base/local-base.db
  ".backup /tmp/lbgo-rehearsal/root/local-base.db"`, then APFS clones of
  everything else (the models directories, `ui-access.json`, credentials,
  voices, `catalog.d`, the checksum caches). No links, as in §16.3.
  `assertNoLinks` is run on the copy.

Either way the copy holds real API key hashes, sessions and access
settings. It is served on loopback only and deleted after the rehearsal.

**Steps:**

1. **Schema baseline.** For the copy, record `SELECT hash, created_at
   FROM __drizzle_migrations` and `sqlite3 .schema`.
2. **Install Go as a service on the copy.** Under a throwaway launchd
   label, which follows from the copy's path: `com.localbase.gateway.<hash
   of /tmp/lbgo-rehearsal/root>`. Use a test port in 24000–29999, set with
   `local-base --root /tmp/lbgo-rehearsal/root config apply` on the copy;
   the backend ports move with it, away from the live ones. Then
   `local-base start`. The TTS, image and video ports are flag-only (defaults 8090–8092,
   the same as live), so the service on the copy is sent no media request.
3. **Checks against the copy:**
   - `local-base smoke`, including a decision question;
   - the contract suite in Go-only mode (`-url http://127.0.0.1:<port>
     -key <a copy key>`);
   - `status`, `diagnostics`, `/health`, `/health/ready`.
4. **No real clients (operator decision).** naya would process real jobs
   twice, and opencode sessions are the user's. Instead, `TestClientReplay`
   (`contract/replay_test.go`) replays their request shapes with the
   operator's temporary key: models list, retrieve of every listed model,
   chat (non-streaming and streaming) and one decision request.
5. **`config apply` with a launchd restart** (§13.6): a new port with
   `--restart auto` gives `activation: restarted`; `--wait` returns on
   `pendingRestart: false`; `--restart never` followed by `restart` clears
   it.
6. **The UI through a temporary path (operator decision).** A `--no-auth`
   scratch variant of the copy on loopback, for the visual and flow checks
   of Chat, Admin and Lab. No cookie from the copy is ever used in a
   browser. Session validation is proven separately by the TS-vs-Go
   comparison of the copied `auth_browser_sessions` rows (§17.7).
   cloudflared and Access are not touched.
7. **Schema both ways.** After Go ran, the `__drizzle_migrations` rows are
   identical to the baseline. The only schema change is the Go extension
   tables (`go_schema`, `imported_models`, `model_backend_selections`,
   `go_settings`).
8. **Rollback on the copy.**
   - `local-base stop` (Go).
   - Swap to the TS binary, the live release `0.1.0` copied to a scratch
     path, never the live one.
   - `restart` under the same throwaway label, then `smoke`.
   - TS opens the database and its journal check passes. It ignores the Go
     tables; keys and sessions work.
   - Record the time from stop to a passing smoke.
9. **Forward again.** Swap back to Go and `smoke`, to prove the database
   still works for Go after TS ran.
10. **Clean up.** `stop`, then `uninstall --yes` on the copy (removes the
    throwaway label and the copy), then remove `/tmp/lbgo-rehearsal`.

Results go into this section: timings, schema dumps, and any issue.

### 17.3 Live cutover runbook (the operator runs it)

**Before:** the rehearsal passed within the last day, and the live
gateway is idle. Use the window preflight (`/tmp/lbgo-video-window/preflight.sh`
has the checks): no inference in flight, no benchmark backends, at least
90 GB of free disk.

1. **Back up** to `~/.local/share/local-base/backups/pre-go-<date>/`:
   - `sqlite3 local-base.db ".backup …/local-base.db"`;
   - `ui-access.json`, `credentials/`, `voices/`, `service/manifest.json`;
   - the installed plist from `~/Library/LaunchAgents/`;
   - a copy of the TS binary as `local-base.ts-0.1.0`.
2. **Stage the binary:** `cp <release>/local-base ~/.local/bin/local-base.new`,
   then `chmod 755`.
3. **Swap**, following the deploy rules (an in-place `cp` over a running
   binary kills it):
   1. `mv ~/.local/bin/local-base ~/.local/bin/local-base.ts-0.1.0`
      (if step 1 didn't already keep it);
   2. `mv ~/.local/bin/local-base.new ~/.local/bin/local-base`;
   3. `local-base --version` prints the release version;
   4. `local-base restart`. The plist may change, and restart rewrites it
      under the same label; Go's rendering is byte-identical to TS's for
      the same inputs.
4. **Post-cutover checks:**
   - `local-base status`, `local-base smoke` (with a decision question),
     `/health` and `/health/ready`;
   - **deferred from the reduced drill (§17.10), now explicit:**
     - the post-swap `local-base smoke` must pass on the live active model
       (qwen3.5-9b), with its real load;
     - one `POST /v1/systemone` decision question must answer 200 with
       the requested type, using a scratch key from the operator;
   - the default chat model list (§18):
     `defaultLlmModels = ["qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-9b-q4_k_m"]`,
     applied with `config apply`. Then `local-base status` shows the
     effective default and its reason;
   - the public hostname through Cloudflare Access: Google sign-in, then
     Chat from the phone, Admin and Lab;
   - naya-device-worker and t3-opencode work normally;
   - the Grafana telemetry flow: traces and logs from the new gateway
     arrive;
   - 30 minutes of `local-base logs --level warn`, watching
     `runtime.memory-admission-rejected`, `runtime.memory-pressure-changed`,
     `backend.start-failed` and error rates.
5. **Rollback triggers.** Any one of these is enough:
   - a failing smoke or `/health`;
   - a sign-in or API key failure;
   - a client failing in a way it didn't before;
   - inference errors above the pre-cutover baseline;
   - a memory incident;
   - missing telemetry;
   - the operator's call.
6. **Rollback.** Target: under 5 minutes from the decision to a passing
   smoke, as measured in the rehearsal.
   1. `local-base stop`
   2. `mv ~/.local/bin/local-base ~/.local/bin/local-base.go-<commit>`
   3. `mv ~/.local/bin/local-base.ts-0.1.0 ~/.local/bin/local-base`
   4. `local-base --version` prints `0.1.0`
   5. `local-base restart`, then `local-base smoke`

   The database needs nothing: the drizzle journal is unchanged, and TS
   ignores the Go tables. The backup from step 1 is only for corruption.
7. **Retiring TS.** The TS binary (`local-base.ts-0.1.0`) and the TS CI
   jobs stay for two weeks after the cutover. During that time there are
   no Go-only drizzle migrations, so rollback stays a binary swap. After
   two weeks with no rollback, and the operator's sign-off:
   - the TS sources and bun CI retire;
   - the `drizzle/` SQL moves under `go/internal/store/migrations`;
   - the stacked PRs (#369 to #374) merge with the operator.

### 17.4 Not touched by the cutover

cloudflared and the Cloudflare Access configuration; every launchd service
other than the LocalBase gateway's own label; model files (no download,
move or rewrite; the Go binary reads the same files); the user's
OpenTelemetry collector and Grafana; System One and Pixelator.

### 17.5 Feature audit of the live root (2026-10-21, read-only)

Read only through `sqlite3 -readonly` on `file:…?mode=ro`, `jq` on the JSON
files, and listings. Secrets were reported as set or unset; no values are
recorded here.

| Live feature | Live state | Go | Notes |
| --- | --- | --- | --- |
| Drizzle journal | 15 rows | **Matches exactly** | The row hashes equal the sha256 of Go's 15 embedded migrations |
| Gateway listener | 127.0.0.1:2273 | Supported | `gateway-listener.json` ({0.0.0.0:2273}) is a leftover neither implementation reads; the live socket is 127.0.0.1:2273 |
| Runtime backends | llm 0.0.0.0:18000, stt 0.0.0.0:18080, ctx 65536 auto, parallel 1 | Supported | Bind hosts as TS (`BackendHost`) |
| Selected models | 8 LLM, 1 STT, 1 TTS (Qwen3-TTS), 1 image, 1 video (FastWan) | Supported | All in Go's built-in catalog |
| `catalog.d` overrides | 8 files | Supported | Go loads all 8 with no diagnostics (checked on a plain copy of the files) |
| Managed runtimes | llama, whisper, sd, tts-server and ffmpeg pins present | Supported | Go finds the same pinned package directories, so nothing is downloaded at cutover. The kokoro-tts and CrispASR pins are absent but unused (no Kokoro model selected) |
| Browser access | direct provider, one Google OIDC registration (`client-secret-basic`) | Supported | No GitHub, magic-link, Cloudflare or email-delivery setup |
| Access policy | 2 roles, 1 email binding, a default role | Supported | |
| API keys | 57 rows, 9 active | Supported | Same table and hashing (contract suite) |
| Browser sessions | 1 row | To verify in the shadow check | TS-vs-Go comparison of the stored row on the copy (§17.2) |
| OTLP | endpoint 127.0.0.1:4318, sample 100, no headers | Supported | |
| Hugging Face token | set | Supported | |
| Custom voices, observed memory, capabilities | 0, 3, 2 rows | Supported | |
| Service | plist with `LOCALBASE_SERVICE_ID` and `LOCALBASE_SERVICE_TOKEN` only | Supported | Rendering is byte-identical (§16) |
| Installs in progress | none (`runtime/` holds only `gateway.lock`) | n/a | |
| Memory reserves | system 15% / 8 GB, accelerator 10% / 2 GB | Supported | |

**Blocking gaps: none.**

Findings for the plan:

1. **The live binary is not the `v0.1.0` tag.** It reports `0.1.0`, but it
   is byte-identical to `/tmp/lb-review/dist/local-base`, built from main
   at `c4a823d1` (#365, 2026-10-18 18:44). The tag has 10 migrations and
   the live database has 15. The rollback drill must use TS at
   `c4a823d1`: the tag's TS would reject the live journal. Rollback in the
   runbook (§17.3) uses the installed binary itself, which is unaffected.
2. **Two models share a projector by hard link:**
   `qwen3.6-35b-a3b-*-mmproj-F16.gguf`, both unselected. Clones give each
   copy its own inode; the live files are untouched.
3. **`bin/` has 54 symlinks.** The 36 inside `bin/runtimes` are relative
   library links within their packages. The others are an old flat layout
   that neither implementation uses and the copy skips.

### 17.6 Shadow tooling and the dry run (2026-10-21)

`tools/shadow.sh` runs §17.2 and §17.3 on a copy:

- **`copy`** runs the idle preflight when the source is the live root. It
  takes the database through `sqlite3 "file:…?mode=ro" ".backup …"`, clones
  the rest (`catalog.d`, `models`, `ui-access.json`, voices, credentials,
  cache, logs, `bin/runtimes`), writes the copy's own root marker and sets
  the copy's config row `root` (both implementations refuse a moved root),
  then verifies that the copy is detached. The live `service/`, `runtime/`,
  backups and `tmp/` are not copied, so nothing in the copy names the live
  label or lock.
- **`serve go|ts`** runs a gateway on loopback, with every backend host on
  127.0.0.1 and every port in the scratch range.
- **`schema <label>`** snapshots the journal and the schema.
- **`sessions`** runs the TS-vs-Go session verdicts on a second copy of the
  database, with TS taken from `LBGO_TS_SRC`.
- **`cleanup`** stops everything and deletes the copy.

`local-base smoke` on the copy finds the copy's gateway from the copy's own
owner record (`runtime/gateway.lock`), never port 2273.

Dry run against a synthetic scratch source root (Go `init`, a read-only
clone of the 1.5B model, a synthetic OIDC registration, and 4 synthetic
sessions: valid, due for a touch, expired, wrong issuer). Everything
passed:

- the copy was 0700 and detached; the journal held 15 rows;
- the Go gateway passed smoke;
- the session verdicts were identical in TS (`c4a823d1`) and Go for all 4,
  and a mutated touch interval made the check fail;
- TS opened the database after Go, and smoke passed; rollback took 5 s
  from stopping Go;
- Go passed smoke again after TS;
- the journal was unchanged by either;
- cleanup left no files or processes.

The contract suite in external mode with a chat and models:read key keeps
6 tests valid on any root: health, readiness, authentication, unmatched
route, model retrieve and chat rejections. The rest assume the harness
fixture (its model list, all-scope keys), so the shadow run uses that
subset plus smoke.

### 17.7 Shadow and rollback drill on the live copy (2026-10-21)

Run with the operator's approval, under their conditions.

- **Preflight before the copy:** 25.8 GiB available, the live gateway
  idle (no inference in the last 60 s), no other backends.
- **The copy:** `/private/tmp/lbgo-shadow`, mode 0700, the database
  through the read-only `.backup`, and APFS clones, verified detached. It
  held 9 selected LLMs (with the operator's 02:20 change), qwen3.5-9b
  active, and the operator's temporary key `go-shadow-temp` (active).
  Free disk was unchanged (123 GB).
- **The key:** read only into the scratch client's environment
  (`LOCALBASE_CONTRACT_KEY`), never printed, written or passed on a
  command line.

| Check | Result |
| --- | --- |
| Journal baseline | 15 rows, as live |
| Session row (TS `c4a823d1` vs Go, by stored hash, no cookie) | `valid-touch` in both; identity fingerprints equal |
| Go gateway on the copy | healthy on loopback, scratch ports |
| `smoke` (Go) | passed: chat on qwen3.5-9b, 11 s with the model load |
| Decision question with the key | answered (clef-flash) |
| Contract subset with the key (6 tests) | all pass on Go, and again on TS |
| Go errors in the copy's log | none |
| Schema after Go | journal unchanged; added only `go_schema`, `imported_models`, `model_backend_selections`, `go_settings` |
| Rollback: TS `c4a823d1` on the same database | smoke passed (decision); **7 s** from stopping Go to a passing smoke |
| Schema after TS | journal and schema unchanged (TS ignores the Go tables) |
| Forward: Go again | smoke passed; 4 s from start to a passing smoke |
| Memory | lowest available 21.1 GiB (the guard aborts below 12) |
| Live `/health` during the run | 200 on every probe |
| Cleanup | copy, clones and TS export deleted; no processes left |
| Live root | nothing under `models/` or `catalog.d/` modified; binary unchanged |

The decision question made clef-flash the copy's active LLM, and both
implementations save a switched model as active. That happened in the copy
only.

### 17.8 Catalog direction and model-directory hygiene (operator, 2026-10-21)

**#368, revised** to the TS-runnable part of the approved proposal. Port
it when it merges, as planned (§17.1). The proposal's benchmark metrics
are internal and never appear in product text.

| Area | Change |
| --- | --- |
| Chat | qwen3.5-4b (compact), qwen3.6-35b-a3b-iq4_nl_xl (new built-in, promoted from the live user entry at unsloth rev a483e9e6), qwen3.8-27b |
| Former entries | qwen3.5-9b and gpt-oss-20b become user entries |
| Unchanged | embeddings 0.6B and 4B, whisper-large-v3-turbo, images (Z-Image, Klein), video, Clef |

**Go-only entries.** Each is added only after its backend is qualified on
this hardware:

- CrispASR Kokoro-82M F16 with its voices;
- CrispASR Qwen3-TTS-0.6B Base Q8 with the F16 codec (voice cloning);
- Parakeet TDT 0.6B v3 and Qwen3-ASR-1.7B, each needing a maintained
  Mac or Linux backend first (tested only through MLX so far);
- a portable FLUX.2 Klein profile, if the MLX result is the one to ship.

Platform-specific adapters such as MLX are acceptable where justified,
with the same user experience. The metadata-only candidates (SD Turbo,
SDXL Lightning, Wan 1.3B, 5B and A14B, optionally LTX 2.5) are not
qualified and are not built-ins; the weekly scan owns them.

**Staging in model directories.** `models list` now skips dot-prefixed
entries, such as a `.benchmark-parts-*.gguf/` staging directory, partial
downloads and checksum caches. It also skips anything that isn't a
regular file, even when the name ends in a model extension
(`looseModelFile`, `TestLooseModelFiles`). TS `models list` lists such a
directory as an LLM, so on a root that contains one, Go's output is
shorter. The live directory is the operator's to clean up.

**#368 ported (cdb45205, merged and deployed).** `main` is merged up the
stack. `go generate` syncs 18 built-in entries and the 61 former entries
(`internal/catalog/data/former`, in `FORMER_CATALOG_DATA` order).

- **Kept as user entries.** On every catalog load, an installed former
  entry (its primary file is on disk) is written to
  `catalog.d/<id>.json`, as is one the settings name (selected or active,
  `conf.KeepFormerSelections`). The file is the shipped data, with
  "No longer in the LocalBase catalog; kept from an earlier release." in
  front of its notes. A user patch at that path is applied on top.
  `catalog.d/.former-built-ins` records each id, so removing the entry
  sticks.
- **Measurements and backends kept.** A kept entry whose launch-shaped
  fields match the shipped ones keeps its checked-in memory records
  (`Spec.UsesReferenceMeasurements`). It also keeps the backends it was
  qualified on, so llama-server is selected without a header read.
- **Set aside.** A complete user file that pins exactly a built-in's
  repository, revision and files (qwen3.6 on the live root) is set aside,
  and the built-in applies as shipped. A file that pins other files stays
  a patch, with trust "modified".
- **Feed.** The feed never provides a former id. Hub pipeline templates
  fall back to former entries.
- **Defaults and copy.** Fresh installs default to qwen3.5-4b and
  dreamshaper-8-lcm. The CLI and the model details page say "planning
  minimum", with its note and the measuring machine. Coding scores are
  neither shown nor used for ranking.
- **`configure --*-models`.** It accepts built-in ids only, as TS's
  `assertCatalogModels` does: TS checks before the root's catalog loads.
- **Tests.** Goldens are regenerated. The memory and plan generators call
  `keepFormerCatalogEntries()`, and their Go tests use
  `catalog.LoadWithFormer()`. `former_test.go` ports
  `former-adoption.test.ts`.
- **Contract fixture.** It keeps its former image and STT ids, as the live
  root does. The retry case asks for the built-in juggernaut-xl-v9: on a
  busy machine, both gateways refuse a missing model for memory before
  its files.

### 17.9 Local iOS and accessibility checks (2026-10-21)

These checks replace the real-iPhone gate (§17.1 item 2). They ran
against a fake-backend `--no-auth` fixture on a scratch port: a stub
llama-server and sparse weights, so no model was loaded.

**How the checks run:**

- **Simulator Safari:** `tools/ios-check.sh <url>` runs an XCUITest
  project, `tools/ios-check/LBCheck.xcodeproj`, that drives real iOS
  Safari. The environment is Xcode 27, an iPhone 17 Simulator (402 pt
  wide) and iOS 27. Xcode 27 has no Simulator.app, so a test
  host app opens the URL in Safari. The accessibility audit is
  `performAccessibilityAudit`, the engine behind Accessibility Inspector.
- **Desktop Chrome:** `tools/axe-check.sh <url>` runs axe-core 4.14.0 in
  headless Chrome. The copy is pinned in `tools/axe.pins`, its sha256
  matches the npm integrity hash, it is cached in `go/.cache` and it is
  injected over the DevTools protocol only. It runs WCAG 2.0 to 2.2 A/AA
  plus best practice on 16 UI states, at 1280 and 390 px, in light and dark.

| Check | Result |
| --- | --- |
| No zoom on focus | Every input, textarea and select is 16px or more (`TestControlsAtLeast16px`). Two Admin and backend textareas were `text-sm` and are now `text-base`. Tapping the composer leaves its width unchanged (376 pt before and after), so the page did not zoom |
| Composer above the keyboard | After the tap, Safari scrolls the composer from y 671 to y 341, and its bottom edge (385) sits above the keyboard's top. The screenshot shows the composer, the form-assistant bar and the keyboard |
| Return and Send | On touch, Return adds a line: the textarea holds `first line\nsecond line` and nothing is sent. Tapping Send submits, and the fake reply appears. ⌘/Ctrl-Enter sends only under `(pointer: fine)`, as in TS |
| Apple accessibility audit: chat, picker, history drawer, Admin, Lab | No contrast, label or element-order issues. All hit-area issues on our elements are fixed (see below). Three findings remain, none from our markup: Safari's own toolbar reload button; one nil element with a zero frame, which also appears on a bare HTML probe page with no LocalBase code; and Dynamic Type (see below) |
| axe-core, Chrome, 64 runs | Two violations found and fixed. The Voices recording input had no label (critical); it is now `aria-labelledby`. The chat composer sat outside any landmark (best practice); it is now a form landmark, "Message composer". The final run passed with no violations, including best practice |
| Microphone dictation, end to end | Passed (2026-10-21), through `tools/mic-check.sh` in desktop Chrome. A fake capture device plays a speech clip (macOS `say`, re-encoded with the pinned ffmpeg to plain 16-bit PCM). Chat's microphone records it through `getUserMedia`, the page encodes the browser WAV, and the gateway's transcription route runs the real whisper-server with whisper-tiny-en. The transcript was "Please remind me to water the plants tomorrow morning. Please remind me to water the" (the clip loops), 4.3 s after the first tap with about 4 s of recording. The iOS Simulator cannot be fed a clip, so iOS Safari's own capture was not exercised; Chrome needs its audio service in-process to read the file |

**Hit areas on iOS.** iOS reports a link's or a label's frame as the frame
of its text, not its box. Apple's audit therefore flags any text under
16px, which gives a frame under 20 pt, as "Hit area is too small", even
when the box is 44px tall.

A probe page settled the cause:
- a 44px `inline-flex` link with 14px text is flagged, with or without an
  `aria-label` or an inner block;
- `<button>` and `role="button"` are reported at their full 44 pt box;
- 16px text is not flagged.

The fixes put text in controls at 16px:
- `.btn` and `.chip` are `text-base`;
- `text-sm` is gone from links, buttons, summaries and labels: the header
  Admin and Settings links, the Lab tabs, the drawer section links, Manage
  models, and the picker's detail line;
- the eyebrow is now 16px semibold in sentence case, instead of 12px
  uppercase;
- the model picker button is 44px tall (it was 32px).

The Lab's "Choose one in Get models" is now a single link, with no stray
"." text. `TestInteractiveTextAtLeast16px` keeps these fixes in place.

A later run on a dedicated Simulator ("LBGO Check iPhone 17"), free of
the other session's device runner, passed both tests. Its audit also
flagged the picker's "Not installed" group heading (14px), which the
fixture showed once an uninstalled model was selected; it is now 16px.

**Dynamic Type** ("partially unsupported") is reported on every page.
The UI sizes text in rem from a fixed root size, so iOS text-size settings
do not scale it; Safari's page zoom and reflow at 320px still work. This
is outside the gate's categories. Supporting it would mean setting the
root to `-apple-system-body`, a type-scale change for its own review.

### 17.10 Release candidate and the service drill (2026-10-21)

**Release candidate 0.2.0** (57e132d7; fe3e004d and b621798a change only
the Chat script, the tools and this document):
- CI is green on macOS and Linux.
- The dual contract suite passed 96 tests. Two failed on memory and timing
  in the full run and passed when rerun:
  - model management, which is difference 4 of §13.6;
  - the video job failure: both gateways answered `insufficient_memory`,
    sampled moments apart.
- `tools/systemd-check.sh` passed all 24 steps.

**Drill on a fresh copy, after #368 went live.**
- **The copy and the swap target.** The copy was taken under the idle
  preflight (0 inference started in the last 60 s), with key2 inside. The
  TS build for the swap came from a `cdb45205` export (`bun run build`),
  byte-identical to the live binary (sha256 `b304f542…`).
- **Reduced mode.** Available memory stayed under 25 GiB for 10 minutes
  (best 23.2 GiB), so the drill ran in its reduced mode (`LBGO_LIGHT=1`):
  - the copy's active LLM was the 1.5B for the smokes;
  - the replay sent no decision request;
  - the preflight needed 14 GiB;
  - the 12 GiB abort floor and the live `/health` watchdog stayed on.
- **Deferred.** The smoke on qwen3.5-9b and the replay's decision request
  wait for 25 GiB of headroom.

| Step | Result |
| --- | --- |
| Media | No TTS, image or video model selected before the start; `lsof`: the copy listened only on loopback scratch ports |
| Go service, throwaway label | Started, healthy; the plist runs the copy's binary; smoke on the 1.5B passed |
| Replay and contract subset (key2) | Passed: 9 models listed and retrieved; chat; a stream of 3 chunks, then `[DONE]` |
| `config apply --restart auto --wait` | Activation "restarted"; healthy on the new port; acknowledged |
| `defaultLlmModels` (run 1) | Applied; `status`: "Default chat model: qwen3.5-9b-q4_k_m (Using Qwen3.5 9B: Qwen3.6 35B-A3B needs more free memory than is available.)" |
| `config apply --restart never`, then `restart` | Pending restart recorded, applied, cleared |
| Rollback: binary swap to TS | TS 0.1.0 restarted under the same label and plist path; smoke and replay passed; **16 s** to a passing smoke |
| Forward to Go | Smoke passed in 14 s |
| Removal | `uninstall --yes`: plist removed, label unloaded, copy root removed; the live label still loaded |
| launchd | During the drill, only the throwaway label and its plist were added. Afterwards, labels and LaunchAgents were identical to before. Spotlight's transient `com.apple.mdworker.shared.*` workers churned and are recorded apart |
| Guard | Lowest 19.0 GiB available; live `/health` 200 on all 20 probes |

**Run 1.** The first run (same copy procedure) passed every product step,
with 16 s to a rollback smoke. It failed five checks, each of them in the
tooling:
- two plist checks: `plutil`'s JSON escapes `/`, so the check now compares
  `ProgramArguments.0` raw;
- the contract subset's media-set assertion, which no longer applies to an
  external gateway;
- two launchd comparisons that counted Spotlight's transient workers.

**`--no-auth` UI check on the copy.** Media was off, and the default list
was `["qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-9b-q4_k_m"]`.
- Chat showed "Qwen3.5 9B" with "Using Qwen3.5 9B: Qwen3.6 35B-A3B needs
  more free memory than is available."
- With the 1.5B picked explicitly, a real reply arrived in 4.5 s.
- The Settings card showed the same choice and reason.

**Cleanup.** The copy and clones (`/private/tmp/lbgo-shadow`) and the TS
builds were deleted. No throwaway label or plist remains.

**The two contract tests that passed only on rerun** (operator request:
five isolated runs each, before cutover):
- `TestDifferentialModelManagement`: 5 of 5.
- `TestDifferentialVideoJobFailure`: 3 of 5 at first. A test bug, not a
  gateway one. Both gateways refused the job for memory with the same code,
  demand (13.7 GiB) and wording. The message names the usable memory
  sampled when each job starts, the two jobs run one after the other, and
  the sample moved by 0.1 GiB. The test now masks only that figure. After
  the fix: 5 of 5.

**TS bug, not in Go: `models show` status** (operator, live, 2026-10-21).
- **Symptom:** TS printed "Installed · Disabled" for qwen3.5-9b and
  qwen3.6, although both are selected (and the 9B is active).
- **Cause:** TS's show command declares `requiresDatabase: false` without
  `readOnlyConfiguration: true`, so its context uses `defaultConfig(root)`.
  Enabled and Default then come from fresh-install defaults, not the
  saved row.
- **Go:** `models show` opens the saved configuration.
  `TestModelsShowStatusFromSavedConfig` checks the text and JSON status
  for selected, unselected and active models.
- **`models list`:** it prints installed files only, in both
  implementations, so it has no enabled state to get wrong.

## 18. Fit-aware default chat model (Go only; operator, 2026-10-21)

A new capability; TS has no equivalent. The user's words: "Yes 3.6 should
be my default chat model now when it will fit in VRAM."

**Configuration.** `defaultLlmModels` is an ordered list of chat models.
- **Where it lives.** `[models] defaultLlmModels` in `config show` and
  `config apply` (rendered only when set, so a TS-shaped document is
  unchanged), `configure --default-llm-models a,b`, and the "Default chat
  model" card in Settings (models:manage; read with models:read), backed
  by `GET/POST /_localbase/default-chat-models`.
- **Storage.** It is stored in `go_settings`
  (`models.default-llm-models`). The TS config row and journal are
  untouched, and TS ignores the table, so rollback is a binary swap as
  before.
- **Rules.** Entries must be selected chat models (not decision or
  embedding models), without duplicates. Disabling a model removes it
  from the list.
- **One entry.** It is the active LLM itself and stores no list; that is
  today's behaviour exactly, sticky active model included.
- **Two or more.** Setting the list leaves `activeLlmModel` alone, so it
  keeps its TS meaning: the chat model last activated, persisted after a
  switch. TS launches it at start after a rollback, without the fit
  fallback, so a list must never set it to a model that may not fit.
- **Chat models only** (fixed after cutover, 2026-10-21). After a decision
  request, live `status` reported clef-flash as the default chat model.
  - **The cause:** a switch to a decision or embedding model made it the
    active LLM, and a one-entry default follows the active LLM.
  - **Now:** only a chat model becomes active. The one-entry default is
    the active LLM when it is a chat model, otherwise the first selected
    chat model (`TestDecisionRequestDoesNotBecomeDefault`).
  - **Also fixed:** `configure --default-llm-models` and the Settings card
    set a list without moving `activeLlmModel`
    (`TestConfigureDefaultsLeaveActive`).
- **When changes apply.** As with every model setting, a CLI `config
  apply` reaches a running gateway at its next start, as in TS; the
  Settings card and the API apply at once.
- **At cutover.** The live install is set to
  `["qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-9b-q4_k_m"]`. Fresh installs keep
  #368's one-entry qwen3.5-4b default.

**Where it applies.** Only where the caller chose no model.
- TS requires `model` on chat and decision requests, so the surface is
  the browser's new conversation: the page load and each "New
  conversation", via `GET /app/chat/default`.
- `LLM.Resolve("")` also uses it for internal callers.
- A request that names a model is never substituted: it gets that model,
  or its real admission error (a retryable 503). Naya and OpenCode name
  their models and are unaffected.

**Choice.**
- **The rule.** Take the first default that would pass memory admission
  now. The check is `fitsNow`: the switch preflight's
  `Memory.CheckAdmission` on the model's admission demand, crediting the
  LLM slot (switch credit) and every runtime idle recovery could stop. It
  stops nothing and makes no separate estimate.
- **Not installed.** A default that is not installed does not fit.
- **None fits.** The last default is used, and it gets the normal
  admission error.
- **The running model.** It fits where it runs.

**Stability.**
- **Decision points only.** The choice is re-evaluated only at a page
  load or a new conversation, and only when the LLM is idle (no active,
  queued or starting request). It never changes mid-stream, and a
  conversation keeps the model it started with (the browser stores it per
  conversation).
- **Dwell.** After a change the choice holds for at least 30 s.
- **Upgrade hysteresis.** Moving back up the list needs the earlier model
  to fit with a margin (the larger of 1 GiB and 10% of its demand) at 3
  decision points in a row, like the pressure monitor's recovery count.
  Moving down when the current choice no longer fits is immediate.

**Visible.**
- **Chat.** The empty conversation shows the reason under its heading,
  for example "Using Qwen3.5 9B: Qwen3.6-35B-A3B needs more free memory
  than is available." Names come from the catalog. Other reasons: "is not
  installed", "cannot start: <the backend's error>", and, while an
  upgrade settles, "fits again; LocalBase switches back once it has room
  to spare".
- **Settings.** The card shows the model used now and the reason.
- **Log.** Each change logs `model.default-selected` (info), with the
  preferred id, the chosen id, the previous id and the admission cause
  (`insufficient_memory`, `not_installed`, `settling` or the error).
- **`status`.** It prints "Default chat model: <id> (<reason>)" and adds
  `defaultChatModel` to its JSON. It reads them from
  `/_localbase/default-model`, which needs the instance token, like
  `/_localbase/instance`. The unauthenticated `/health` names no models.

**Tests.**
- `internal/runtime/default_llm_test.go`: preferred fits; fallback with
  its reason and event; back to the preferred only after the dwell and 3
  margin samples; none fits → the last one, with its admission error; an
  explicit request is never substituted; no change while a request
  streams; a single entry is the active model.
- `internal/conf/defaults_test.go`: the document, plan and validation,
  and the store round trip.
- `internal/gateway/default_models_test.go`: the API and the token gate.

**Live context.** Qwen3.6's planning demand is about 21 GiB against a
reserve of about 10.3 GiB. Available memory is usually around 23 GiB
with the user's apps open, so the fallback will often be what runs.

## 19. Observability additions and the NVIDIA VRAM gap (operator audit, 2026-10-24)

The audit covered three days of live Mac Loki and Tempo data, plus a
serialized Linux run of 6b908f76 on the Pixelator. It found no secrets in
logs, Loki or Tempo, and the 429 queue-full and timeout responses and the
SIGKILL → `runtime.exited` path were all captured. Four gaps, fixed here.

### 19.1 Runtime stop events

Before, a model swap showed only a start. Every backend stop now logs one
event next to `runtime.starting`, `runtime.ready` and `runtime.exited`:
- `runtime.evicted` when memory was the reason: idle recovery, the pressure
  monitor, or critical memory (`EmergencyStop`);
- `runtime.stopped` otherwise.

| Field | Meaning |
| --- | --- |
| `runtime` | `llm`, `stt`, `tts`, `image` or `video` |
| `model_id` | the model that was running |
| `reason` | `memory`, `switch` (another model replaced it), `shutdown`, `crash` (after `runtime.exited`) or `cancel` (a video job's backend killed) |
| `memory_freed_bytes` | the reservation released, i.e. the admission ledger's projection, not a footprint sample |
| `pid` | the backend process |

- `idle` is reserved: there is no idle-unload timer yet.
- `TestStopEvents` checks switch, crash, eviction and shutdown.

### 19.2 Error kind on request logs and spans

A 4xx or 5xx response's error envelope is read, at most 8 KiB of it, for
its `type` and `code` only, never its message. They become:
- `error_type` and `error_code` on the `http.request` log;
- `localbase.error.type`, `localbase.error.code` and `error.type` (the
  code) on the server span.

This works for every error writer, whether gateway-made or relayed from a
backend. TS has no equivalent, so the OTLP differential test skips these
keys and asserts them on Go. Tests: `TestHTTPRequestLogsErrorKind`, which
also checks that no message text is logged, and `TestDifferentialOTLP`.

### 19.3 NVIDIA GPUs on Linux: reported, not admitted

On the Pixelator, doctor said "CPU / Integrated Graphics, 0 GB VRAM", while
the backend host found CUDA and a custom CUDA backend worked. Admission
rejections named `pool=system` with nothing about the GPU. Full NVML
admission stays deferred (§11.3); the gap is now explicit:
- `memory.DetectSpecs` lists the GPUs `nvidia-smi
  --query-gpu=name,memory.total` reports (`nvidiaGpus` in `doctor --json`).
- Doctor prints each one, then "GPU VRAM: NVIDIA GPU present, VRAM
  admission not qualified on this build; models are admitted against system
  memory." In place of a VRAM-based fit line, the status says the same.
- An admission rejection on such a host keeps `pool: system` and adds
  `nvidia_gpu_present: true` and `vram_admission: not_qualified`. Hosts
  without an NVIDIA GPU log exactly as before.
- Tests: `TestParseNvidiaSMI`, `TestDoctorNVIDIALines` and
  `TestAdmissionRejectionNamesPoolWithNVIDIA`.

**Design note: real VRAM admission (not started).**
- **Pools:** a `gpu:<index>` pool per device, sampled through NVML (cgo-free
  via `nvidia-smi --query-gpu=memory.used,memory.total` at the 2 s
  pressure cadence, or `libnvidia-ml` via purego, which §11 rules out). It
  sits next to the system pool. A model whose launch offloads layers
  reserves in both: GPU bytes for the offloaded weights, KV cache and
  compute buffers, and host bytes for the rest.
- **Plans:** the launch plan needs `-ngl` and a per-device split.
  Estimates exist for unified memory only; discrete needs measured peaks
  per GPU (`nvidia-smi --query-compute-apps=pid,used_memory`) feeding the
  same observed-peak table under a new method.
- **Pressure:** VRAM is not pageable, so a GPU pool has no "constrained"
  state, only admit or refuse. Eviction recovery still applies.
- **Qualification:** on the Pixelator, through its serialized queue (never
  without the operator), against the TS-free Linux CI plus a real 12 GB
  card.

## 20. CPU/GPU placement: automatic, with advanced overrides (design addendum, 2026-10-25)

User-approved (benchmark thread). This is capacity placement: where a model's
tensors live so that it fits. It is not a claim that offload is fast. Product
and catalog text name no measurements or rankings; the qualification
evidence stays internal, as test fixtures. Out of scope: catalog or default
changes, new runtimes, and anything on the live gateway.

### 20.1 Modes and settings

A placement is chosen per model and per device. It is device-local, like the
backend choice (§12), and stored in a new Go-only table `model_placements`
(model_id, device_id, mode, gpu_layers, cpu_moe_layers, ctx_size,
updated_at). The Go schema version goes to 2, created idempotently; TS
ignores the table, so rollback stays a binary swap. No row means `auto`.

| `mode` | Meaning | llama-server arguments |
| --- | --- | --- |
| `auto` (default) | Everything on the GPU when it fits; otherwise the smallest conservative CPU offload that fits (§20.3) | none, or `--n-cpu-moe N` / `-ngl L` from the plan |
| `gpu` | GPU-only. If it does not fit, the request is refused for memory as today. It never falls back to an offload | none |
| `cpu` | CPU-only | `-ngl 0` |
| `custom` | `cpuMoeLayers` (MoE only), `gpuLayers` and `contextSize`, each optional | `--n-cpu-moe N`, `-ngl L`, `-c` |

- `cpuMoeLayers` is llama.cpp `--n-cpu-moe N`: the expert tensors (weights
  and biases matching `blk.<i>.ffn_(up|down|gate|gate_up)_(ch|)exps`) of
  layers 0…N-1 stay in host memory.
- `gpuLayers` is ordinary partial offload (`-ngl`): this many repeating
  layers, counted from the output end, on the GPU.
- `contextSize` overrides the configured context ceiling for this model
  only (clamped as today to the model's window, with the same auto or
  explicit rule).

### 20.2 Surfaces

- **API:** `GET` (models:read), `PUT` and `DELETE` (models:manage) at
  `/_localbase/models/{id}/placement`. The body is `{"mode":
  "auto"|"gpu"|"cpu"|"custom", "cpuMoeLayers"?, "gpuLayers"?,
  "contextSize"?}`. The response is `{"requested": {…}, "resolved":
  Placement}`. `/_localbase/models/{id}` gains `device.placement`, the
  resolved Placement.
- **Placement** (resolved, JSON):
  - `mode`;
  - `platform`: `unified`, `discrete-nvidia` or `cpu`;
  - `pool`: `system` or `gpu:<index>`;
  - `gpuLayers` and `totalLayers`;
  - `cpuMoeLayers` and `hostTensors` (count), `hostLayers` (indices);
  - `gpuBytes`, `hostBytes`, `kvBytes`, `workspaceBytes`, `reserveBytes`,
    `freeVramBytes`;
  - `support`: `{nCpuMoe, gpuLayers, cpuOnly}`, each with a reason when
    false;
  - `note`, a short factual line, for example "Unified memory: GPU and CPU
    share the same memory, so offloading frees nothing."
- **CLI:** `local-base models placement <id>` shows the resolved split.
  Setting it: `--mode auto|gpu|cpu|custom`, `--cpu-moe-layers N`,
  `--gpu-layers N`, `--context-size N`, and `--reset` to go back to auto.
  `models show` adds a "Placement" block with the same lines.
- **Doctor:** per GPU, `nvidia-smi` free and total VRAM and the reserve. It
  says whether placement planning is available on this machine: "discrete
  NVIDIA: planned against free VRAM" or "unified memory: not applicable".
  This replaces #377's "VRAM admission not qualified" line where §20.4
  now applies.
- **UI:** the model detail page gets a "Placement" card (models:manage):
  the resolved split in words, for example "Experts of layers 0–3 stay in
  system memory (1.6 GB); everything else on the GPU", plus the mode
  selector and the Custom fields. Wording is generic and names no models.
- **Errors.** Unsupported overrides are 400 `invalid_request_error`, code
  `placement_unsupported`, with an actionable message:
  - `cpuMoeLayers` on a model with no expert tensors;
  - `cpuMoeLayers` on a backend without `--n-cpu-moe` (checked against the
    pinned runtime's flag set; a custom build is probed once with
    `--help`);
  - `gpuLayers` > 0 on a CPU-only backend;
  - `cpuMoeLayers` on unified memory;
  - values out of range.
  A placement that cannot fit at all is 409 `placement_does_not_fit`, with
  the numbers: needed versus free VRAM and host RAM.

### 20.3 The planner (`internal/placement`, pure, no model load)

Inputs:
- the GGUF tensor descriptors (name, dims, ggml type → bytes using
  llama.cpp's block sizes, e.g. MXFP4 17 bytes per 32 values, Q4_K 144 per
  256, F32 4 per value), read from the header only;
- the architecture geometry (§3, `ReadGeometry`);
- the launch (context, slots, KV cache types);
- the device: free VRAM per GPU and available host RAM.

Margins match admission:
- workspace 256 MiB on the GPU;
- GPU reserve 512 MiB (the VRAM floor);
- host side: the existing system reserve (§3).

GPU need = resident weights on the GPU + KV (`KVCacheBytes`) + workspace +
reserve.

Auto:
1. All on the GPU if that fits.
2. Otherwise, for an MoE model: the smallest N with the first N layers'
   experts on the host that fits.
3. Otherwise, for a dense model or when N runs out: the largest `-ngl` that
   fits.
4. The host side must still fit the system pool, or the plan is "does not
   fit", answered as an admission error. Nothing is half-started.

Golden test: on the GPT-OSS-20B header descriptors and the reference free
VRAM, the planner reproduces from the header alone:
- 144 expert tensors over 24 layers, 424,120,320 B per layer;
- KV at 4K = 76,873,728 B;
- minimum N per context 4/8/16/32/64/128K = 1/1/1/1/2/4;
- the qualified N = 4 for the whole ladder (1,696,481,280 B on host).

### 20.4 Discrete NVIDIA (Linux): the minimum of §19.3

- **Pools:** a `gpu:<index>` pool per device, sampled from `nvidia-smi
  --query-gpu=index,memory.free,memory.total` (2 s cadence when it is
  used), with the 512 MiB reserve. A placement on a discrete GPU reserves
  in both pools: `gpuBytes` in `gpu:<i>` and `hostBytes` in `system`. The
  system admission, pressure monitor and eviction stay as they are, and the
  GPU pool is admit-or-refuse only.
- **Single GPU:** one device (index 0) only. Multi-GPU split is out of
  scope and refused clearly.
- **Qualification:** CUDA qualification runs later on the Pixelator through
  the DFS-owned queue with the user. This repo ships the code and the tests
  with synthetic fixtures only.

### 20.5 Startup placement check (fail closed)

The llama-server output is read as bytes; a strict UTF-8 decode once
crashed a harness.
- **Experts:** with `--n-cpu-moe N`, every expected expert tensor of layers
  < N must appear in `tensor <name> (…) buffer type overridden to <buft>`
  with `<buft>` `CPU*` or `CUDA_Host`. Any of them on `CUDA0` (or another
  device buffer), any missing, or any extra host expert stops the backend
  with `placement_mismatch` (503 for that request, `backend.start-failed`
  with the diff).
- **Layers:** with `-ngl L`, the "offloaded L/T layers to GPU" line must
  match the plan.
- Tests use synthetic logs: accept CPU/CUDA_Host, reject CUDA0, reject
  missing, survive invalid UTF-8.

### 20.6 Apple Silicon

Memory is unified: offloading to the CPU frees no memory, so VRAM-based
planning does not apply.
- `auto` keeps today's full-GPU (Metal) launch, and admission is unchanged.
- `gpu` is the same as `auto`.
- `cpu` (`-ngl 0`) and `custom.gpuLayers` are honoured as plain launch
  choices.
- `custom.cpuMoeLayers` is refused with "On unified memory, keeping experts
  in CPU memory frees no memory; this setting applies to discrete GPUs."
- The resolved Placement says `platform: unified` with that note.

### 20.7 Order of work (draft PRs stacked on #377)

1. GGUF tensor descriptors and the planner, with the golden test on the
   committed descriptor fixture (`testdata/gptoss20b-tensors.json`,
   internal evidence, header-derived only).
2. The placement validator with synthetic native logs (bytes).
3. Storage, API, CLI, `models show`, doctor, and launch arguments wired
   through the LLM runtime, fail closed.
4. The NVIDIA `gpu:<index>` pool (free VRAM plus reserve) for auto
   planning and dual-pool admission, with a fake `nvidia-smi` in tests.
5. The UI card.

### 20.8 Operator adjustments (approval, 2026-10-25)

1. **Rollback safety.** The deployed Go binary refuses a database whose
   `go_schema` version is newer than its own (`applyGoSchema`). So
   `model_placements` is created idempotently and the version stays at 1.
   A drill checks the previous Go binary and TS 0.1.0 against a scratch
   database that has the table (§20.9).
2. **Context: one source of truth.** Placement's `contextSize` reads and
   writes the existing shared setting, `ctxSize` with `ctxMode` (#293, §2).
   Setting it saves `ctxSize` with `ctxMode = "explicit"`. It applies to
   every model, and the UI and CLI say so. Nothing is stored per model.
   Precedence, highest first:
   1. `serve --ctx-size` (the process override);
   2. the saved `ctxSize`/`ctxMode` (the same value placement shows);
   3. the model's context window, which clamps both as today.
3. **Stable plans.** A plan is resolved when the model loads, and the
   runtime record keeps it (with `plannedAt` and the free VRAM it saw)
   until the model unloads. It is re-planned only on a load, a reload or
   an explicit change, never per request. Surfaces show "planned at
   <time> against <free VRAM>".
4. **Workspace check.** At startup the logged compute buffer (CUDA0 and
   CUDA_Host compute buffer sizes) must not exceed the planned workspace
   by more than 10% or 64 MiB, whichever is larger. Otherwise the start
   fails closed with `placement_mismatch`, and the error names both
   numbers.
5. **Copy.** Capacity and CPU use only: host memory used, which layers or
   experts are on the host, and what auto did and why. No speed claims,
   no model names, no rankings.
6. **Changes and loads.** `PUT` and `DELETE` are refused with 409
   `model_loading` while the model is starting. A change to a loaded model
   is stored and shown as "applies on next load". The running backend
   keeps its plan, and the runtime is never restarted implicitly.
7. **Multi-GPU.** Refused, with the instruction to set
   `CUDA_VISIBLE_DEVICES` to one GPU or choose CPU-only.

### 20.9 Rollback drill for `model_placements` (2026-10-25)

`tools/placement-rollback-check.sh` builds a scratch root shaped like a
real one:
- configured, with only the LLM selected;
- holding an APFS clone of a real model file (read-only source);
- with a placement row.

It then starts copies (never `~/.local/bin` itself) of the previous Go
binary and TS 0.1.0 on loopback scratch ports, and finally this build.

**Result.** All three serve `/health` and `/v1/models`, and after each one
`go_schema` is still 1 and the placement row is kept:

| Binary | Result |
| --- | --- |
| previous Go, c183621 | passed |
| TS 0.1.0 | passed (it loads the 1.5B at start) |
| this build | passed |

`TestGoSchemaVersionForRollback` fails if the version is raised without
this plan.

**Wiring (PR 3).**
- **Resolution:** placement is resolved in the LLM switch (`place`) before
  admission, kept on the load's plan (`Placement`, `PlacedAt`,
  `PlacementRequest`), appended to the llama-server arguments and checked
  after `/health` (`checkPlacement`). A mismatch stops the backend and
  answers 503 `placement_mismatch`.
- **Request errors:**
  - an unsupported stored choice: 409 `placement_unsupported`;
  - not fitting now: 503 `placement_does_not_fit` with `Retry-After`.
- **API:** `/_localbase/models/{id}/placement` (GET, PUT, DELETE).
  `/_localbase/models/{id}` gains `device.placement`, which is the preview
  until the model loads.
- **CLI:** `models placement`; `models show` adds a Placement block in text
  only, so its JSON keeps TS's shape.
- **Local CLI channel:** the token-gated `/_localbase/placement-state`
  tells the CLI whether a model is loading or loaded.
- **Tests:**
  - `TestPlacementAutoOffloadsExperts`, `TestPlacementMismatchFailsClosed`
    and `TestPlacementGPUOnlyNeverFallsBack` (runtime, with the fake
    backend printing llama.cpp's placement lines);
  - `TestPlacementAPI`, `TestPlacementAppliesOnNextLoad` and
    `TestModelMetadataPlacement` (gateway);
  - `TestModelsPlacementCLI` and `TestPlacementStore`.

### 20.10 The gpu:0 pool and doctor (PR 4)

**The pool.** `memory.GPUPool` admits a discrete placement's `gpuBytes`
on GPU 0 against an `nvidia-smi` sample taken at start time.
- **Formula:** free − 512 MiB reserve − loads not yet resident ≥ request.
  A load counts as pending until it is resident, after which the sample
  shows its usage. A request larger than the card is permanent.
- **In the LLM:** the pool reserves next to the system pool before
  starting. A refusal is the usual retryable 503 `insufficient_memory`,
  naming GPU memory, and the log names `pool: gpu:0`.
- **System pool:** unchanged, with its pressure monitor and eviction.
- **Tests:** `TestGPUPool` and `TestPlacementGPUPool`.

**Doctor.** This supersedes #377's "VRAM admission not qualified" wording
(§19.3).
- Per NVIDIA GPU: free and total memory.
- **One GPU:** "Placement: discrete NVIDIA GPU; models are planned against
  its free memory (0.5 GB kept free), and what does not fit stays in system
  memory."
- **Several GPUs:** the instruction to set `CUDA_VISIBLE_DEVICES` to one
  GPU.
- **Unified memory:** "placement planning does not apply".
- **No GPU:** "models run on the CPU".
- **Admission log:** rejections on an NVIDIA host carry
  `vram_admission: gpu:0`.
- **Test:** `TestDoctorNVIDIALines`.

**Linux CUDA qualification** is the operator's, with the user, through the
Pixelator's DFS-owned queue. Nothing here ran on it.

### 20.11 The Placement card (PR 5)

The model detail page (`/app/models/m/{id}`) shows a Placement card for
chat models. Only accounts with models:manage can change it; others see
it read-only.
- **"When it loads"** (before a load) or **"Next load"**: the split in
  words. It covers the platform note, GPU layers out of total layers, and
  which expert layers stay in system memory and how many bytes they take.
- **"Loaded now":** shown once the model is loaded. On discrete NVIDIA it
  carries "Planned at <time> against <free> of free GPU memory". When the
  stored choice differs from the running split, an "Applies on next load"
  badge shows.
- **The form:** the mode selector, the two Custom fields and the shared
  context size, labelled "shared by all models". Save and "Back to auto"
  post to `POST /app/models/placement/{id}` (models:manage). The handler
  goes through the placement API, so it gets the same validation and the
  same mid-load refusal, and it re-renders only the card. An error shows in
  the card; the fields show the stored choice again.
- **The next-load preview carries no `plannedAt`.** The API's `next` and
  metadata's `device.placement` preview drop it, because only a load is
  planned.
- **Copy:** capacity and CPU use only, with no model names.
- **Checked:** at 390 px, with no horizontal overflow and 44 px controls.
- **Tests:** `TestPlacementCard` and `TestPlacementCardSaves`.

## 21. Detached runs: generation that outlives the page (design addendum, 2026-10-27)

User request (operator, 2026-10-27): "make LocalBase work so that it
continues generation even if the FE is moved on the wrong tab."

**Today (verified).** The UI is client-driven.
- **How it breaks:** `chat-core.js` reads a `fetch()` stream of
  `/app/api/v1/chat/completions`. `relay` builds the backend request on
  `r.Context()`. Anything that drops the connection cancels the
  generation upstream: navigating away, a page change in the multi-page
  app, or iOS suspending a background tab.
- **Lab:** its image, speech and transcribe POSTs call the API with the
  page request's context too (`u.call(r.Context())`).
- **Test:** `TestChatDisconnectCancelsUpstream` pins this. The fake backend
  sees the cancellation, and the gateway logs `inference.completed` with
  `terminal_source: request_aborted`.

**Goal.** A *run* is a server-side execution of one inference request.
- It does not depend on any browser connection.
- A returning page re-attaches to it: it replays what it missed, then
  follows live.
- Only an explicit Stop cancels it.

**Non-goals.**
- `/v1/*` semantics are unchanged. API clients keep disconnect-cancels, and
  the test above stays.
- No server-side conversation history: the browser still owns history.
- No new runtime and no schema change.
- Video already has jobs (`/v1/videos`), so it is not wrapped again.

### 21.1 Execution: the same path as the API

`POST /_localbase/runs/v1/<route>` takes exactly the body of `/v1/<route>`
(JSON or multipart, ≤ 25 MiB).
- **Targets:** PR 1 has `chat/completions`. PR 2 adds `images/generations`,
  `images/edits`, `audio/speech` and `audio/transcriptions`. Other routes
  get 404.
- **How it executes:** the gateway dispatches the body to the real `/v1`
  handler internally (`Server.ServeHTTP`) as the caller's principal. That
  handler runs on a context detached from the creating request
  (`context.WithoutCancel` plus the run's own cancel).
- **What that gives for free:** validation, permission scopes, admission,
  the queue, memory pressure, telemetry, the inner request's own
  `request_id`, `inference.completed`, and its spans. They are identical to
  the API's because they are the API's code.
- **Validation:** the internal call's 4xx comes back to the creator
  synchronously. The run is not created, and the client sees the same
  error envelope `/v1` gives.
- **Chat runs always stream upstream,** with `stream: true` and
  `stream_options.include_usage: true`, so partial output exists at every
  moment.
- **The response is 202** with the run object. Its `Location` header is
  `/_localbase/runs/{id}`.

- **Queued runs hold nothing.** A run waits in the inference queue exactly
  like a request: no memory admission and no slot until it is admitted.
  Its status is `queued` until then, and Stop on a queued run is immediate.
  Once admitted it holds the lease the way a request does, and it releases
  the lease when the inner handler returns. Idle unload and eviction are
  unchanged: a finished run pins nothing.

### 21.2 API (final for the FE thread, 2026-10-27)

Every route also works through `/app/api/…` as the signed-in browser,
under the same same-origin and session rules as the other UI endpoints
(`X-Localbase-Ui: 1` on fetches). The event stream is the one exception,
§21.3. All run routes need an authenticated principal and are owner-bound.
- **Owner:** the creating principal's `OwnerID()`, so a browser account,
  an API key or the environment key. Under `--no-auth` it is one local
  owner.
- **Other principals** get 404 `run_not_found`, the same as an unknown id,
  so no existence leak.
- **Ids** are 128-bit random, `run_<32 hex>`.
- **Creating** needs the target route's scope (`inference:chat`). The
  inner `/v1` guard checks it, and a refusal is answered synchronously.

| Route | Answer |
| --- | --- |
| `POST /_localbase/runs/v1/{chat/completions, images/generations, images/edits, audio/speech, audio/transcriptions}` (PR 2 adds the media four; bodies are the `/v1` JSON or multipart, ≤ 32 MiB) | **202** with the run object (output so far is empty) and `Location: /_localbase/runs/{id}`. A request `/v1` refuses before the queue (validation, scope, unknown model) gets the **same status and error envelope** as `/v1`, and no run exists. **429** `too_many_runs` (with `Retry-After: 5`) over the active-run limit. **503** while the gateway stops. |
| `GET /_localbase/runs` | the caller's runs, newest first: `{"object":"list","data":[{id, target, label, model, status, reason?, createdAt, startedAt, endedAt}]}`. `label` is `"Chat"` (PR 2: `"Image"`, `"Image edit"`, `"Speech"`, `"Transcription"`). It carries **no prompt or output text**, and is meant for the shell's "Generating…" indicator. |
| `GET /_localbase/runs/{id}` | the run object with `output` |
| `GET /_localbase/runs/{id}/events` | SSE, §21.3 |
| `GET /_localbase/runs/{id}/result` | a completed media run's `/v1` response body, with its content type (image JSON with `b64_json`, speech audio, transcription JSON or text) and `Cache-Control: no-store`. **409** `run_result_unavailable` before completion, for chat, or after a restart |
| `POST /_localbase/runs/{id}/cancel` | **Stop**, the only thing that cancels a run. It returns the final run object, with the partial output kept. Stopping a finished run is a no-op that returns it. |
| `DELETE /_localbase/runs/{id}` | **dismiss:** stops the run if active, deletes it and its checkpoint. Returns `{"id","object":"localbase.run.deleted","deleted":true}` |

**Deliberate deviation from the operator's brief.** The brief had
`DELETE` = Stop. Here Stop is `POST …/cancel` and `DELETE` dismisses, as
with video jobs (`/v1/videos/{id}/cancel` and `DELETE`). A stopped run
then keeps its partial text until the page has collected it. The operator
accepted this on 2026-10-27.

**Run object.**

```json
{"id": "run_…", "object": "localbase.run",
 "target": "/v1/chat/completions", "label": "Chat", "model": "<model id>",
 "status": "queued|in_progress|completed|failed|cancelled|interrupted",
 "reason": "stopped|gateway_restart|memory_pressure|max_duration|output_limit",
 "error": null | {"type", "code", "message", ...},
 "requestId": "lbreq_…",
 "createdAt", "startedAt", "endedAt", "expiresAt": RFC 3339 or null,
 "lastEventId": 12,
 "resumable": false,
 "continuation": {"by": "client", "message": "…"},
 "output": {"content", "reasoning"?, "toolCalls"?, "finishReason"?, "usage"?},   // chat
 "result": {"contentType", "bytes", "url": "/_localbase/runs/{id}/result",
            "notes"?: ["audio_truncated"]}}  // media, once completed
```

- **`reason`** is present only when it applies. **`continuation`** is
  present only when the run is `interrupted`.
- **`resumable` is always false.** The server never resumes a run: it
  does not keep the request (§21.6). After `interrupted`, continuing is a
  client action. The page still has the conversation, and sends it again
  with the partial reply. `continuation.message` says so in product words.
- **`output`** is accumulated server-side from the stream, so `GET` alone
  is "the result so far".
- **`toolCalls`** are merged by index, in the OpenAI shape
  `{id, type, function: {name, arguments}}`.
- **`requestId`** is the inner `/v1` request's id. It joins the run to the
  http, inference and backend logs and spans.

### 21.3 Events and resume (final)

`GET /_localbase/runs/{id}/events` answers `200` with these headers:
- `Content-Type: text/event-stream; charset=utf-8`
- `Cache-Control: no-store, no-transform`
- `X-Accel-Buffering: no`

It opens with a `: connected` comment and flushes after every event.

**Events** (each with a per-run increasing `id:`):
- `event: status`, data `{"status","reason"?}`: every transition,
  starting with `queued`.
- `event: chunk`, data: an OpenAI `chat.completion.chunk`, byte for byte
  as `/v1` relays it (public model id, `reasoning` normalized). The
  client's existing chunk parser reads it.
- `event: error`, data: the error envelope (`{"error":{…}}`), when the
  stream ends in an error.
- `event: end`, data: the final run object without `output`. The server
  then closes the stream.
- `event: snapshot` (after a restart only), data: the run object with
  `output`, followed by `end`.
  - **Every** connection to a restored run gets both, whatever
    `Last-Event-ID` or `?after` it sends. The buffer is gone, and a
    resuming page's last id can be past the last checkpoint (written at
    most once a second).
  - They are numbered after the checkpointed event count, so their ids
    are stable across reconnects.
  - A client treats `snapshot` as replacing everything it rendered for the
    run. This was found by the FE thread on 2026-10-27: a resume with id 52
    used to receive nothing.

**Resume.** `Last-Event-ID: N` (sent automatically by `EventSource` on its
own reconnects) or `?after=N` (for a fresh `EventSource` on a returning
page) replays every buffered event with id > N, then follows live. The
header wins when both are present.
- With no id, the stream starts from the beginning.
- Several tabs can attach at once.
- Ids have no gaps, so a page that renders chunks in order and remembers
  the last id never duplicates or loses text.
- A page that lost its state can always rebuild from `GET` (output so
  far) plus `?after=<lastEventId>`.

**Keep-alive.** A `: ping` comment every 15 s keeps Cloudflare, the
tunnel and iOS Safari from closing a quiet stream. A quiet stream is one
queued behind another run, or a model reading a long prompt.

**Browser authentication.** `EventSource` cannot set headers, so through
`/app/api` the event stream (GET only) needs only the session cookie, the
exact host, and same-origin markers (`Sec-Fetch-Site: same-origin` and an
`Origin`, when the browser sends them). This is
`Browser.SameOriginStream`, and it is read-only: without CORS headers no
other site can read the stream.

The forwarding keeps only `after` from the query. Every mutating run
route keeps the full same-origin, `X-Localbase-Ui` and CSRF rules.

**Ownership.** The events route is owner-checked like the others. A
buffer belongs to one run, and a run to one principal, so a resume cannot
read across principals.

### 21.4 Lifecycle and statuses

`queued` → `in_progress` → `completed` | `failed` | `cancelled` |
`interrupted`.
- **`queued`:** waiting in the inference queue, holding no admission.
- **`in_progress`:** from admission (the lease is held).
- **`cancelled`,** reason `stopped`: the owner's Stop or dismiss. The
  partial output is kept until dismissed or expired.
- **`interrupted`:** the server stopped the run. The partial output is
  kept, and continuing is the client's (§21.2). Reasons:
  - `gateway_restart`: a graceful stop, or a crash found at the next start;
  - `memory_pressure`: the critical-memory monitor stopped the runtime
    mid-run. `/v1` reports that with a `service_unavailable` stream error,
    which is also in `error`;
  - `max_duration`: the run hit the limit.
- **`failed`:** `error` holds the `/v1` error envelope (an admission
  refusal after queueing, the backend failed, the context overflowed, …),
  or reason `output_limit`.

### 21.5 Limits

| Limit | Value | On exceeding |
| --- | --- | --- |
| Active runs (queued + in progress) per principal | 4 | 429 `too_many_runs` on create |
| Active runs per gateway | 32 | 429 `too_many_runs` |
| Retained runs per principal (any status) | 32 | the oldest *finished* run is dropped (`run.dismissed`) |
| Run duration | 30 min from creation | `interrupted` / `max_duration` |
| Buffered output per run (chunk bytes) | 8 MiB | generation stops upstream; `failed` / `output_limit` |
| Retention after a run ends | 30 min (`expiresAt`) | deleted with its checkpoint; `run.expired` |

Admission is unchanged: a run is a request, so it waits in the same queue
under the same queue timeout and capacity. A queue-timeout or memory
refusal ends the run as `failed` with the `/v1` envelope.

### 21.6 Privacy and storage

**The one place model output touches disk.** Everything else about a run
lives in memory.
- **The checkpoint:** `<root>/runs/<id>.json` (directory 0700, file 0600)
  holds the run object and its accumulated output, in plaintext. It is
  written at every status change and at most once a second while
  streaming.
- **Why it exists:** a restart, graceful or a crash, must report
  `interrupted` with the text the person already saw. Without the file the
  partial reply would silently vanish.
- **What is not on disk:** the request body (the prompt and the
  conversation) is held in memory only, while the run executes. It is
  never written and never returned.

**Deletion.** The file goes on:
- dismiss;
- expiry, 30 minutes after the run ends, checked every minute and on
  every runs request;
- retention trimming.

At start, the gateway sweeps `<root>/runs/`:
- every non-finished record becomes `interrupted` / `gateway_restart`,
  and its `expiresAt` is the start time plus 30 minutes;
- expired records and unreadable or foreign files are deleted.

Nothing outlives 30 minutes after the end of the run that made it. There
is no long-term conversation storage: the page collects the result into
browser-owned history, then dismisses the run.

**Excluded everywhere else.**
- `local-base diagnostics` bundles only logs and summaries, never
  `runs/`.
- `tools/shadow.sh` copies an allowlist of root entries that does not
  include `runs/`, and no tool copies a whole root.
- Tests:
  - `TestRunCheckpointsStayOutOfBundles` (Go-only);
  - the contract `TestCLIDiagnostics`, which plants a checkpoint holding
    `runOutputSecret55` next to the other planted secrets.

**Shutdown.** Graceful shutdown (`StopRuns`, before the HTTP server
drains) cancels active runs, marks them `interrupted` /
`gateway_restart`, checkpoints them, and closes every event stream.

**Rollback.** There is no database change. Older binaries ignore
`<root>/runs/`, and the next Go start sweeps it.

### 21.7 Telemetry (no content, ever)

JSONL and OTLP log events:
- `run.started`, at queueing
- `run.completed`
- `run.failed`
- `run.cancelled`
- `run.interrupted`
- `run.expired`
- `run.dismissed`

Attributes: `run_id`, `request_id` (the inner `/v1` request), `target`,
`model_id`, `owner` (the principal id, as in `auth_principal_id`),
`status`, `reason`, `error_code`, plus `duration_ms`, `output_bytes` (chunk
bytes, or the media result's size) and `events` once the run has ended. Message text, prompts and outputs are
never logged.

A `localbase.run` internal span parents the inner request's server span,
so a trace shows the run, then the request, the inference and the backend.

### 21.8 Tests and PRs

**Tests (`internal/gateway/runs_test.go`):**
- `TestChatDisconnectCancelsUpstream`: the `/v1` baseline, kept.
- `TestRunSurvivesDisconnect`: the creator and every reader go away, and
  the run completes upstream with the full text. The model is released
  (active 0, still loaded).
- `TestRunEventsResume`: `Last-Event-ID` and `?after=` replay exactly the
  missed events, with no gaps or duplicates, then live.
- `TestRunCancel`:
  - Stop cancels upstream (the fake backend sees it) and keeps the
    partial output;
  - a queued run (one active, one waiting, no admission) stops in under
    a second;
  - dismiss deletes the run and its checkpoint.
- `TestRunOwnership`: another key gets 404 on get, events, cancel and
  dismiss, and its list is empty. No credentials gets 401. A key without
  `inference:chat` gets 403, synchronously.
- `TestRunValidationSynchronous`: the same status and envelope as `/v1`,
  and no run exists afterwards.
- `TestRunRestartInterrupted`: covers both a crash (a new gateway on the
  same root) and a graceful stop. The run is `interrupted` /
  `gateway_restart` with the partial text, `resumable: false` and
  `continuation.by: "client"`, and its events are a snapshot then end.
- `TestRunExpiry`: checks the 0600 and 0700 modes. Expiry deletes the
  run and its file, and the startup sweep removes expired and junk files.
- `TestRunLimits`: the active-run cap returns 429; the output cap stops
  the generation upstream.
- `TestRunMemoryPressure`: `interrupted` / `memory_pressure`.
- `TestRunKeepAlive`: a quiet stream gets comments, plus the
  Cloudflare/Safari headers.
- `TestRunLogs`: the lifecycle events carry the ids and never the output
  text.
- `TestRunBrowserRoutes`: the `/app/api` allowlist, and `after` kept
  through forwarding.

The failure checks were confirmed by breaking the code on purpose:
- a run on the creator's context fails `TestRunSurvivesDisconnect`;
- ignoring `Last-Event-ID` fails `TestRunEventsResume`.

**PRs**, drafts:
- **PR 1** (`rewrite/go-runs`, on #382): the runs core, chat, and this
  section.
- **PR 2** stacks on PR 1.
  It adds the Lab targets (`images/generations`, `images/edits`,
  `audio/speech`, `audio/transcriptions`). Bodies may be multipart. The
  media result is served from `GET /_localbase/runs/{id}/result` with its
  content type, capped at 64 MiB, kept in memory only and never
  checkpointed. After a restart a media run reports `interrupted` with no
  result.

The FE thread owns the client.

### 21.9 Media runs (PR 2)

`images/generations`, `images/edits`, `audio/speech` and
`audio/transcriptions` are run targets.
- **Labels:** "Image", "Image edit", "Speech" and "Transcription".
- **Body:** passed through unchanged, JSON or multipart, so the `/v1`
  route applies its own limits and validation. A refusal before the queue
  is byte-for-byte `/v1`'s. The indicator's `model` is read from the JSON
  body or the multipart `model` field.
- **Hooks:** the image, speech and transcription handlers call the same
  hooks around their runtime's `Acquire` as chat. A media run is
  therefore `queued` without admission, `in_progress` once admitted, and
  `interrupted` / `memory_pressure` when the critical-memory monitor stops
  its runtime.
- **Events:** `status` events and `end`. Media has no partial output, so
  there are no `chunk` events.
- **Result:** the `/v1` response body and its content type, held in
  memory only, never checkpointed, capped at 64 MiB (over the cap:
  `failed` / `output_limit`). It is fetched from `GET …/result` and
  deleted with the run.
- **After a restart** the result is gone. A media run that had completed
  reports `interrupted` / `gateway_restart`, with no `result`. The page
  repeats the request if it still wants it.

**Tests:**
- `TestRunImage`: a fake sd-server run that completes, serves its result,
  keeps the result out of the checkpoint, and is `interrupted` with no
  result after a restart.
- `TestRunImageCancel`: Stop reaches the image backend.
- `TestRunImageEditMultipart`: multipart refusals are identical to
  `/v1`'s.
- `TestRunRequestModel`
- `TestRunMediaTargetsNotConfigured`
- `TestRunBrowserRoutes`: covers the media routes and `result`.

Speech and transcription have no fake runtime in the unit tests. Their
hooks are the same two calls, and the contract real runs exercise those
handlers.

## 22. Front-end redesign: shell, design system, keyboard (2026-10-27)

User request (operator, 2026-10-27): the UI must be responsive and
mobile-first, and each section should follow the best apps in its area.
On an iPhone the old header wrapped (Sign out on two lines), the six
sections were a two-row grid, and on the Lab the keyboard covered the
prompt. The research brief and mockups are kept outside the repository
(`.design/`, not committed); this section records the decisions.

### 22.1 Information architecture

- **Phones (< 1024 px):** a bottom tab bar with Chat, Models, Lab,
  Running and More. More is a bottom sheet with My voices, Settings,
  Admin (only with `access:read` or `keys:read`), the account and Sign out.
  Pages have a large title and no header row.
- **Desktop (≥ 1024 px):** a sidebar with every destination, and an
  account menu at its foot (the Popover API: no script, light dismiss,
  Escape) holding Sign out. On Chat the sidebar also holds New
  conversation, search and the conversation list; the phone drawer holds
  the same list.
- **Models** is one destination: Library and Get models are siblings under
  a segmented switch. Details, import and backends belong to it.
- The current tab is `aria-current="page"`; when More holds the current
  page its button is marked and says so to screen readers.
- Routes, permission checks, CSRF and htmx boosting are unchanged.
  `section()` in `web.go` maps a page to its destination.

### 22.2 Design system

Tokens stay CSS custom properties in `css/input.css` (light and dark by
`prefers-color-scheme`, as before), plus scrim, two elevations
(`shadow-pop`, `shadow-sheet`), one easing, and the shell geometry
(`--lb-tabbar-h`, `--lb-sidebar-w`, `--lb-kb`). Components:
`.page`, `.page-title`, `.sidebar`, `.nav-item`, `.tabbar`/`.tab`,
`.sheet` (a modal `<dialog>` at the bottom edge, slide-in unless reduced
motion), `.segmented`, `.list-group`/`.list-row`, `.menu`/`.menu-item`,
`.run-pill`, alongside the existing `.btn`, `.chip`, `.badge`, `.card`,
`.input`. `.input` now has the 44 px minimum height itself.

### 22.3 The keyboard

- Every page's viewport has `interactive-widget=resizes-content`, so Chrome
  on Android shrinks the layout for the keyboard. Zoom stays enabled.
- iOS Safari overlays the keyboard instead. `shell.js` measures the visual
  viewport and sets `--lb-kb` (the covered part of the layout viewport,
  0 when pinch-zoomed); `scroll-padding-bottom` uses it.
- While a text field has focus on a touch screen, `html[data-typing]`
  hides the tab bar and the "Generating…" pill, so the keyboard owns the
  bottom edge. A focused field the keyboard covers is scrolled into view
  once the keyboard settles. Docked composers (`[data-docked]`, the chat
  composer and later the Lab prompt) sit above the keyboard and opt out.

### 22.4 Detached runs in the UI

Generation outlives the page (§21). `static/js/runs.js` is the client:
`supported()`, `list()`/`active()`, `create(route, body, place)`,
`get`, `events(id)` (an SSE reader that resumes from the last event id it
rendered, kept per run in `localStorage` with the page to return to;
no content is stored), `cancel` (Stop), `dismiss` and `result`.
- Until the gateway answers `GET /_localbase/runs`, `supported()` is false
  and Chat and the Lab keep today's direct requests.
- The shell shows a quiet "Generating…" pill (sidebar, and above the tab
  bar on phones) for active runs, linking back to where each started; it
  refreshes on load, on navigation, when the tab is shown again and on a
  `localbase:runs` event. A page that shows the run itself sets
  `<body data-run-page>` to hide it there.
- Chat and the Lab move onto runs in their own PRs: Stop, an
  "Interrupted. Continue?" state, no lost text on tab switch, and a Lab
  results tray across tools.

### 22.5 Order of work (draft PRs stacked from `rewrite/go-fe-redesign`)

1. Shell, IA, tokens, keyboard handling, runs client (this).
2. Chat: composer, messages, actions, picker sheet, runs.
3. Models: Library and Get models (search, filters, states, one primary
   action per row).
4. Running now: memory, loaded models, queue.
5. Lab: docked prompt, results tray and history, runs.
6. My voices. 7. Settings (grouped lists). 8. Admin (keys, access).

### 22.6 Checks

- **Go tests (`shell_test.go`):** `TestSection`, `TestShellOnEveryPage`,
  `TestViewportKeyboardAndZoom`, `TestShellAccountAndAdmin`,
  `TestModelsSwitch`, `TestShellSignedOut`.
- **`tools/jstest/runs.test.ts`:** the event-stream parser under every
  chunking.
- **`tools/phone-check.sh <url>`** (headless Chrome against a `--no-auth`
  scratch gateway): at 360, 390, 430 and 1280 px every page has no
  horizontal overflow, controls of at least 44 px (inline text links
  exempt), the tab bar on phones and the sidebar on desktop, and nothing
  hidden under the tab bar; the More sheet takes focus, closes on Escape
  and returns focus; a focused field hides the tab bar and stays in view
  when the viewport shrinks for a keyboard. `tools/axe-check.sh` and
  `tools/ios-check.sh` still apply.

### 22.7 Chat (PR 2)

- **Detached replies.** When the gateway serves runs, each chat-completions
  request of a turn is a run (`runs.response`): its events are turned back
  into a `/v1` stream, so `streamText`, tool calls, reasoning, compaction
  and steering are unchanged. Stop (and steering) cancel the run and then
  dismiss it; leaving the page, switching tabs or a phone suspending the
  browser do not. A finished run is dismissed once its text is in the
  conversation.
- **Resume.** A dropped stream reconnects once the page is visible again
  (0.5 s to 15 s back-off) with `Last-Event-ID`, so nothing is lost or
  repeated. If a resume brings nothing (after a gateway restart the run's
  event ids start again; reported to the backend thread), the client reads
  the run and ends with its state.
- **Re-attach.** Each run remembers, in this browser, the conversation and
  reply it fills (`localStorage`, ids and the page link only). Opening that
  conversation, or the tab becoming visible, replays the run from the start
  into the reply. Earlier tool rounds of that reply are not replayed.
- **Interrupted.** A run the gateway stopped (restart, memory, time limit)
  keeps its text; the error offers **Continue**, which sends "Continue from
  where you stopped." as a new turn. Otherwise **Retry**.
- **Nothing lost on leaving.** Without saved history, the tab keeps its
  conversations in `sessionStorage` (text only, gone when the tab closes),
  and the unsent message is kept per tab. Saved history (`localStorage`)
  works as before.
- **Look.** The model picker and generation controls are bottom sheets on
  phones (centered on desktop). Header and composer use borderless round
  icon buttons; Send and Stop share a place. The newest reply offers Retry
  next to Copy. Choosing a conversation on a touch screen no longer opens
  the keyboard.
- **Checks.** `TestChatSheets`; `tools/jstest/runs.test.ts`
  (`runs.response`: streaming, resume from the last id, interrupted, the
  empty resume); `tools/chat-runs-check.sh` (headless Chrome against a
  scratch gateway with the fake llama-server: leave mid-reply and come back
  to the whole reply, the "Generating…" pill, Stop keeps the partial text,
  the draft survives, a dropped connection resumes without gaps or
  repeats). A gateway restart mid-reply was checked by hand: the reply
  keeps its text and offers Continue.

### 22.8 Models: Library and Get models (PR 3)

The one thing people come to do: Library, find and open a model they
have; Get models, add one that fits.
- **Header.** One title, "Models", the Library / Get models switch, and on
  the Library one round + ("Get a model"). The eyebrow, second heading and
  memory/disk card are gone; a single line says "N installed · size on
  disk · free" with a link to Running now (memory lives there).
- **Rows.** A grouped list; each row is one link (initial, name, task,
  default, size) with one state pill in words: Busy, Loaded, Off (not
  enabled) or Ready. Details stay on the model page.
- **Filters.** Task chips for the tasks present (Chat, Images, Speech,
  Transcription, Video, Embeddings, Decisions), one row that scrolls
  sideways on phones. `?filter=other` still works.
- **Get models** opens on the qualified LocalBase catalog (experimental
  runtimes left out), grouped by task with those that fit first. Each row:
  task, download size and fit in words (fits this machine, tight fit,
  needs more memory), and one action: **Get** (the existing recipe
  install, which answers "Installing…" in the row), **Installed**, or
  **Why?** for a model that needs more memory. Hugging Face search stays at
  the top; a search shows its results instead of the catalog. Import and
  Backends are a short list at the foot.
- **Empty states** hold the one action (Get a model; Show all).
- **No numbers beyond sizes:** no downloads, scores or speeds in these
  lists.
- **Checks:** `TestLibraryModelsFirst`, `TestLibraryEmpty`,
  `TestTaskFilter`, `TestGetModelsCatalog`, `TestRecipeCompact`.
  `tools/phone-check.sh` now also fails on text cut off inside a control
  (segmented items, tab labels, chips, pills, buttons, nav and list rows)
  and on a control that sticks out of its group (a segmented switch, the
  docked composer, the tab bar), not only on page-level overflow.

### 22.9 Running now (PR 4)

The one thing people come to do: see what LocalBase is using right now
and whether it is busy.
- **Memory** first: "X free of Y" and the shared bar (other apps, models,
  available) with its legend.
- **In memory**: the loaded model as one row (size, waiting requests) with
  one state pill: Idle, Busy · N requests, or Finishing (draining before
  another model loads); "A model is starting…" while one starts; an empty
  state otherwise.
- **Unload** (with models:manage; §23): one button on the loaded row.
  While the gateway drains it says "Unloading… (waiting for N requests to
  finish)"; then the page shows and announces the outcome once: "Unloaded.
  Freed X GB.", still busy (it stayed loaded), already unloaded, or that
  this LocalBase cannot unload yet. Polls are held off while an unload is
  in flight (`hx-sync`).
- **Quiet polling.** The region refreshes every 3 s (not while hidden), and
  carries one sentence (`data-announce`). `shell.js` writes it into a
  status region outside the swapped part only when it changes, so screen
  readers hear "… is busy." once, not every refresh.
- **Checks:** `TestRunningNow`; `tools/chat-runs-check.sh` checks two
  unchanged polls announce nothing, the loaded model shows Idle, and a
  request announces "… is busy." once.

### 22.10 Lab (PR 5)

The one thing people come to do: make something (an image, a video,
speech, a transcript) and get it back.
- **Layout.** One four-way switch (Image, Video, Speech, Transcribe; items
  share the row by label width so all four fit at 360 px), then **Results**,
  then the **prompt docked** at the bottom: above the tab bar on phones,
  on the keyboard while typing, at the foot of the column on desktops.
  It is Chat's composer box: one row of option chips that scrolls sideways
  (model, size or voice, More; each opens a sheet, and `lab.js` keeps the
  chip showing its choice), then the field with its one action as a round
  button inside it. At 360×740 it takes at most a fifth of the screen.
- **Before the first result** each tool says what it does, with two or
  three examples that fill the prompt (no model names).
- **Item actions** (44 px): Save and, where the browser can share files,
  Share for images and speech; Copy for transcripts; Try again (puts the
  whole prompt back, switching tool if needed) for a run that stopped,
  failed or was lost; Remove. There is no image edit in the Lab yet, so no
  "use as input".
- **Runs.** When the gateway serves runs (§21.9), image, speech and
  transcription requests start detached runs server-side (the handlers
  build the same `/v1` bodies as before and post them to
  `/_localbase/runs/v1/…` as the person). The answer is a tray item that
  polls `GET /app/lab/runs/{id}` every 2 s (not while hidden) until the run
  ends; a completed result is collected once into the Lab's result store
  (memory, 30 min, owner-bound) with the prompt as caption and alt text.
  The tray is rebuilt from the person's runs on every visit, so results
  survive leaving the page or switching tools. Stop cancels the run;
  Remove dismisses it. A media run lost to a restart says so and asks to
  generate again. Translation and video are not run targets: translation
  calls the API directly, video keeps its job polling.
- **Without runs** the Lab calls the API directly as before, and the result
  shows above the tray.
- **Checks:** `TestLabImageRun`, `TestLabRunInterrupted`, `TestLabDock`
  (plus the existing Lab tests on the direct path);
  `tools/chat-runs-check.sh` (Lab steps with the fake as sd-server: the
  prompt sits right on the tab bar; an item appears with its caption; the
  finished image is in the tray on another tool's page after leaving the
  Lab; Stop; Remove drops item and run; while typing the prompt sits on
  the keyboard and the tab bar is hidden); `tools/phone-check.sh` (the
  four-way switch fits at 360 px).

### 22.11 My voices (PR 6)

The one thing people come to do: add a voice, hear it, and use it.
- A title with one round + (Add a voice), one line on what voices are for
  (its "How voices work" link opens a help sheet: length, privacy,
  consent), and the voices as a grouped list: a round **Play**
  button (`aria-pressed`; one recording plays at a time, `voices.js`), the
  name, length and date, and "⋯" for an options sheet: Use in the Lab,
  Rename, the speech API id, Delete (confirmed).
- **Add a voice** is a sheet with name, recording (upload or record, as
  before), the consent statement and one button; it closes once the voice
  is added (`data-close-sheet-on-success`, a shell behaviour) and stays
  open, with the error, when it is not.
- Without models:manage the page lists names only (no playback, add or
  options), as the API allows.
- **Checks:** `TestVoicesSheets` (and `TestVoicesPage`);
  `tools/chat-runs-check.sh` with `LBGO_VOICE_FILE` adds a voice through
  the sheet (it closes, the list says "Added …") and plays one;
  `tools/phone-check.sh` types in the Add sheet with the keyboard open.

### 22.12 Settings (PR 7)

The one thing people come to do: change a preference and leave.
iOS-style grouped lists, one setting per row, no cards:
- **Chat:** "Save text history on this device" as a switch (a checkbox
  drawn as a switch, so its role and state stay real; the off track meets
  3:1 non-text contrast), Clear all history, and a row to Chat's
  generation controls.
- **Default chat model:** "New chats use …" with the factual reason when it
  is a fallback ("Using X: Y needs more free memory than is available."),
  then the ordered choices, which save as soon as one changes (no Save
  button to lose under the tab bar), and one sentence on the order: first
  choice, else the next that fits in the memory free now.
- **Hugging Face:** the token row with its state, the form under it.
- **Account:** who is signed in and what they can do, then Sign out.
- **Checks:** `TestSettingsGrouped` and the existing settings tests.

### 22.13 Admin (PR 8)

The one thing people come to do: give an app or a person access, or take
it away.
- One title and a segmented switch (People, Access, API keys; short labels
  so three fit at 360 px), always shown so the section is marked.
- **API keys** follow the dashboard pattern: "Create API key" opens a sheet
  (name, expiry, scopes); keys are rows in one list with their status, and
  Rotate and Revoke keep their confirmations; the new secret is shown once,
  as before. The empty state says what keys are for.
- People and Access keep their forms (roles, invitations, providers,
  policy) inside the new shell; restyling them into grouped lists is the
  next polish.
- **Checks:** `TestAdminKeysSheet` and the existing admin tests.

### 22.14 Consistency pass (operator review, 2026-10-27)

Across the eight pages:
- **Titles:** one `h1.page-title` per page (`TestPagesConsistent`).
- **Groups:** grouped lists for rows (`.list-group`, `.list-row`,
  `.setting-row`, `.model-row`); forms that need a box use one titled group
  (`.group-card`, `.group-title`); Admin's People and Access use them, and
  Admin keeps no Refresh (create, rotate and revoke redraw the list).
- **Sheets:** every sheet has the same header (title left, Done/close
  right, no rule); Chat's picker and generation sheets were aligned.
- **Empty states:** `.empty-state` (dashed box, one sentence, at most one
  action), including Chat's "no model" and "can't chat".
- **Outcomes:** `.notice` / `.notice-error` (tinted, bold), the same on
  Running, My voices and Admin.
- **Destructive actions:** the shared `confirm` disclosure (a danger
  button that reveals what will happen and a second button to do it):
  Rotate, Revoke, Delete voice, Clear all history. Clear all history also
  removes this tab's session copy of chats and the unsent message.
- **Dark mode:** every page and sheet was captured at 390 px in dark
  (`.design/screens/dark/`); the selected segment carries a ring.
- **iOS Safari** (`tools/ios-check.sh` on a dedicated simulator, booted
  for the run and deleted after): the composer moves above the keyboard
  (y 614 → 341 in a 874-point web view), Return adds a line and Send
  submits. Apple's audit flagged the tab labels (12 px text taken as the
  hit area) and two 14 px lines in the Lab: the tab links now carry their
  name (`aria-label`, the small visible label is `aria-hidden`), and the
  Lab lines are 16 px or part of the heading. One finding with no element
  or frame remains on shell pages (not identified by the audit); the skip
  link is now full size and parked off screen until focused.
- **Unload, after review:** the row's Unload opens a confirmation sheet
  ("This frees about X GiB. Anything running on it finishes first; new
  requests for it wait until it is gone, then load it again."); polls
  pause while a sheet in the polled region is open. A busy model answers
  "It's busy with N requests. Try again when they finish." A request or
  run refused because its model was unloading (503 / failed
  `model_unloading`) says "The model was being unloaded, so this didn't
  run. Try again: it loads again when needed." in Chat and the Lab.

### 22.15 Studio (PR 9, operator direction 2026-11-01)

"Lab" was everything on one page. It becomes **Studio**: a landing page and
one page per tool, each to follow the best apps in its category. "Studio",
not "Create", because transcribing and deciding are not creating.
- **Routes.** `/app/studio` is the landing; `/app/studio/{image,video,speech,
  transcribe}` are the tools (Decisions joins in its own PR). `GET /app/lab`
  and `?tab=` redirect. The form and polling routes under `/app/lab/` stay.
- **Landing.** Recent (the person's four latest results across tools, or a
  line saying what will appear), then one card per tool: a small drawing of
  what it makes (built from theme colors; a picture tool shows its latest
  picture), one line of purpose, and the caption of its last result. A tool
  with no model says "Needs a model" and the whole card links to Get models
  filtered to that task. A tool the account may not use says so and is not
  a link. The caption is one line with a relative time.
- **Expired results.** A generated picture carries its fallback: if the file
  is gone or will not decode, `shell.js` shows the tool's drawing and
  "Expired", never a broken-image icon (Save and Share hide with it).
- **Tool pages.** A back link and the tool's name replace the four-way
  switch. The tray holds that tool's results only. Without a model or
  permission the page shows only that state, not a prompt.
- **Waiting.** An active result is a `.skeleton` of its own shape with its
  state in words; the shimmer stops with reduced motion.
- **Checks:** `TestStudioLanding`, `TestLabRedirects`, `TestLabImageRun`
  (tray per tool, Recent), `TestResultImageFallback`,
  `tools/chat-runs-check.sh` (a real PNG decodes; a missing file falls back),
  `phone-check`, `axe`. `phone-check` now seeds a result with a long prompt,
  so pages are checked with content in them, and fails when a card or list
  leaves the 16 px gutter at 360, 390 or 430 px; page overflow alone had
  missed this (cards were 1000 px wide once a long caption was present).
  The fake backend's images are now real PNGs.
- **Known limits of the backend, for the tool pages:** video runs only at
  each model's fixed profile (no duration or aspect presets; more qualified
  profiles are a product decision). Speech has its own list (§22.19).

### 22.16 Image studio (PR 10)

The one thing people come to do: describe a picture and get it back.
- **Gallery first.** Each generation is a group: its pictures (one takes the
  row, a batch shares it, in the chosen shape) and a line with when and the
  prompt. While it is made, tiles of the final shape and the state in words;
  only a just-started group shimmers (about 5 s at most, then still), so
  polling does not replay it.
- **The dock** is the shared composer. Chips show their values: Shape (with
  Standard or Large resolution), Look, Number (1, 2 or 4), Model only when
  there is a choice, and More (Avoid, Seed, Steps).
- **Looks** are six named sets of words added to the prompt and negative
  prompt; the sheet says so, and results depend on the model. A result's
  caption stays the person's own prompt, and its details name the look. A real
  pass (2026-11-01, DreamShaper 8, 4 prompts by each look) kept Photo,
  Illustration, Watercolor and Cinematic. 3D render looked like Plain and Line
  art stayed photographic, so both have new words. Line art still lets strong
  colors through (oranges stayed orange), and a dark subject can invert to white
  on black: a limit of the words, not a promise of pure line work.
- **A batch is one run with `n`** (§24): separate runs would meet the limit
  of four active runs. The pictures arrive together, and Stop stops the batch;
  the Number sheet says more means a longer wait. Measured on that pass, each
  extra picture adds its full time and no memory: 14 s each at 512x512 (1, 2 and
  4 took 14, 29 and 57 s), 22 s at 512x768, 50 s at 768x1024, 72 s at 1024x1024,
  with the server near 2.1 GiB throughout. These are one computer's times and
  stay out of the product's copy. Sizes came back exactly as asked.
- **Seeds.** The page always picks the seed (no seed means the same seed every
  time at the gateway), leaving room below the API's limit for a batch, and
  reads each picture's seed from `data[i].seed`, else base + i. On real weights
  `data[i].seed` matched what was sent, the same seed gave identical bytes, a
  batch's second picture equalled seed + 1 alone, and through the page's own
  routes Regenerate returned identical bytes and Vary a new seed and picture.
- **The viewer** is a full-screen modal `<dialog>` per picture, always dark,
  with Previous and Next (buttons and arrow keys; swipe is not needed) and
  "2 of 6". Its bar: Details, Save, Share where files can be shared, **Vary**
  (same prompt, new seed; not "more like this", as nothing makes it similar)
  and **Regenerate** (same prompt, same seed). Details holds the prompt, size,
  look, seed, the model's name (the only place it appears), Copy prompt, Reuse
  settings, the note "A saved copy includes its prompt", and Delete with the
  shared confirmation. Delete takes one picture of a batch; the last one takes
  the run with it.
- **Share leaves the prompt out; Save keeps the file.** A generated PNG holds
  its prompt and settings in text chunks. Share draws it to a canvas and encodes
  a PNG again, which is lossless and keeps its size, so those chunks are gone;
  the viewer says "Shared without the prompt", as a page cannot write in the
  system's share sheet. Save hands over the original bytes. If encoding fails,
  Share goes away and the viewer says to use Save; where files cannot be shared
  at all, Details says so.
- **Not built:** "Use as input" needs an edit flow and an edit-capable model
  (the API has `/v1/images/edits` as a run target); it is a follow-up.
- **Checks:** `TestImageBatch`, `TestImageAgain`, `TestImageDelete`,
  `TestLabImageRun`, `TestLabDock`; `tools/chat-runs-check.sh` now opens the
  viewer and checks it fills the screen with its actions and 44 px targets (a
  closed dialog had hidden a stylesheet mistake from every other check) and
  moves between pictures, and walks Share's three paths: without the prompt, a
  failed encode, and no file sharing. Measured facts about `n`, seeds and limits
  are §24.

### 22.17 Decisions (PR 11)

A playground for typed questions: one page (`/app/studio/decisions`) with a card
on the Studio landing. The owner asked for "Jev-style decision AIs" to have
their own place. It asks `POST /v1/systemone`.

- **The form** keeps one fixed control at the top, the type: Pick one (`choice`),
  Yes or no (`noul`), Rate (`score`). Below it the situation and the options,
  and in the dock the Template chip, the Batch chip and the question as a real
  input, with one round action. After sending, the form becomes a summary with
  Edit, so the answer is on screen, and the type has not moved.
- **Templates** fill the form and never run. "Save example" keeps the form in
  this browser only (`localStorage`, at most 50), and the same sheet lists them.
- **The read comes first, in words,** then the answer with its odds, then bars:
  one accent for the top answer and one neutral, because answers are not good or
  bad. Percentages are the answer here, not a performance claim, so they appear
  next to the words. A scale keeps its own order; the rest go high to low.
- **The words** come from one table, `BANDS` in `decision-read.js`, keyed by
  model so another model can have its own row. With gap = top minus second:
  Unsure (top < 0.50), Strongly favored (gap ≥ 0.70), Close call (gap < 0.25),
  otherwise Leaning. A close call names both answers.
- **These cuts are provisional and belong to one model,** Clef Flash, 93 questions
  on 2026-11-01 (`docs/decision-calibration-2026-11-01.json`, from
  `tools/decision-calibrate.ts`, which runs a real model and is not in CI). On
  48 labelled questions all 48 were right; on 28 hard ones 24; 17 were judgment
  calls with no right answer. High odds did not mean right, and a judgment call
  could still come back at 0.989, so every result carries the same line: "These
  are the model’s odds, not a check that it is right. Judgment calls can still
  come back lopsided." The API snippets do not repeat it. No word claims accuracy,
  and the sample is too small to promise a rate for any band.
- **Runs.** A question is a detached run (§21): it survives a reload, and the
  pill leads back here. The page deletes the run once it has the answer, so a
  question stays in the browser (`sessionStorage` for this tab). Stop cancels it
  and returns to the form with its text. Starting a template stops what is
  running, and Send is off meanwhile.
- **Batch** asks one question of many situations: one per line, pasted or from a
  text or CSV file, at most 200. Rows run one at a time from the browser, and a
  row is redrawn only when it changes. Rows belong to the form as sent, so editing
  it cannot change later rows. Table on wide screens, cards on phones, with the
  word as a column and the filters All, Close calls and unsure, Failed. Stop keeps
  finished rows and offers Continue; Retry failed; Export CSV, where a cell that
  could run as a formula is prefixed. A row shows the first 240 characters (a
  200 KB row stopped the page from painting) and the export has all of it. The
  batch says "Keep this page open; reloading pauses it": a reload offers
  Continue, and a boosted navigation does not stop it. A lost connection pauses
  it too, rather than failing every row left in an instant, which is what the
  real pass found. Running a batch as gateway-side runs is a follow-up; the
  `/v1/systemone` run target exists (§24).
- **View code** gives cURL and JavaScript for the current form with the
  `$LOCALBASE_API_KEY` placeholder. A real key is never in the page.
- **Not built:** image and audio inputs, multiple questions in one request, and
  comparing against an expected answer. Follow-ups if asked for.
- **Checks:** `TestDecisionsPage`, `tools/jstest/decisions.test.ts` (band edges,
  a model with its own row, each type, requests, code, CSV), and
  `tools/chat-runs-check.sh` walks templates, every word, Stop, a reload, saved
  examples and a batch against the fake backend, whose state text (`fake:close`,
  `fake:unsure`, `fake:leaning`, `fake:slow`) reaches each word.
- **The form is not boosted.** `<body hx-boost>` also takes forms, and with it on
  a validation message was wiped right after it appeared (found by the real
  pass; it stayed once the boost was off). The form says `hx-boost="false"`, and Send starts
  disabled until the script is ready, so a form that needs the script cannot
  submit natively.
- **Real pass (2026-11-02, Clef Flash, scratch gateway, through this page):** 32
  questions of all three types, including all 17 ambiguous ones. Each word was
  recomputed from the raw answer and every one matched, with the standing line
  present. 21 read Strongly favored, 6 Leaning, 4 Close call, 1 Unsure. A coin
  flip was a Close call (51/49), "xqzv plm" Unsure (29%), and a judgment call
  still came back at 99% (bring an umbrella at 50% rain), which is what the
  standing line is for. The thresholds stay, and no second line depends on the
  question's wording; "About these odds" under each answer shows this case. A greeting routed to billing at 61% read as Leaning. A
  question took about 0.2 s once loaded (7 s cold), 30 rows 8 to 10 s, 200 rows
  62 s; the table stayed responsive at 200 rows and 201 lines were refused. A
  row too long for the model failed alone, with the gateway's own sentence, and
  still failed on Retry; the rest went on. One computer's times, not product
  copy. The 7 s includes the model's load. The load also caused a pressure blip on
  the same computer, and the live gateway evicted its own idle copy of the model,
  so later real passes check the live gateway's `/health` first.

### 22.18 Video studio (PR 12)

Text to video, or a picture to animate where the model takes one, at the model's
fixed profile. No length or shape presets: each model has one profile, a product
decision recorded for the owner (§24), so the sheet says it in words ("About 5
seconds, tall. It can take up to 10 minutes and uses a lot of memory while it
runs."). The 10 minutes is the model's limit, not a measurement.

- **A job is a tray item,** on one pattern with the other tools. Its place has
  the video's shape and says its stage in words: "Waiting for its turn…", "Making
  the video…", with "You can leave this page; it keeps going." The API reports no
  steps, so there is no percentage. Past half the model's limit it says "This is
  taking a while" beside Stop. While it runs the place is short enough that Stop
  stays above the prompt on a phone.
- **The API has no job list,** so the page keeps a note per job in memory, bound
  to its owner, and rebuilds the tray from them. A job the API has forgotten leaves
  the tray. A gateway restart clears the notes, and with them the tray; the jobs do
  not outlive the gateway either. Finished jobs last 15 minutes (the job manager's), so this page's
  header says 15 and the landing says "for a while".
- **Jobs are not runs (§21),** so the shell asks `GET /app/lab/video/active` too:
  the pill leads back from other pages, and is off on this one. Polled pages would
  ask constantly, so it asks at most every 10 s while a job runs, every 60 s when
  none does, and not while the page is hidden. A job started in another browser
  can take up to a minute to show.
- **One job at a time** is the API's rule. A second says "One video at a time. Stop
  the one that’s running, or send this when it finishes." and keeps the words.
- **Failures** are in plain words with a next step: not enough memory links to
  what is running, and leaves out the API's byte figures. Verified against the real
  gateway (2026-11-02, FastWan): its refusal, at an estimate of about 16 GiB plus a
  9.6 GiB reserve, reached the tray with these words within 2 seconds. Stop says "Stopped.
  Nothing was saved." Try again repeats the words, model and starting picture; the
  picture lives in the result store, so if it has expired it says so.
- **Starting picture:** the "Start from" chip shows only for a model that takes
  one. A file, or one of the last six generated pictures; it is cropped to fill
  the frame, previewed, and can be removed.
- **Not built:** a thumbnail of a finished video on the landing.
- **Checks:** `TestLabVideo`, and `tools/chat-runs-check.sh` walks stage, shape, Stop
  on the first screen, the busy message, the pill, leaving and returning, playback
  of a real file, a failure, Try again, a recent picture and Remove. Its scratch
  root needs sparse model files marked verified and an `LOCALBASE_FFMPEG` that
  writes a real MP4 the test browser can decode (VP9: the test Chromium has no
  H.264). No real video model has run through this page yet.

### 22.19 Speech studio (PR 13)

A script of up to 5,000 characters, a voice, and a joined file. There is no speed
control, because the API takes only speed 1 (§24).

- **A script is a job on the gateway's UI side,** not in the browser: split, made
  one part at a time as the signed-in person, joined, and put in the result store.
  A reload or a suspended phone loses nothing, and there is no client state to
  restore. One script runs at a time per person, said plainly.
- **Limits.** Parts call `/v1/audio/speech` directly rather than as detached runs, so
  they do not count against the run limits (§21). That is safe because a script is
  sequential: it holds at most one speech request at a time, and each part goes
  through the API's own admission. What stops one person holding the speech model
  for long is the 5,000 character limit, refused before anything runs, the one
  script per person, and a 30 minute limit on the job.
- **A gateway restart loses:** a script still running, including parts already made,
  since nothing is on disk until it is joined; every note, so all scripts leave the
  tray; and finished files, which live in memory only. It does not lose the script's
  text anywhere else, because there is no other copy: the person writes it again.
- **Splitting** (`splitScript`): at most 200 characters a part (the API takes 256)
  and an estimated 200 frames (`partFrames`; the runtime's cap is 256). At sentence
  ends, then at a comma, then a space, and mid-word only for a run with no space.
  A list of numbers comes to about 85 characters a part, from §24's measurements: a
  112-character list ran 202 to 256 frames, 3 of 8 hit the cap, and it needed up to
  about 2.8 frames a character. That is a small sample and the model's length varies,
  so a cut is less likely but still possible, and the cut flag is what keeps this
  honest. A fixed seed does not make speech repeatable (§24), so none is offered.
- **Joining:** the sample format must match. Each part's own silence is trimmed at
  joined edges, since the model left anything from 0.16 s to 1.6 s and pacing came
  out uneven, and one pause goes between (`speechPartPause`, 350 ms, the one place it
  is set: about the middle of the model's own gaps between sentences, checked by
  analysis and not by ear). The last 10 ms before a pause fades, so an edge the
  model cut does not click.
- **Progress is real here:** "Making part 3 of 8…" with a `<progress>`, because the
  parts are real steps. The page's count ("About 8 parts") packs sentences the same
  way but is only a count.
- **Digits:** a script of mostly digits (at least 12, and more than half of what is not
  a space) shows "Long runs of digits can come out cut off or repeated." under the
  count, since numbers measured unpredictable on the real model.
- **Nothing is cut silently.** The field has no effective `maxlength`, since that
  truncates a paste without saying so; past the limit the count turns red and the
  send says so and keeps the script. If the API flags a part as cut
  (`X-Localbase-Audio-Truncated`, §24), the result says "The model cut part of this
  short, so some words may be missing." Stop and a failure keep what was made and
  say which part they reached.
- **Voices:** a sheet with the person's own first, "Mine". Choosing (a radio) and
  hearing (a button) are separate controls, and one sample plays at a time. A sample
  is one short sentence made on first play and cached per model and voice, which
  costs compute once, so the sheet says so. "Add a voice" opens the same form as My
  voices, for models that clone from a recording and people who manage models; the
  new voice is chosen on return.
- **The player** is the native `<audio controls>`. Above it, a waveform drawn by the
  server from the real samples as SVG (the CSP allows no inline styles), filled as
  it plays. Tapping moves the player; it is hidden from assistive technology and the
  keyboard, since the native control does the same job.
- **The pill** covers scripts with the same capped request as video jobs
  (`GET /app/lab/active`).
- **Real pass (2026-11-02, Qwen3-TTS, scratch gateway, through this page):** nobody
  has listened; this is analysis of the files. 1,532 characters made 11 parts and 97 s
  of audio in about 37 s once loaded (57 s from cold), with the server near 3.3 GiB. The page's count matched the parts on both scripts. Joins are an
  even 350 to 388 ms, with quiet edges either side and DC near zero. A loud click
  (one sample step of 42,410, reaching full scale) sits 217 ms after a join and
  inside the model's own audio, and the first run had one too, so it is not the join;
  the cause is not established. A 256-character list of numbers through the API came
  back flagged at 20.48 s, and through the page the flag reached the words. A sample
  took about 1 s, then nothing once cached. Stop took about 2 s, kept the two parts
  made, and left the model loaded: it still held its memory 15 s later, and the
  gateway reported it running. A custom voice, made from the model's shipped reference,
  spoke through the picker. Files are in `.design/research/studios/speech-real/`.
- **Known limits of the backend:**
  - **Speech clicks:** Qwen3-TTS output sometimes has a short burst of noise, a click of a
    few milliseconds. This is an upstream `tts-server`/vocoder artifact, not the gateway or
    the page: the gateway's audio was byte-identical to `tts-server`'s in 73 of 73 requests.
    In ordinary prose, about 1 request in 9 had a burst. There is no fix on our side. Whether
    they're audible hasn't been checked by ear. Details: §24.
  - **Numbers:** a list of numbers can be cut or repeated, as above. The cap stays at 256
    frames; the 85-character split and `audio_truncated` hold either way.
- **Not built:** a chat speech tool reading the cut flag (not this page's), MP3, and
  choosing a pause length.
- **Checks:** `TestSplitScript`, `TestJoinWAV`, `TestShapeEdges`, `TestWavePeaks`, `TestSpeechJob` (parts,
  one at a time, the cut flag, Stop, a failure part way, over the limit) and
  `tools/chat-runs-check.sh` with `LBGO_VOICE_FILE`. The fake's `fake:slow` slows each
  part, and `fake:cut` runs a part to the cap.

### 22.20 Transcribe studio (PR 14)

Record or choose a file, and get the words with their times.

- **Two equal starts,** Record and Choose a file (and dropping a file on a wide
  screen). Either one starts by itself: there is nothing to confirm once you have
  stopped or chosen. Language and Translate live in one sheet, on a chip that says
  "Detect language". Every transcription the web UI makes says a language: this page and
  chat's dictation send `language=auto` unless the person chose one, because with no
  language the runtime takes the speech as English (a real Spanish clip came back as
  confident, wrong English). `/v1` keeps its default (§24). The page shows `language`,
  never `detected_language`, which means nothing on English-only models. The browser converts to 16 kHz WAV, as before; the limit
  works out at about 13 minutes, which recording stops at and the page says.
- **A recording is hard to lose.** Sending starts at once, so what was sent stays in
  memory until it has become a run. A send that fails or is refused says "Your recording
  is still here, send again.", with Send again and Discard, and that survives moving
  around the app, since the script stays loaded for as long as the page lives. A full
  reload ends that memory, so the browser asks first while a recording or an unsent
  send exists.
- **Recording** is a panel in the dock: time, a level bar with a calm fall, one word
  ("Hearing you", "Very quiet" after 3 s, "Too loud"), Pause and Stop. The level is
  reassurance, not a measurement. "You'll get the text after you stop", since nothing
  streams. While the browser's permission question is up, the page says "Allow the
  microphone to start recording." A boosted link waits while recording and says why; a full navigation gets
  the browser's own question. A refused microphone says how to fix it, and a file
  still works.
- **A transcript is a detached run (§21)** asking `verbose_json`, so it survives
  leaving. A translation is a run too, now that `/v1/audio/translations` is a target,
  and is labelled Translation. The audio stays in the result store to check the words
  against, for the same 30 minutes.
- **Times** are buttons at the start of each stretch, full-size targets in their own
  column, named with what they show ("1:05, play from 1 minute 5 seconds"), that move
  the player, which stays in reach while the words
  scroll. Times hides them. A transcript without times shows plain text and offers
  only text, as subtitles would be invented.
- **Export:** Text, Subtitles (.srt) and Web subtitles (.vtt), named by what they are
  for, built by the server from the same stretches. Copy gives the plain words.
- **Real pass (2026-11-02, whisper-base-q8_0, scratch gateway, through this page):** files
  only, no microphone. A 97 s clip of real speech (the Speech pass's story, known script)
  came back at 4.2% word errors against the script, and the same clip three times over
  (4 min 51 s) at 4.0%. Those errors mix the speech model's reading and the transcription.
  Times held: a phrase repeating every 96.97 s landed within 0.4 s each time. SRT and VTT
  from every clip passed a parser (format, numbering, order, no empty text). Direct API
  time was 0.7 s and 1.9 s, with the server near 0.3 GiB. A Spanish clip made with macOS's
  `say` was detected as Spanish once Detect sent `auto`. Translating it reported Spanish
  but gave poor English ("The door was locked this morning" for "El puerto estaba
  tranquilo"), so every translation says "Check a translation before relying on it." An
  English-only model takes `auto` safely: it stays English, and marked a Spanish clip
  "(speaking in foreign language)". On such a model the page hides Language and Translate
  and says "This model hears English only." The signal is the catalog's `multilingual`
  mark, else the model's name (as the Hub listing does), so an oddly named import can be
  misread; the runtime knows from the file (`capabilities.go`) but does not tell the page.
- **Not built:** word-level timing (`segments[].words[]` is optional), search in a
  long transcript. Chat's dictation is not reused: it records up to 60 seconds and adds the
  text to a draft in one step, with no panel, no Pause and no times, so the only code to
  share would be the WAV encoding.
- **Checks:** `TestSubtitleFormats`, `TestSegmentWords`, `TestTranscriptExport` and
  `tools/chat-runs-check.sh` with `LBGO_VOICE_FILE`. The microphone there is a generated
  tone through the real recorder, level and sending, because the headless browser's
  own never answers, so a real microphone has not been tried. The fake's whisper
  answers a fixed transcript. Very quiet, Too loud and the 13 minute stop have not been
  driven. No real transcription model has run through this page yet.

### 22.21 Audit and performance (PR 15)

**Performance.** The `web-perf` skill's Chrome DevTools server is not set up here, so the
same measures came from the browser's debug protocol: 390x844 at 3x, CPU 4x slower, 1.6 Mbps
down, 750 kbps up, 150 ms latency, cold cache, median of 3, on a scratch gateway with fake
backends. Compressing text assets (gzip, once per asset, with `Vary`) was the one change
worth making, as assets were already immutable and content-addressed. The tray was empty or
short, so results add to these. KB is transferred, and `/app/studio` also counts the page its
test tap opens.

| Page | KB | First paint, ms | Largest paint, ms | CLS | Tap delay, ms | Blocking, ms |
|---|---|---|---|---|---|---|
| `/app/studio` | 207 → 90 | 1169 → 549 | 1169 → 549 | 0 | 24 | 0 |
| `/app/studio/image` | 191 → 74 | 1281 → 577 | 1281 → 577 | 0 | 32 | 0 |
| `/app/studio/decisions` | 223 → 81 | 1412 → 561 | 1478 → 916 | 0.01 | 16 | 0 |
| `/app/studio/speech` | 204 → 84 | 1308 → 556 | 1308 → 556 | 0 | 32 | 0 |
| `/app/studio/transcribe` | 200 → 77 | 1329 → 578 | 1329 → 578 | 0 | 32 | 0 |
| `/app/studio/video` | 184 → 66 | 1260 → 575 | 1260 → 575 | 0 | 32 | 0 |
| `/app/chat` | 349 → 160 | 1416 → 579 | 1416 → 579 | 0 | not measured | 0 |
| `/app/models` | 168 → 61 | 1174 → 537 | 1174 → 537 | 0 | not measured | 0 |

Web Vitals' good lines are 2,500 ms, 0.1 and 200 ms. Tap delay is one scripted tap, not a
person's use over time, so it is only a floor for INP. The server here is on loopback, so a
real connection adds to the first response. The public hostname's tunnel was not measured.

**Audit.** Every existing page was shot from #393 and from this stack
(`.design/screens/before-after/`): nothing regressed, and the pages' copy now says Studio.
Added here: press feedback on buttons, chips and rows (dimming always, a small scale only
without reduced motion), and "Expired" where a player outlives its file. iOS Safari
(`tools/ios-check.sh`, iOS 27 simulator) passes the composer test on the new build, and its
accessibility audit now covers every studio. It still flags Dynamic Type as partly unsupported
on every page, plain text as small hit areas, and an unnamed element it cannot locate.

**What has not been checked, across the studios:**

- **Microphone:** no real microphone has recorded through Transcribe. The test browser's own
  capture hangs, so a generated tone went through the real recorder, level and sending. The
  browser's permission question, background and lock behaviour, "Very quiet", "Too loud" and
  the 13 minute stop have not been driven. iOS Safari's recording format is unverified.
- **Real phones and people:** no real device, no assistive technology (VoiceOver, TalkBack), no
  usability testing. axe and Apple's audit are automated and catch only part of this. Contrast
  is axe's reading only, and has not been checked by hand on a phone in daylight.
- **Video:** no real model has run through the page. A pass needs 25.6 GiB available and has
  not had it; the one attempt was refused before loading. So "a few minutes" and "a lot of
  memory" are unmeasured here, the stage words are unchecked against the runtime, Stop's effect
  on memory is unknown, and real H.264 has never played in the page (the test browser has no
  H.264, so tests use VP9). Only the refusal message is verified on the real gateway.
- **Speech:** checked by analysis, never by ear. Numbers and digit strings can be cut or repeated.
  Short clicks from the model are possible (§22.19). The 350 ms pause is untested on people.
  One voice was tested at length.
- **Transcribe:** files only, on one small model. The clips were one speech model's voice and one
  macOS voice, so real people, accents, noise and long files near the limit are untested. A
  translation can be plainly wrong, and the page says to check it. The English-only signal goes by
  name for models outside the catalog.
- **Decisions:** one model. The bands are provisional, from 93 questions. No one has read results
  and judged the words (the brief's H4).
- **Image:** the looks were judged by one reviewer on 4 prompts, with DreamShaper 8 standing in for
  the LCM model it lacks. A real phone's share sheet has not been tried; Share's paths ran with
  a stubbed share call. The "Shared without the prompt" text is in the viewer, not the sheet.
- **Across the pages:** a gateway restart loses video jobs and scripts in progress, and all results
  in memory. Phone adaptations in the research brief are proposals, not tested patterns, and its
  competitor evidence is mostly docs and published screenshots.

## 23. Explicit unload (2026-10-27)

Operator request, for the FE's Running page (#388). There was no way to
free a model's memory from the UI: freeing RAM for a bigger model on the
live Mac meant restarting the gateway.

**`POST /_localbase/models/{id}/unload`** needs `models:manage`.
- **Browser:** mirrored under `/app/api` with the same same-origin,
  `X-Localbase-Ui` and CSRF rules as the other UI mutations. Forwarding
  keeps only `wait`.
- **Scope:** acts on whichever runtime has `{id}` loaded: llm, stt, tts,
  image or video.
- **`?wait=true`** (the default) drains for up to 30 s. **`?wait=false`**
  refuses at once if anything is in flight.

**The drain** works like an eviction.
1. Requests already holding the model finish. They are not cancelled.
2. Requests for the model that are queued but not started are refused,
   as is any new request for it until the drain ends. The refusal is 503
   `model_unloading` with `Retry-After: 2`; a retry loads the model again.
3. Other models' queued requests wait, as they would behind any switch.
4. When the last lease is released, the backend stops. The admission
   ledger drops its reservation, and `runtime.stopped` is logged with
   reason `unload`, next to memory, switch, shutdown, crash, cancel and
   idle, including `memory_freed_bytes`.

**In flight** means held leases. A detached run (§21) is a request, so a
run generating on the model counts, whichever principal owns it, and is
drained, never cancelled. A queued run is refused like any queued request
and ends `failed` with `model_unloading`.

**Answers:**

| Case | Answer |
| --- | --- |
| Unloaded | **200** `{"id","object":"localbase.model.unload","unloaded":true,"runtime":"llm"\|…,"memoryFreedBytes","drainedMs"}` |
| Busy, `wait=false`; or still busy after 30 s; or a switch is under way | **409** `model_busy` with `error.in_flight`. The model keeps running and its work is undisturbed |
| Known model, not loaded | **404** `model_not_loaded` |
| Unknown model | **404** `model_not_found` |
| No `models:manage` | **403**; no credentials: **401** |
| `wait` not true/false | **400** |

**Unchanged:** auto-eviction, idle recovery, pressure handling and the
default-model logic. An unloaded model reloads on demand as usual.
`memory.Controller.Reserved()` exposes the admission ledger, per runtime,
for tests and a future Running page.

**Tests (`internal/gateway/unload_test.go`):**
- `TestUnloadIdle`: the ledger drops the reservation, `memoryFreedBytes`
  equals it, and the stop event carries reason `unload`; the model then
  reloads on demand.
- `TestUnloadBusyDrains`: a detached run completes in full, and requests
  queued before the unload or sent mid-drain are refused at once.
  Removing the mid-drain refusal fails this test.
- `TestUnloadBusyRefused`: `wait=false`, and the bounded wait, both give
  409 with `in_flight` 1, with the run undisturbed.
- `TestUnloadNotLoaded`
- `TestUnloadScope`: a key with only `models:read` gets 403; another
  principal's run counts as in flight.
- `TestUnloadBrowserRoute`
- `TestUnloadImageRuntime`: a media runtime unloads the same way.

## 25. UI v2: Hearth (2026-11-02)

The owner's bar: "The UI is still trash and needs real work." What shipped was sound and generic. v2 keeps the structure and gives it an identity. The critique, the three directions and the independent review are in `.design/screens/v2-before/` and `.design/mockups/v2/`.

### 25.1 The identity

**Hearth, revised:** warm, plain and readable, with one signature: **the host-and-memory strip**, naming the computer doing the work, its state in words, and a ruled gauge of its real memory. Only a local product can show that. On a phone it matters most, because the phone isn't the computer doing the work.

- **Color:** cool warm paper, ink for action, and one lamp amber that means "live on your machine". Dark is a lit window at night. Reviewed as too close to Claude's look in its first form, so rust, drop caps, italics and engraved vignettes went.
- **Type:** Newsreader for titles and a model's words, Figtree for everything you operate. Upright only. Both SIL OFL, self-hosted, about 78 KB.
- **Gauge:** 48 places. Tall lamp blocks are the model in memory, short blocks everything else, and rules free space. They differ by shape, because the two fills are close in lightness.
- **Mark:** a doorway with a light inside, replacing a ⌘ nobody chose. Files are in `static/brand/`, with favicon, app icon and manifest.

### 25.2 How it is built

- **One theme file.** `css/theme.css` holds every value a direction chooses. `TestThemeIsOneFile` fails on a color anywhere else.
- **Budgets in CI.** `TestSizeBudgets` holds gzipped HTML, CSS, scripts and fonts per page.
- **Screenshots.** `tools/ui-shots.sh` shoots every page at 360, 390, 430, 768 and 1280 in both schemes, with contact sheets, and compares with a baseline. Pictures stay out of the repository. It is not in CI, because it needs a scratch gateway.
- **Inventory.** `docs/ui-components.md` lists every component and how far it has come.

### 25.3 The strip's facts

- **Name:** `os.Hostname()` without `.local`, on the server, or `LOCALBASE_COMPUTER_NAME` for scratch gateways. A hostname often holds its owner's name and everyone signed in sees the strip, so the Go thread is adding an owner-editable name (`host.name`, `PUT /_localbase/computer-name`). Fixtures only ever use generic names.
- **Memory:** machine-wide figures from the host. The model's share is its plan, for the text model only, and the Running page's legend says "Text model" and "Everything else" until `host.memory.localbase` lands, as other runtimes would otherwise count as other apps. The strip does not call `LLM.Footprint`, which runs a process per call and is macOS-only.
- **"This needs N GB"** shows only from the planned admission preview (`GET /_localbase/models/{id}/admission`), never computed in the browser. A refusal has no figures as fields today, so until then a refusal shows the gateway's plain sentence.
- **Refresh:** every 15 s, not while hidden. A failed refresh marks it "Can’t reach this computer", in the same shape.

### 25.4 Acceptance for v2, across the PRs

From the review: first-run, Models, Running, desktop layouts, keyboard-open states, long replies, failures, enlarged text at 320 px, and a real device. Each is shown as built screens in the PR that builds it, or listed as open.

### §25 after the first review

- **The gauge draws only what is known.** Below 0.05 GiB a figure prints "<0.1 GiB" and draws no segment. Without a reading the gauge is a dashed rule and the strip says "Memory unavailable"; no figures are drawn.
- **One source.** The gauge shows the computer's own reading, in use and free, and draws no share for LocalBase, because the only figure there is covers the text model alone. Running labels that figure "Text model only, measured" or "planned". The amber share returns with `host.memory.localbase`.
- **The state comes from real state:** Ready, Text model busy, Getting the model ready, Finishing, Not enough memory, Memory is tight, Can't reach this computer. "Busy" for the whole computer waits for a `host.activity` that covers every modality. After a refusal the strip says "Not enough memory for this model" until the person moves on.
- **The poll.** `/app/strip` refreshes every 3 s from a visible tab, pauses when hidden, backs off to 60 s when it can't reach the computer, and answers 204 when nothing changed. The state word is the runtime's own state. Memory runs `vm_stat` on macOS, so one reading is shared for 15 s.
- **One strip, everywhere.** At the head of every signed-in page. Running keeps it above its legend, going nowhere, and shows a long name whole. Elsewhere a long name truncates with a tooltip, and the state wraps and is never cut. While composing it is one line.
- **Boundaries.** Controls use `--lb-edge`, and being selected is a mark at 3:1 or better, not only a tint. Decorative card edges use `--lb-bound`: 3:1 at night, faint on paper. Checked from rendered colors in both schemes by chat-runs-check.
- **Tablets get a rail** (768 to 1099 px, 4.5rem), with chats in the drawer, as on a phone. From 1100 px the sidebar is 14.5rem. `lg:` means 48rem here; `min-[1100px]:` is where a page goes wide.
- **Two ambers:** `--lb-brand` for the mark, `--lb-lamp` for live status. They start equal and may part.
- **The mark is a frame and a lit doorway with no gap,** because the gap closed up at favicon sizes and read as a padlock.

### §25 first run

`/app` goes to `/app/welcome` when no chat model is installed and the person can add one. Hello, choose, download, first chat, with no tabs, and "Not now" always leaves. Sizes and fit are the catalog's and this machine's: "Fits this computer" compares against total memory, not what's free now, until the admission preview lands. Phones are stated plainly on the hello: they need a sign-in provider, because the first link only works in a browser on this computer. A finished download is enabled then activated, and Chat gets a real page load (`HX-Redirect`), because a swapped-in body would not start its scripts. There is no Cancel, because the management API has none.

Running refreshes every 10 s with a Refresh button: it is a status page, and reading memory spawns a process on a Mac.

## 24. Backend facts and small additions for the studios (2026-11-01)

Asked by the FE thread for the per-modality studios. Each behavior below
was measured on 2026-11-01 on a scratch gateway with the pinned runtimes
(`sd-server` with `dreamshaper-v8`, `tts-server` with Qwen3-TTS,
`whisper-server` with tiny.en, `llama-server` with Clef Flash), unless it
says otherwise.

**Image `n` and `seed`.** One adapter serves images, `sd-server/v1`, on
every platform.
- **`n`** (1–10) is honored. All images come back in one `/v1` response,
  so in one run `result`. A batch runs one image after another on the same
  runtime: 3 images took about 3 times as long as 1.
- **`seed`** (0–2147483647) is exact: the same seed, prompt and parameters
  give byte-identical PNGs.
- **Batches use base + i.** Image 1 of `n=2, seed=42` is byte-identical to
  `n=1, seed=43`.
- **No seed means seed 42,** by design (operator, 2026-11-01), so the same
  prompt gives the same image. This matches TS, and API clients may rely on
  it. Revisit only if the user asks. Everywhere LocalBase generates on a
  person's behalf sends a random seed when none was chosen: the chat image
  tool (`randomImageSeed` in `tools.js`, the one deliberate difference from
  TS in `tools.test.ts`), the Lab (`labImage`), and the FE studios.
- **Echo:** `data[i].seed` is a Go extension on `/v1/images/generations`
  and `/edits`. It comes from the image's own PNG `parameters` text, else
  from the requested seed + i on `sd-server/v1` (JPEG and WebP carry no
  parameters). With neither, the field is absent, not guessed.
- **Metadata:** sd-server's PNGs carry the prompt and parameters in that
  text, so a shared PNG shares its prompt.

**Speech.**
- **`speed`:** only `1` is accepted, on every runtime; other values get
  400. Nothing here implements speed.
- **Input:** 256 code points, gateway-wide, for every runtime.
- **Silent truncation on Qwen3-TTS:** `tts-server` stops at 256 frames,
  20.48 s, and answers 200 with what it has. 255 characters of prose took
  14.08 s, but 256 characters of a number list came back at exactly
  20.48 s with the end missing. The bytes are not changed, but the cut is
  visible (operator, 2026-11-01):
  - a warn event `speech.output_truncated` with `request_id`, `model_id`,
    `frames` (256) and `input_chars`, never the text;
  - the response header `X-Localbase-Audio-Truncated: true` on
    `/v1/audio/speech`;
  - `result.notes: ["audio_truncated"]` on a run object and its `end` event,
    since run results keep no headers. `notes` is absent otherwise.

  The flag means the output reached the cap exactly; text that happens to
  end on frame 256 is flagged too. It applies to `tts-server` only. CrispASR and Kokoro were not
  measured: no weights are installed here.
- **No server-side join.** Non-streamed WAVs all have a 44-byte header and
  the same format (24 kHz, mono, s16le), so a page can join the parts
  itself. Runs are limited to 4 active and 32 retained per principal, so a
  page runs parts one at a time and dismisses each after collecting it.

**Transcription language (2026-11-02, FE thread's report).** With no `language`,
whisper-server treats audio as English: its `--language` defaults to `en`, and
neither gateway sets it. Go and TS match: a real side-by-side run on scratch roots
gave the same text and fields for every case below, on the pinned runtime.

| Model | Audio | `language` | Result |
| --- | --- | --- | --- |
| `whisper-base-q8_0` | Spanish | none | wrong English, like a rough translation; `language: english`, `detected_language: spanish` |
| `whisper-base-q8_0` | Spanish | `auto` or `es` | Spanish; `language: spanish` |
| `whisper-base-q8_0` | English | none or `auto` | English, the same text |
| `whisper-tiny-en-q8_0` | English | none or `auto` | English, the same text |
| `whisper-tiny-en-q8_0` | Spanish | none, `auto` or `es` | "(speaking in foreign language)"; `language: english` |

- **This is a known difference from what people expect, kept for TS parity
  (operator).** `language=auto` is the workaround, and the Transcribe studio sends
  it. Chat dictation (`dictation.js`) sends no language.
- **`.en` models take `language=auto` safely.** They stay English and mark foreign
  speech. Their `detected_language` is meaningless (it said "afrikaans" for
  English speech), so do not show it for them.
- **OpenAI:** its reference lists `language` as an optional hint with no default
  and says nothing about omitting it, and its guide describes detection only for
  `gpt-transcribe`. Auto-detection on omission is the common understanding, but
  those pages do not state it; the platform.openai.com page could not be read.
- **One short clip per language,** so this shows the behavior, not accuracy rates.

**Considered, not built, for the user to decide:**
1. **Auto by default on multilingual models:** the gateway adds `language=auto`
   when the caller sends none and the model has the `multilingual` capability. `.en`
   models are unaffected. It fixes API clients and dictation with no setup, and
   would be an explicit difference from TS. The risk is misdetection on very short
   or quiet audio, which today is at least consistently English.
2. **A `speech.default-language` setting:** default `en` (today's behavior), with
   `auto` or a code allowed. Parity stays by default, but people have to find it.

I recommend the first: wrong text with a 200 is the worse failure, and callers who
want English can still send `language=en`.

**Transcription.** Only `whisper-server/v1` runs transcription in this
build; the CrispASR command refuses it, and an imported `process-openai`
backend answers in its own shape.
- **`verbose_json`:** gives `segments[].start` and `.end` in seconds, plus
  `segments[].words[]`. `timestamp_granularities[]=word` adds no top-level
  `words`; word timings are under segments. A run's `result` is the same
  body.
- **Translations:** on an English-only model the answer is a plain
  transcription (`task: "transcribe"`). Multilingual models were not
  measured.

**New run targets** (§21.9), labels in brackets: `/v1/audio/translations`
("Translation") and `/v1/systemone` ("Decision"). Scopes, refusals and
queueing are the routes' own, as for every target. A real decision run was
accepted at once while its model loaded, and completed in 9 s.

**Fix.** A run's event-stream body is parsed as chunks only for chat. A
speech run with `stream_format: "sse"` used to end `completed` with no
result; now the stream is its `result`, as `text/event-stream`.

**Tests:**
- `TestImageSeedFromPNG`, using real sd-server text, including a prompt
  that imitates the field.
- `TestAnnotateImageSeeds`, `TestImageSeedEcho`
- `TestRunDecision`, `TestRunTranslationNotConfigured`
- `TestRunMediaEventStreamIsItsResult`
- `TestSpeechTruncationVisible` and `TestSpeechTruncationInRun`, with the
  256-character number list as the fixture; prose of the same length is not
  flagged. The fake `tts-server` speaks at the measured rates.
- `TestLabImageSeed`, and `tools.test.ts` for the chat tool.
- The fake sd-server now honors `n` and `seed` as measured above.

**Known limitation: clicks in Qwen3-TTS speech (2026-11-02, FE thread's report).**
An upstream `tts-server`/vocoder artifact, not a gateway fault; there is no
gateway change (operator, 2026-11-02). In 73 requests of ordinary prose, 8
(about 11%) had a burst of at least 3 sample jumps over 12,000 or one over
20,000, and 12 (about 16%) had any jump over 12,000.
Evidence: a recording proxy between the gateway and `tts-server`, on a scratch
gateway. Nobody has listened to this audio; it was checked by analysis only.
- **The gateway adds nothing:** in 73 of 73 requests the WAV's samples were
  byte-identical to what `tts-server` sent. `/v1/audio/speech` adds a 44-byte
  header and does no resampling or conversion.
- **The clicks are the runtime's output.** They are short bursts, not single
  samples: 8 of the 73 had one (jumps up to 26,622), and all were already in
  `tts-server`'s bytes. They do not fall on its 72-frame vocoder windows.
  `tts-server.cpp` only clamps and scales floats to s16, so a value of exactly
  32767 in a burst had saturated before that point. Whether the vocoder or the
  sampled codes cause them was not separated: the pipeline's source is upstream
  llama.cpp, not in this repo.
- **A seed does not repeat speech.** Three `/v1/generate` calls with the same
  `seed` gave three lengths. The gateway sends no seed.

**Why number lists vary (same day).** The only generation setting LocalBase
passes is `-n 256`, the frame cap, as TS does; no sampling flags. The same
112 characters of numbers, 8 runs each, on the real binary:

| Launch | Frames (0.08 s each) | At the cap |
| --- | --- | --- |
| As shipped (`-n 256`) | 202–256 | 3 of 8 |
| `--temp 0.5` | 185–256 | 7 of 8 |
| `--repeat-penalty 1.1` | 188–256 | 4 of 8 |
| `-n 512` | 195–311 | none; all stopped on their own |

So the length is the model's own, varying by about a third, and the cap cuts
speech that would have finished (5 of 8 ran past 256). Lists run up to about
2.8 frames a character, so about 85 characters stays under the cap on this small
sample. Sampling flags did not help the cap. Bursts were 2 of 10 as shipped
and 0 of 10 for both sampling variants, too few to conclude anything.

**Considered, not built: a higher speech frame cap (2026-11-02).**
`SpeechMaxFrames` stays 256 (TS parity), and `speech.max-frames` is not being
built (operator): the benefit is narrow, 512 does not solve it (about 600+ frames
is needed), and the Studio already splits number lists. Reopen it if a user asks,
or once Linux, Vulkan and CPU timing is measured. Measured once, on this machine
(Metal), with 256 characters of numbers as the worst input and context 2048
unless noted. Vulkan and CPU were not measured.

| Cap | Frames reached | Time per request | Peak RSS | Footprint |
| --- | --- | --- | --- | --- |
| 256 (shipped) | 256, 4 of 4 | 6.4–7.0 s (14.6 s with the load) | 3.41 GiB | 2.6 GB |
| 512 | 498–512, 3 of 4 at the cap | 12.0–13.0 s | 3.42 GiB | 2.3–2.6 GB |
| 1024 | 458–599, none at the cap | 11.5–14.7 s | 3.42 GiB | 2.6 GB |

- **Memory:** no measurable change; a higher cap only lets a request run longer.
  The admission reservation was 4.11 GiB.
- **Context:** 1024 ran 512 frames fine (2 of 2 at the cap), with 0.1 GiB less
  peak RSS. At 600, both runs stopped short (417, 428 frames); that may be the
  context or natural stopping, so 600 is not known to be enough. 2048 covers
  every cap measured here.
- **Time:** about 0.025 s a frame, well inside the gateway's 120 s speech timeout
  here. A request running all the way to 1024 was not seen; by that rate it would
  take about 26 s.
- **Benefit:** narrow. Prose stayed under 256 (182–189 frames for 255 characters).
  Only digit-heavy text gains, and 512 still cuts the worst input.

**The proposal, as considered:** an opt-in `speech.max-frames`
in `go_settings` (default 256, allowed 256–1024, a Go-only setting like
`speech.provenance-marking`, so rollback stays safe), shown in doctor and config.
It takes effect when `tts-server` next loads, applies to `tts-server` only, and
the PCM limit and truncation flag follow it.

**Known flake: `TestUnloadScope`.** One macOS CI run (41407289747, on
`rewrite/go-fe-studio-speech`) failed only in Go's temp-dir cleanup, "directory
not empty", with every check in the test passing; it passed on rerun. So a file
appeared in the scratch root during teardown, and which file is unknown. Not
reproduced: 470 parallel runs of the test (150 under `-race`), and 3 full
`-race` package runs with the CPUs saturated. Delaying the `footprint` peak write
in `LLM.watch`, which `Close` does not wait for, did not reproduce it either (240
runs), though that test did not confirm the delayed write ran. Gateway test roots now use `scratchDir`, so a repeat fails with the files left
behind (paths, sizes, ages, never contents), and says whether a retry cleared
them: `TestRemoveScratch`.

**Contract tests:** image admission tests need free memory and should not
run alongside other model loads (`harness_test.go`).
`TestDifferentialImageRejections` logs the free-memory reading when it fails.
Its one failure (2026-11-01) came during a 7 GB model load in another session.

## 25. First-run browser access: a one-time local sign-in link (design addendum, 2026-11-02)

Operator request, from the OSS-readiness audit. On a fresh root the browser has
no way in: `/app/login` answers raw JSON `ui_access_denied`, and the only paths
are configuring an identity provider or `--no-auth`. So "install, start, first
chat" cannot end in the browser.

**The idea.** Whoever can run the CLI on this root already has full control of
it (the CLI is the break-glass path, §13.6). So the CLI may hand that same
person a browser session, as long as the link can only be used from this
computer.

### 25.1 Issuing

`local-base access login-link [--force] [--json]` prints:

```text
http://127.0.0.1:2273/app/login/once#<token>
```

- **Token:** 32 random bytes, base64url. Only its SHA-256 is stored, with a 10-minute
  expiry, in a new Go table `local_login_links`. It is `CREATE TABLE IF NOT EXISTS`,
  `go_schema` stays 1, and TS ignores it.
- **One at a time:** issuing replaces any unused link.
- **`--force` is needed when an identity provider is configured:** otherwise it
  refuses and says so, since sign-in already exists.
- **The URL uses the gateway's real port.** A wildcard host prints `127.0.0.1`. A
  gateway with no loopback listener (a LAN IP only) refuses, because the link could
  not be redeemed.
- **It works with the gateway stopped:** the link is in the database, and the
  output says to start it.

**First-run output.** `start`, and foreground `serve`, issue and print one link
**once**: when browser access is unconfigured and none has been shown on this root
(`go_settings` key `access.first-run-link-shown`). It goes to the terminal only. A
service-managed `serve` never prints it, so it cannot land in the captured service
stderr. `--json` carries it as `loginLink` on the `started` event.

**Deviation from the brief: a fragment, not `?token=`.** The token sits in the URL
fragment and is redeemed by a same-origin POST from a hash-pinned script, exactly as
magic links do (`MagicLinkPage`). A fragment never reaches a server log, and a link
preview or a prefetching browser cannot use up a single-use token on GET.

### 25.2 Redeeming, and the loopback rule

`GET /app/login/once` serves the redeem page; `POST` takes the token. Both, and every
later request on the resulting session, must pass **all** of:

1. the socket: both `RemoteAddr` and the listener's local address are loopback;
2. the `Host` header is a loopback literal or `localhost`, on the gateway's port;
3. no proxy markers: `Forwarded`, `X-Forwarded-*`, `X-Real-Ip`, `Cf-Connecting-Ip`,
   `Cf-Ray`, `Cf-Access-Jwt-Assertion`.

**The socket alone is not enough:** a tunnel or reverse proxy on the same machine,
such as this project's own Cloudflare deployment, connects from `127.0.0.1`, so a
remote visitor also looks like a loopback client. Rules 2 and 3 are what exclude
them, and rule 2 also stops DNS rebinding. A proxy configured to rewrite `Host` to
loopback and strip every marker would pass; that is called out in the docs as
unsupported in front of a local session.

**Failures:** a wrong, used or expired token gets the same "This link has expired"
page, status 410 (no oracle). Failures are limited to 10 per minute per gateway, then
429 with `Retry-After`. A request that fails the loopback rule gets 403 and does not
count, so a remote visitor cannot use up the local person's tries. There is at most
one live link, so the token's hash is compared in constant time against it, and a
single conditional `DELETE` spends it, so a double submit gets one session. `GET` only
shows the page, so a link preview cannot spend a link.

### 25.3 The session

- **Cookie:** `localbase-local-session`, `HttpOnly`, `SameSite=Strict`, host-only,
  **not** `Secure`. The existing `__Host-` cookie requires `Secure`, which Safari
  rejects on `http://127.0.0.1`. Plain HTTP on loopback does not leave the machine.
- **Server side:** a hashed token in a new Go table `local_sessions`. Keeping it apart
  from the TS session rows leaves rollback untouched.
- **Lifetime:** 12 hours idle, 7 days at most, shorter than the 7 and 30 days of
  identity provider sessions, because nothing can review or disable this identity.
- **At most 5 at once.** A new one ends the least recently used.
- **Permissions: all of them, on purpose,** as `--no-auth` and the CLI have: whoever
  can issue a link already controls the root. It deliberately does not go through the
  role policy, and it does not count as a manager for the lockout rules (§13.6 item
  5), because it is not an identity anyone else can see or manage. Because it bypasses
  the policy, `login-link` refuses when an identity provider is configured unless
  given `--force`, and the first-run link is not offered there.
- **The same CSRF and same-origin rules** as other browser sessions, with this request's
  loopback origin in place of the configured one.
- **Identity:** the owner id is `local`, so runs and results (§21) belong to it.
- **Sign out** works as usual. `local-base access login-link --revoke` removes every
  local session and unused link. `local-base access sessions list` and `sessions revoke
  <id>` list and end them, and `GET`/`POST /_localbase/local-sessions` does the same for
  Admin (`sessions:read`, `sessions:revoke`; `{"action":"revoke","id":...}` or
  `{"action":"revoke-all"}`). No response carries a token or a hash.
- **Only from this computer.** A phone or another computer needs an identity provider.
  The CLI says so when it prints a link.

### 25.4 `/app/login` when there is no way in

A navigation (`Accept` includes `text/html`) gets an HTML page, status 403, saying
what to do; a page that needs sign-in shows the same help in place, status 401. API clients keep today's JSON, so the differential and any script are
unchanged.

| Situation | Page says |
|---|---|
| No access configured, request passes 25.2 | Run `local-base access login-link` in a terminal on this computer, then open the link it prints |
| No access configured, request does not pass | Browser sign-in is not set up. Whoever runs this LocalBase can set it up on that computer |

The FE thread styles it. The contract is `web.LoginHelp{Local bool}`: a template that
gets this one flag and shows the command, nothing else from the server.

### 25.4a Threat model

**What it protects against:**
- Someone elsewhere on the network, or arriving through a tunnel or standard reverse
  proxy, using a link or a session. They fail the loopback rule on every request.
- A web page in a browser on this computer. Rebinding fails the `Host` check, and
  cross-site requests fail the same-origin rules and the `SameSite=Strict` cookie.
- A link in a log, a preview or a prefetch. The token is in the fragment, so it is not
  sent to any server, and a `GET` never spends it.
- A copy of the database or a diagnostics bundle being replayed. Only hashes are stored.
- Guessing. Tokens are 32 random bytes, and redeem failures are rate limited.

**What it does not protect against:**
- **Anyone who can run commands as this user, or read the terminal or its scrollback,
  within the 10 minutes.** They could already run the CLI, which has full control.
- **Another account or process on this computer that gets the link before you use it.**
  Anything that can reach loopback and holds the token can redeem it.
- **Malware or a browser extension in your browser session.** The session has every
  permission, and nothing stops code acting as you in that browser.
- **Traffic on loopback.** It is plain HTTP, which does not leave the machine, but a
  privileged local process could read it.
- **A proxy that rewrites `Host` to loopback and strips every proxy header.** It looks
  exactly like a local request. This setup is unsupported in front of a local session.
- **Reviewing who did what.** Audit events say a local session acted. Every local
  session is the same owner, `local`, not a named person.

It is not sign-in for other people or devices. Those need an identity provider.

### 25.5 Telemetry

`audit.local-login-issued` and `audit.local-login-redeemed`, with `link_id` (the first
8 characters of the token's hash), `actor_kind` and `forced`. Refusals log
`auth.local-login-refused` with a reason (`expired`, `not-loopback`, `rate-limited`). Never
the token, a URL with one, or a hostname.

### 25.6 Rollback and tests

**Rollback:** older binaries and TS ignore both tables, and `go_schema` stays 1. Local
sessions stop working until a build that knows them returns, and the identity provider
path is untouched. `tools/placement-rollback-check.sh` was not extended and no rollback
drill was run; this rests on the tables being additive, as with `model_placements`.

**Tests:**
- `TestLoginLinkSingleUse`, `TestLoginLinkExpires`, `TestLoginLinkHashedAtRest`,
  `TestLoginLinkReplacesUnused`, `TestLoginLinkRateLimited`, `TestLoginLinkSurvivesPreview`,
  `TestLoginLinkRedeemForm`, `TestLocalSessionLifetime`, `TestLocalSessionsCappedAndRevoked`,
  `TestLocalSessionCSRF`, `TestLocalSessionLogoutAndRevoke`.
- `TestLocalSessionLoopbackRule`: a table over the socket, `Host` and each proxy marker,
  on redeem and on every later request, including a tunnel-shaped request with a valid
  cookie.
- `TestFirstRunLocalSignIn`: the real binary from `init`: the link printed once, the
  pages, the session, the refusal with a provider unless `--force`.
  `TestServiceServeNeverPrintsLink`.
- `TestLoginHelpPage`: HTML for navigations, JSON for API clients.
- `TestLoginLinkAudit`: the planted-token scan over logs and the diagnostics bundle.
## 26. The host block and the admission preview (2026-11-02)

The memory strip and Running must tell the truth about the machine across every
modality. Both routes are read-only, start and stop nothing, and read one shared
sample a second (`memory.Controller.reading`), so polling never spawns a process per
call. Admission and the pressure monitor keep reading the host themselves, always
fresh. **An unknown figure is `null`, never 0.**

**`GET /_localbase/host`** (`models:read`):

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

- `source` for `total` and `available` is `system` or `unavailable`. For a runtime it is
  `measured`, or `planned` when it is starting or the platform cannot sample it (Linux
  today). `localBase.source` is `measured`, `planned`, `mixed` or `none`.
- `other` is `total - available - localBase`, clamped at 0, `derived`, and `null` when
  `total` or `available` is unknown. It includes what the system itself holds.
- `pressure` is the state admission refuses on (`healthy`, `constrained`, `critical`), or
  `null` with no reading. It comes from available memory against the reserve, with
  hysteresis. It is not a macOS pressure API.
- `host.memory` covers the system pool. On a discrete GPU the VRAM is not in it.
- `activity.busy` is true when any modality has a request in flight or queued, or a
  detached run has not finished. `video.inFlight` counts a generating job.

**The computer name** is the `go_settings` key `host.name`: the owner's, else
`os.Hostname()` without `.local`. `PUT /_localbase/computer-name` with `{"name": "…"}`
(`models:manage`) sets it, at most 64 characters; an empty name goes back to the
machine's. It stays out of logs and diagnostics.

**`GET /_localbase/models/{id}/admission`** (`models:read`) says what a request would
meet now: `modelId`, `runtime`, `admitted`, `requestedBytes`, `reserveBytes`,
`availableBytes`, `pool`, `demandSource` and `refusal`. **It is the real path:** the
demand comes from what a switch computes, the verdict from the same
`Controller.evaluate`, and `refusal` is `runtimeError`'s own status, code, message and
`Retry-After`, plus `permanent`. `TestAdmissionPreviewMatchesRefusal` compares them with
a real request. A refused preview's figures are the refusal's, so a GPU refusal shows
accelerator bytes against the GPU limit.

- It credits this runtime and every idle one recovery could stop, as a request finally
  would, without stopping anything. So `availableBytes` can exceed what is free now.
- A model not on disk answers `admitted: false` with
  `model_install_consent_required`. For an LLM its figures are then `null`, because the
  real path checks the install first. Image and STT report memory first, as they do.
- **`?assumeInstalled=1` answers the memory question alone,** for a model not yet on
  disk: `admitted`, the figures, and `refusal` only for memory (`model_too_large` with
  `permanent: true`, or `insufficient_memory`). The verdict is the real `evaluate` on
  what the model is expected to need. That figure is an estimate: `demandSource` says
  which (`estimated`, `measured`, `feed`, `observed` or `derived`), and it can change
  once the file's real geometry can be read.
- A video refusal has `jobFailure: true` and `status: null`, because the request is accepted
  and the job then fails (§13.6).
- Readiness is not promised: a model can pass and still fail to start.

`/app/strip`, which every visible page polls, is not traced and logs only on failure,
like `/health`.

## 30. Connect an app, and the OpenAPI document (design addendum, 2026-11-02)

Someone who has a model running should get an editor, an agent or their own code
talking to it in a minute, without reading the source. Today that takes the API
reference, a key command with the right scopes, and knowing that a client wants
`…/v1` and a model id. Nothing describes the API in a form tools can read, and
`production-inventory.md` lists "route table or OpenAPI tested against routes" as missing.

### 30.1 `GET /openapi.json`

- **Public, like `/health`:** no credential, because docs tools and client generators
  fetch it without one. It is the same static text everywhere and holds no instance data:
  no paths, host names, model ids or key names. The version is already public on `/health`.
- **Answers:** 200 `application/json; charset=utf-8`, `Cache-Control: no-cache`, a strong
  `ETag`, 304 on `If-None-Match`. `HEAD` works. Any other method gets 405 with `Allow`.
- **The document** is `internal/gateway/openapi.json`, embedded and edited by hand. It is
  OpenAPI 3.1.0. `info.version` in the file is a placeholder, and serving replaces it with
  the running `Version`, so a stamped build (#406) reports itself. `servers` is
  `[{"url": "/"}]`, relative to where the document was fetched, so it is right behind a
  proxy.
- **Covers:** `/v1`, `/_localbase`, `/health`, `/health/ready` and itself.
- **Leaves out, by name and with a reason in the test:** the browser's pages and sign-in
  (`/app/*`, callbacks), and the routes only this machine's CLI can call, behind the
  instance token (`/_localbase/instance`, `/default-model`, `/placement-state`).
- **Extensions on each operation:**
  - `x-localbase-permission`: the permission the route needs. It is absent on a public route,
    and `"any"` means any authenticated caller. A method's own permission is on that method's
    operation.
  - `x-localbase-also-requires`: a second permission the route needs today. `unload` and
    `placement` sit under the `models:read` guard of `/_localbase/models/{model_id...}`, so a
    `models:manage`-only key gets 403, which §23 and §20.2 do not say. The document states what
    the code does, and the test fails if it stops being true.
  - `x-localbase-stability`: `stable` for `/v1`, `unstable` for `/_localbase`, as
    `docs/versioning.md` promises.
- **Security schemes:** `Authorization: Bearer` and `X-Api-Key`, the two `Resolver.Token`
  reads. The document says what the code enforces: **a request carrying `Origin` or
  `Sec-Fetch-Site` never authenticates with an API key** (`auth.IsBrowser`), so a key cannot
  be used from a web page, and the gateway sends no CORS headers.
- **Schemas** are exact for `/v1` and for what `/_localbase` returns; request schemas state
  the bounds the validators enforce. Pass-through fields stay open
  (`additionalProperties` is not `false`) where the gateway forwards them to the backend.

### 30.2 Tests that stop it drifting

No test dependency is added: the project keeps one module dependency. All of these use the real
`gateway.New` over the fake backend, in `internal/gateway/openapi_test.go`.

| Test | Fails when |
|---|---|
| `TestOpenAPIDocument` | it is not 3.1.x; an operation lacks an id, a response or a known tag; a `$ref` or path parameter does not resolve; a permission is not in `auth.Permissions` |
| `TestOpenAPICoversEveryRoute` | a pattern registered on the mux is neither documented nor exempt by name; an exemption names a route that is gone; a documented path reaches no route |
| `TestOpenAPISubroutes` | a wildcard route's real operations (`models/{id}/unload`, `/admission`, `/placement`, `runs/{id}/…`, `voices/{id}/audio`) differ from the document; `runTargets` differs from the documented targets; anything `allowedUIRoute` allows is undocumented |
| `TestOpenAPIMethods` | a documented method gets 405 or `route_disabled`; an undocumented method does not get 405. The handlers that have never checked the method (TS parity) are listed by name |
| `TestOpenAPIPermissions` | for each operation: no key does not get 401; a key with every permission but the documented one does not get 403; a key with only that one (and any `also-requires`) does get 401 or 403; a key lacking an `also-requires` does not get 403. It uses a real model id, since some routes look the id up first, and a valid body for the route that validates first |
| `TestOpenAPIResponses` | a real response has a status the operation does not document, or a body its schema rejects (a small validator for the keywords the document uses) |
| `TestOpenAPIServed` | the answers in 30.1 change, or the served version is not `Version` |

So a route added without documentation fails CI, and so does documentation without a route.
Each test was checked by breaking the document or the routes on purpose: a dropped path, method,
status or field, a wrong permission, a fake path, a bad `$ref`, and an undocumented route each
fail at least one test. **What these cannot catch:** a request field the validators accept and
the document omits, a sub-route of a wildcard route that the code adds in a way the source scan
does not read, the schemas of answers no test reaches, and the wording of descriptions. The syntax is also checked once by an external OpenAPI 3.1
linter, run by hand and reported in the PR; CI does not run it, since that would add a
dependency.

### 30.3 Connect: one shared source for page and CLI

`internal/connect` is a small package with no gateway dependency. Given a base URL, a model id
and a key (or none), it returns each client's snippets. The page and the CLI both render from
it, so they cannot disagree. It has **no HTTP API of its own**: keys go through the existing
`/_localbase/api-keys`, and base URLs are not something a remote caller needs from the server.

**Base URLs.** Each has a label and an optional note.
- **This computer:** from the listen address. A wildcard reads as `127.0.0.1`, and a LAN-only
  bind gives that address.
- **Your public address:** the `origin` from `ui-access.json`, when one is configured. With
  Cloudflare Access the note says API requests must also get past Access at the edge (a
  service token or a bypass rule for `/v1/*`), since the gateway only checks the key.
- **This page's address (page only):** when the page is open at another address, such as a LAN
  name. Never added for a loopback name.

**The key token.** Snippets read the key from the environment wherever the client allows.
The one place it appears literally is the token `YOUR_LOCALBASE_KEY`.

**The model** is the effective default chat model, else `YOUR_MODEL_ID`, with a choice among
the installed chat models. Copy never names a model: the id is data, shown as chosen.

### 30.4 `/app/connect`

Needs `models:read`, like the pages around it; creating a key needs `keys:manage`.

1. **Base URL,** with a copy button; a choice only when there is more than one.
2. **Key.** A person with `keys:manage` gets one button, "Create a key for chat"
   (`inference:chat`, `models:read`), with chat and embeddings, and all inference, one
   step further. `POST /app/connect/key` calls `/_localbase/api-keys` as that person, so the API's
   validation and audit apply. Anyone else is told who can make one and sees the CLI command.
3. **Model,** when there is a choice. Changing it re-renders the snippets with htmx.
4. **Your app:** an accordion by group (Code; Editors and agents; Apps), each with its snippets,
   where they go, a copy button and a link to the client's own docs. `?client=<id>` opens one.
5. A link to `/openapi.json`, and what will not work (30.6).

**The secret** appears once, in the response to the POST, as Admin does: never in a URL, no
history entry, `Cache-Control: no-store`. The response fills the token into every snippet. `connect.js`
keeps it in memory and re-applies it after an htmx swap, so changing the model does not lose it.
Leaving the page does. Like every change in the UI, creating a key needs JavaScript: the CSRF and
same-origin rules depend on htmx's headers.

`--no-auth` says any key works and that this is for local development only.

Data contract for the FE thread: `Bases []{Label, URL, Note}`, `Base`, `Models []{ID, Name}`,
`Model`, `Groups []{Label, Clients []{ID, Name, Docs, Note, Open, Snippets []{Label, Path, Language,
Text}}}`, `CanCreateKey`, `Presets []{ID, Label, Scopes}`, `Secret`, `Error`, `NoAuth`.

### 30.5 `local-base connect`

```text
local-base connect [client] [--model <id>] [--base-url <url>] [--list]
                   [--create-key] [--name <label>] [--scopes <permission,...>] [--expires-days <n>]
```

- **With no client:** base URLs, whether the gateway is running, the model, how to get a key, and
  the client ids.
- **With a client:** the snippet that does the job on stdout, and everything else on stderr, so
  redirecting a config into a file works. Snippets read the key from `LOCALBASE_API_KEY` wherever
  the client allows, so setting it comes first. An unknown client exits 2 and lists the ids.
- **`--base-url`** overrides the address, for a proxy the CLI cannot know about.
- **`--create-key`** creates a key the way `keys create` does, and fills it into the snippets. It works
  with the gateway stopped. The defaults are the name `connect-<client>` and the chat scopes;
  `--scopes` is the complete set, as elsewhere. It writes `audit.api-key-changed` with ids only.
  Without the flag no key is created and no secret is printed.
- **`--json`** gives the usual envelope: `{"baseUrls":[{"label","url","note"}],"baseUrl","model",
  "models","gateway":{"running"},"clients":[{"id","name","group","docs","note","snippets":[{"label",
  "path","language","text"}]}]}`, plus `key` and `secret` with `--create-key`.

It is Go only. TS has no such command, and the differential does not compare it.

### 30.6 What will not work, stated on the page and in the docs

- **A client that requires `/v1/responses`, `/v1/completions` or Anthropic's `/v1/messages`.**
  None of them exists here. The gateway serves chat completions.
- **Keys from a web page** (30.1): a browser-side app cannot use a key, by design.
- **Services that call the base URL from their own servers** cannot reach `127.0.0.1`. They need
  the public address.

Which clients appear, and each one's exact fields, come from that client's current documentation and
are recorded in `internal/connect` with the date they were checked. A client that cannot use chat
completions is not listed as supported. Each client says which it is, on the page and in the CLI:
**run** (its snippet has been run against a gateway) or **follows the client's docs, not run**.
Today that is run for curl and the two SDKs, and not run for every editor, agent and app.

### 30.7 Tests for Connect, and what is not verified

- `TestRenderEveryClient`: every client renders for every combination of key and model, with no
  unfilled token other than the two documented, and no model name in fixed copy.
- `TestBaseURLs`: wildcard, loopback, LAN, IPv6, public origin, and the Cloudflare note.
- `TestSnippetsWorkAgainstTheGateway`: runs the `curl` snippet's request against a real test gateway,
  and checks that every snippet's base URL ends in `/v1` and reaches `/v1/models`.
- `TestConnectPage`: permissions, one-tap key with exactly the preset scopes, the secret absent from
  a later GET and from every URL, the no-store header.
- `TestCLIConnect` (contract suite, real binary): text and JSON, stdout against stderr, `--create-key`
  with the gateway stopped, the secret absent without the flag, unknown client exits 2.
- **Checked by hand, 2026-11-02:** curl, and the OpenAI Python (3.42.1) and JS (7.47.0) SDKs, running
  exactly what `local-base connect` prints against a scratch gateway with the fake backend. Each got
  the fake's own reply, and a wrong key got 401.
- **Not run:** opencode, Continue, Cline, Aider, Zed, goose and Open WebUI. Their snippets follow each
  client's docs. Someone who runs one can check it, and these are listed in `docs/connect.md`.

**Rollback:** nothing here touches the database or `go_schema`. An older binary simply lacks the
page, the command and the route. The keys it creates are ordinary keys.

## 29. Usage and activity (design addendum, 2026-11-02, for the operator's review)

Someone running this for themselves, or for a few people and apps, should be able to answer: which
key is busy, which model, how many tokens, how often it fails, and how slow it is. Today that means
reading logs. **Nothing here is built yet.**

### 29.1 What is stored, and what never is

**Hourly totals, not a log of requests.** One row per hour, principal, route, model and status. There
is no per-request timeline, so nobody can reconstruct when a person sent any one message, and the
table stays small: a busy gateway writes a few thousand rows a month.

| Stored | How |
|---|---|
| Hour | The start of the hour, UTC. The only timestamp |
| Principal | Its kind and id: a key's id, a browser identity's owner hash, or `local`, `environment`, `no-auth`. Never an email or a name |
| Route | One of a fixed list: `chat`, `embeddings`, `decision`, `speech`, `transcription`, `translation`, `image`, `image-edit`, `video` |
| Model id | Only an id the gateway knows. A request naming anything else is stored with no model, so a string a client typed can never land here |
| Status | The final HTTP status, as a number |
| Counts and totals | Requests, input tokens, output tokens, and how many requests reported tokens, so a total is not misread as complete |
| Durations | Sum, queue-wait sum, and a latency histogram (29.3) |

**Never stored:** prompts, completions, request or response bodies, file names, key material, headers,
IP addresses, user agents, emails, names, request ids, per-request timestamps, error messages, and the
`user` field clients can send. A key's name is looked up when usage is read, and is not copied here.

**Nothing leaves the machine.** No new network call, no telemetry, and no cost estimates: a local model
has no per-token price, and a rate made up here would be a number nobody can check. OTLP, when someone
has configured it, exports what it already does, and this table is not added to it. The diagnostics
bundle leaves this table out, and backup (§33) will too unless asked.

### 29.2 What counts

Requests to the inference routes that got as far as an identified principal, once each:

- A request refused for its body, its scope, a full queue or memory counts, under that principal. That is
  what makes an error rate true. A request with no valid key has no principal and is not counted; the
  logs cover those.
- **Failed, refused and cancelled are separate.** `failed` is 5xx, `refused` is any other 4xx, `cancelled` is
  499, a client going away. The error rate is `(failed + refused) / requests`, so people closing a tab
  do not look like an outage.
- **A stream that fails after it started counts as failed,** though its HTTP status was 200.
- **A detached run (§21) counts once,** as its inner request. The UI's in-process calls count as the signed-in
  person.
- **A video job counts once, when it ends:** completed as 200, failed as 500, cancelled as 499, with the job's whole
  time as its duration. A refused create counts as a refused request.
- Not counted: `/v1/models`, `/_localbase`, `/app` pages, health checks.

### 29.3 Percentiles are approximate, and say so

Latency is the time to the last byte, in a histogram with buckets 25% apart from 1 ms to 1 hour, about 70
in all, stored sparsely. Merging hours gives p50, p95 and p99 over any range, within about 12%. The API marks
them `approximate`, and the page shows them with a `~`. Exact percentiles would need a row per request, which
29.1 rules out. Time to first token is not recorded in this version.

### 29.4 Storage, retention and clearing

- **Table:** `usage_hourly` in `local-base.db`, `CREATE TABLE IF NOT EXISTS`. `go_schema` stays 1 and TS ignores it,
  like `local_sessions`. File permissions are the root's (§27).
- **Writing:** counted in memory and written every 30 s and at shutdown, in one transaction. So a request never
  waits on SQLite for this, and a crash loses at most 30 s of counts. Only `serve` writes. A failed write logs
  `usage.flush-failed`, keeps the counts, and never fails a request.
- **Retention:** 30 days by default, from 1 to 365, in `go_settings` key `usage.retention-days`. Pruned hourly, and
  reads never return anything older.
- **Turning it off:** `usage.recording`, on by default. Off, nothing is counted or written; what exists stays until it
  is cleared or ages out.
- **Clearing:** `POST /_localbase/usage` with `{"action":"clear"}`, or `local-base usage clear --yes`. It removes
  every row, and optionally only those before a date. The API also empties the in-memory counts. **The limit:** the CLI
  writes to the database directly, so with a gateway running, up to the last 30 s of counts can reappear after it clears.

### 29.5 Who sees what

| Caller | Sees | Changes |
|---|---|---|
| Any authenticated principal | Its own usage only. `scope=self`, the default. A key cannot name another principal | Nothing |
| `models:manage` or `system:read` | Everything: `scope=all`, totals and by key, person, model or route | Nothing |
| `system:manage` | The same with `system:read` | Retention, recording, clearing |
| The CLI on this computer | Everything, as with `keys` | Everything |

A caller asking for `scope=all` or another principal without the permission gets 403, not an empty answer. `--no-auth`
has one principal, so `self` and `all` match.

**Per-key against total:** `group=principal` splits by key and person, and `principal=<id>` narrows to one. With no
`group`, the answer has totals only.

### 29.6 `GET /_localbase/usage`

Query: `range=24h|7d|30d` (default `7d`) or `from` and `to` as RFC 3339, `scope=self|all`,
`group=principal|model|route` (repeatable), and the filters `principal`, `model`, `route`. `step` is `hour` up to 7 days,
else `day`. Times outside retention are clipped, and `range.from` says where the answer really starts.

```json
{"object": "localbase.usage", "scope": "self",
 "range": {"from": "2026-10-26T00:00:00Z", "to": "2026-11-02T23:00:00Z", "step": "hour"},
 "recording": true, "retentionDays": 30, "pendingSeconds": 30,
 "totals": {"requests": 1240, "failed": 3, "refused": 12, "cancelled": 7, "errorRate": 0.0121,
   "inputTokens": 91234, "outputTokens": 40112, "requestsWithTokens": 1190,
   "latencyMs": {"p50": 420, "p95": 2800, "p99": 9100, "approximate": true}, "queueWaitMsAvg": 12},
 "groups": [{"principal": {"kind": "api-key", "id": "key_…", "name": "my editor"}, "model": "…", "route": "chat",
   "requests": 800, "failed": 1, "refused": 2, "cancelled": 3, "errorRate": 0.0038,
   "inputTokens": 60000, "outputTokens": 30000, "requestsWithTokens": 790,
   "latencyMs": {"p50": 400, "p95": 2500, "p99": 8000, "approximate": true}, "queueWaitMsAvg": 9}],
 "series": [{"t": "2026-11-02T22:00:00Z", "requests": 12, "failed": 0, "refused": 0, "cancelled": 0,
   "inputTokens": 900, "outputTokens": 400, "p95Ms": 2100}]}
```

- `principal.name` is present only for a key that still exists, read from `api_keys`. A browser identity shows as
  `{"kind":"browser","id":"browser:ab12…"}`, because the name was never stored.
- `groups` is ordered by requests, most first. It is a count, not a ranking of anything's quality.
- A period with no requests is left out of `series`.

**Changes:** `POST /_localbase/usage` with `{"action":"clear","before":"<RFC 3339, optional>"}`,
`{"action":"set-retention","days":30}` or `{"action":"set-recording","enabled":false}`, and `system:manage`. Each writes
`audit.usage-changed` with ids only.

The browser gets it through `/app/api`, like the other admin reads. It goes in `openapi.json`, and the drift tests of
§28.2 apply.

### 29.7 `local-base usage`

```text
local-base usage [--range 24h|7d|30d] [--from <time>] [--to <time>] [--by key|model|route]...
                 [--key <id>] [--model <id>] [--route <route>]
local-base usage clear [--before <time>] --yes
local-base usage retention <days>
local-base usage recording on|off
```

It reads the database directly, so it works with the gateway stopped, and with one running it can be up to 30 s behind. A plain
table, or `--json` with the same fields as the API.

### 29.8 The page: data contract for the FE thread

`/app/usage`: the signed-in person's own usage for anyone, and a `Everything` choice for those allowed. Admin gets a
**Usage** section that links here, and Settings a **Your usage** row. The FE thread restyles it later; the first version
uses existing components.

Template data: `Scope`, `CanSeeAll`, `CanManage`, `Range`, `Ranges`, `Group`, `Usage` (the JSON above, decoded), `Recording`,
`RetentionDays`, `Notice`, `Error`. States to design: **empty** (no requests yet), **recording off** (says so, with what still
exists), **partial tokens** (`requestsWithTokens` below `requests`), and **a range clipped by retention**. Copy names no model
and ranks nothing, and a model's id is data.

### 29.9 Tests, first

- `TestUsageStoresNoContent`: planted values in a prompt, a completion, a file name, the `user` field, an unknown model and a key.
  None appears anywhere in the database, its WAL or the logs' usage events, read as raw bytes after a flush.
- `TestUsageCountsOnce`: API, detached run, UI call, stream, stream failing midway, video job, each once.
- `TestUsageCategories`: each status lands in the right count, and 499 stays out of the error rate.
- `TestUsageScopes`: a key sees only its own, naming another gets 403, each permission in 29.5.
- `TestUsageRetention`, `TestUsageClear`, `TestUsageRecordingOff`, `TestUsageFlushFailureKeepsCounts`.
- `TestUsagePercentiles`: the histogram is within its stated bound on known distributions, and merging hours matches one bucket.
- `TestUsageDiagnosticsExcludes`, `TestCLIUsage`, the §28.2 drift tests, and `-race -count=20`.

**Rollback:** the table is additive. An older binary ignores it and does not prune it, so it stops growing and sits there until a
newer binary returns or someone removes it. **What this will not do:** per-request history, cost, alerts, or export to anywhere.
