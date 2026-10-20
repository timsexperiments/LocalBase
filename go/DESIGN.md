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
6. **The pending-restart fingerprint for declarative config.** The
   `config_activation` table and the "acknowledge static settings" handshake
   go away. Listener and memory-policy changes take effect on restart, and
   `config apply` reports `restart-required`. The table is kept but unused, so
   rollback still works.
7. **The OpenTelemetry SDK.** It brings 11 packages, a diag-echo filter, and
   batch processors whose failures needed their own reporter. In Go, a small
   OTLP/HTTP JSON exporter (logs and traces only) uses a bounded channel, one
   goroutine, a 5 s deadline, and the #358 outage-reporting rules built in.
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
- `HostProvider`: darwin uses `host_statistics64` free+inactive,
  `hw.memsize`, `kern.memorystatus_vm_pressure_level` and the Metal working
  set. Linux uses `/proc/meminfo` MemAvailable plus PSI, and NVML through
  `nvidia-smi --query` for discrete GPUs.
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
| `config validate/plan/apply/show` | Simplify | Same TOML v1 document. No activation fingerprint: `apply` reports `restart-required`, and `--restart auto` restarts the managed service. |
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
| `config_activation` table | Keep schema, stop writing. |
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

Navigation is a bottom bar on phones (Chat, Models, Lab, Settings) and a side
rail at ≥1024 px.

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
| JSONL events | Done | Schema 2, the same field names, rotation at 10 MiB with 5 archives. Not yet: OTLP or the full TS redaction rules. |
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

**Provenance (product decision, open):** CrispASR adds an inaudible AI-content
watermark and C2PA metadata by default. LocalBase passes none of its marking
opt-outs, so Kokoro output through CrispASR is marked; tts-server output is
not. Qwen3-TTS through CrispASR needs a reference voice and a consent
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
| 5–7 | Not started | |

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
