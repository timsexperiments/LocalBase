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
| `go/internal/store/` | SQLite, migrations and typed queries |
| `go/internal/root/` | Data directory, marker and the gateway lock |
| `go/internal/service/` | launchd and systemd user units |
| `go/internal/obs/`, `otlp/` | Logs, redaction and OTLP export |
| `go/contract/` | Black-box HTTP contract suite |
| `go/tools/` | Check scripts and golden generators |
| `docs/` | User documentation |
| `scripts/release/`, `packaging/` | Go release build, installer test, Homebrew template |
| `src/`, `drizzle/`, most of `scripts/` | Legacy TypeScript version and what it needs. See below |

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

`src/` is the version Go replaced. It stays as a rollback path and as the source of the catalog and runtime pins that `go generate` copies, and golden test data comes from it. See the retirement plan in `docs/typescript-retirement.md`.

## CI

| Workflow | Runs |
| --- | --- |
| `go.yml` | Format, vet, race tests, CSS check and contract suite, on macOS and Linux |
| `repo-guards.yml` | Binaries, large files, action pins, permissions and secrets scan |
| `go-lint.yml` | golangci-lint with `go/.golangci.yml` |
| `contributor-setup.yml` | `make bootstrap`, `check`, `ui`, `lint` and `build` from a fresh Linux checkout, when the setup changes |
| `release-go.yml` | Release build, installer test on four platforms. Publishes a draft on tags only |
| `go-dual.yml` | Differential tests against TypeScript: nightly, by hand, or with the `dual` label |
| `ci.yml` | TypeScript checks, on PRs into main |
