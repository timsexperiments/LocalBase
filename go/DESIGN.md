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
  install check, so too little memory answers 503 `insufficient_memory` and
  leaves the model inactive; tts has no switch preflight in TS. Go once
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
 "result": {"contentType", "bytes", "url": "/_localbase/runs/{id}/result"}}  // media, once completed
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
`status`, `reason`, `error_code`, plus `duration_ms`, `output_bytes` and
`events` once the run has ended. Message text, prompts and outputs are
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
