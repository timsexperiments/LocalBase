# Development overview

LocalBase is one Go binary: the OpenAI-compatible API, the admin API, the server-rendered UI and the CLI. It runs each model in a separate backend process, such as `llama-server`, and proxies to it. [`go/DESIGN.md`](go/DESIGN.md) is the detailed design and history.

## Repo map

| Path | What it is |
| --- | --- |
| `go/cmd/localbase/` | CLI entry point and one file per command. `serve.go` wires everything |
| `go/internal/gateway/` | HTTP router, OpenAI and admin handlers, detached runs |
| `go/internal/web/` | UI: Go templates, htmx, small JavaScript files, compiled Tailwind CSS |
| `go/internal/runtime/` | Launch plans, process supervision, per-type queues, runtime downloads |
| `go/internal/memory/` | Host memory readings, estimates and admission |
| `go/internal/placement/` | CPU and GPU placement planner |
| `go/internal/catalog/` | Built-in catalog, user entries and the remote feed |
| `go/internal/hub/`, `acquire/`, `install/` | Hugging Face discovery and downloads |
| `go/internal/auth/` | API keys, browser sessions, OIDC, Cloudflare Access, policy |
| `go/internal/store/` | SQLite, migrations (the drizzle-format SQL both versions read) and typed queries |
| `go/internal/root/` | Data directory, marker and the gateway lock |
| `go/internal/service/` | launchd and systemd user units |
| `go/internal/obs/`, `otlp/` | Logs, redaction and OTLP export |
| `go/test/contract/` | Black-box HTTP contract suite |
| `go/tools/` | Check scripts and test tools |
| `docs/` | User documentation |
| `scripts/release/`, `packaging/` | Go release build, installer test, Homebrew template |
| `src/`, most of `scripts/` | Legacy TypeScript version and what it needs. See below |

## Build, run and test

The Go module is in `go/`; run these from there.

```sh
cd go
go build -o local-base ./cmd/localbase          # one static binary, UI included
scripts/build.sh dist/local-base                  # the reproducible release build, stamped
./local-base --root /path/to/root serve --port 26410 --runtime-port-base 26411
go test ./internal/...                            # unit tests (no models needed)
scripts/contract.sh                               # the contract and system suites
scripts/tailwind.sh                               # rebuild internal/web/static/app.css (commit it)
go run ./tools/catalog-feed                       # regenerate catalog/feed*.json from internal/catalog/data
```

Tests and `scripts/*-check.sh` take scratch ports from `LOCALBASE_TEST_PORTS` (`low-high`, default
`25000-25499`; CI uses `24000-29999`). Set it when something else on the machine uses that block.
24173 and 25611-25613 are never used.

A second gateway on one computer needs its own backend ports as well as its own `--port`. Without
them it uses the configured ones, such as 18000 for the LLM. `--runtime-port-base <port>` takes five
in a row: LLM, STT, TTS, image, video. A start refuses a port another process holds (DESIGN §29).

Real inference in the contract suite needs `LOCALBASE_CONTRACT_MODELS`, a directory holding
`qwen2.5-coder-1.5b-instruct-q4_k_m.gguf` (read only), and `LOCALBASE_CONTRACT_RUNTIMES`, a
`bin/runtimes` directory copied into each scratch root. The golden plans in
`internal/runtime/testdata/llm-plans.json` were recorded from the TypeScript version and are now
Go's own: a change to them is a behaviour change, reviewed by hand.

### Build identity

`local-base --version` prints the number alone, for scripts. `local-base version`,
`--json --version`, `doctor` and the `gateway.started` log event also give the commit
and date; the first three give the Go version too. `/health` stays the number alone.

A release stamps these with `-ldflags` (`scripts/build.sh` is the exact command):

```sh
-X github.com/timsexperiments/LocalBase/go/internal/version.Version=<version>
-X github.com/timsexperiments/LocalBase/go/internal/version.Commit=<full sha>
-X github.com/timsexperiments/LocalBase/go/internal/version.Date=<RFC 3339>
```

The Go version comes from the toolchain. With `-trimpath -buildvcs=false`,
`CGO_ENABLED=0`, a commit-derived date and the same Go version, two builds are
byte-identical (`TestBuildIsReproducible`). A plain `go build` in a clone reports the
commit from version control instead; from a git worktree it says `unknown`.

## Life of a request

```mermaid
sequenceDiagram
    participant C as Client
    participant G as Gateway
    participant A as Auth
    participant M as Runtime manager
    participant B as Backend process
    C->>G: POST /v1/chat/completions
    G->>A: Resolve key or session, check permission
    A-->>G: Principal, or 401 / 403
    G->>G: Validate the body
    G->>M: Acquire a lease for the model
    Note over M: FIFO queue per type (429 if full or timed out)
    Note over M: Switch model if needed, check memory (503 if refused)
    M->>B: Start and wait for health, if not running
    M-->>G: Lease
    G->>B: Proxy the request
    B-->>C: Response, streamed chunk by chunk
    G->>M: Release the lease when the body ends
```

## Rules worth knowing

- **Few dependencies.** The only Go module the code imports directly is `modernc.org/sqlite`. There's no HTTP router, CLI framework or ORM, so every addition needs a reason.
- **No cgo.** `CGO_ENABLED=0` gives a static binary that cross-compiles.
- **One active model per type.** Requests change the model, in FIFO order.
- **Dependencies run one way:** `gateway, web → runtime → memory → catalog → store → root`, and `gateway, web → auth → store`.
- **Secrets and content stay out of logs.** The log handler redacts attributes whose names contain words like `input`, `output`, `prompt` and `content`. Test new log events through `obs.NewHandler`.
- **Detached runs** are the one place model output reaches disk (`<root>/runs/`), and that's deliberate and short-lived.

## The legacy TypeScript tree

`src/` is the version Go replaced. It stays as a rollback path, and the golden test data was recorded from it. The catalog's source is `go/internal/catalog/data`; `cd go && go run ./tools/catalog-feed` writes `catalog/feed.json` and `catalog/feed-v2.json` from it. The managed runtime manifest's home is `go/internal/runtime/managed-runtime-manifest.json`; the sync workflows also write the identical copy in `src/manager/`, which the runtime release helpers read until they move, and a test fails if the two differ. See the retirement plan in `docs/typescript-retirement.md`.

## CI

| Workflow | Runs |
| --- | --- |
| `go.yml` | Format, vet, race tests (internal/backup in shards), CSS check, the contract and system suites, and the page-script and Bun script tests, as parallel jobs on Linux and macOS; `go-checks` passes only when all of them do |
| `repo-guards.yml` | Binaries, large files, action pins, permissions and secrets scan |
| `go-lint.yml` | golangci-lint with `go/.golangci.yml` |
| `contributor-setup.yml` | `make bootstrap`, `check`, `ui`, `lint` and `build` from a fresh Linux checkout, when the setup changes |
| `soak.yml` | One soak cycle nightly on Linux and macOS, and the 30-minute soak plus a race-detector cycle weekly |
| `release-go.yml` | Release build, installer test on four platforms. Publishes a draft on tags only |
| `go-dual.yml` | Differential tests against TypeScript: nightly, by hand, or with the `dual` label |
