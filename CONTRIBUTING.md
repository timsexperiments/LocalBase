# Contributing to LocalBase

Thanks for helping. New work goes in [`go/`](go/). [`src/`](src/README.md) is the legacy TypeScript version, so please don't add features there. [DEVELOPMENT.md](DEVELOPMENT.md) explains how the code fits together.

Bug fixes and docs are welcome straight away. For a larger change, open an issue first so we can agree the approach before you spend time on it.

## Set up

You need git and Go 1.21 or newer, which fetches the exact toolchain in [`go/go.mod`](go/go.mod) by itself. The Makefile uses that toolchain's `gofmt`, because an older one on your `PATH` can disagree with CI. [Bun](https://bun.sh) is needed only for `make ui`, using the version in `package.json`. No GPU, models or runtimes are needed for the normal tests.

```sh
git clone https://github.com/timsexperiments/LocalBase.git
cd LocalBase
make check
```

For linting, `make ui` and release rehearsal, `make bootstrap` installs the pinned tools (golangci-lint, gitleaks, syft, Bun, mdBook) into `.cache/tools`, verified by checksum. It needs no sudo and changes nothing outside this checkout.

The first `make check` downloads Go modules and compiles dependencies. From an empty cache it took about a minute on an Apple Silicon Mac. On Linux, CI runs the same Go checks; `make` itself has only been run on macOS so far, so please report anything that differs.

## Commands

| Command | What it does |
| --- | --- |
| `make bootstrap` | Install the pinned tools into `.cache/tools` |
| `make build` | Build `go/local-base` |
| `make run` | Run a gateway on a throwaway root in `.scratch/` (`PORT=26400` by default, runtimes on 26410-26414) |
| `make test` | Unit tests |
| `make race` | Unit tests with the race detector, as CI runs them |
| `make contract` | Black-box HTTP contract suite against the Go gateway |
| `make ui` | Installs JavaScript dependencies, then checks the compiled CSS is current and runs the browser JavaScript tests |
| `make lint` | golangci-lint, as CI runs it |
| `make fmt` | Format Go code |
| `make check` | Format, vet, repo guards and unit tests |
| `make rehearse` | Rehearse a release from local artifacts. Publishes nothing |
| `make docs` | Build the documentation site into `.cache/docs-site/book` |
| `make soak-smoke` | One soak cycle against fake backends, about 2 minutes. `make soak` runs for 30 minutes and `make soak-race` adds the race detector. See `go/docs/reliability.md` |

`make hooks` optionally runs the fast checks before each commit.

`tools/tailwind.sh` has pinned CSS tools for macOS ARM64 and Linux x86-64 only, so `make ui` won't run on other platforms.

## Linting

CI runs golangci-lint v2.15.0 with `go/.golangci.yml`. To run it locally: `make bootstrap`, then `make lint`.

`go/docs/lint-baseline.md` lists findings that haven't been reviewed. **It isn't a clean bill of health:** it includes real unchecked errors, and they're excluded rather than fixed. New code is still held to the rules. The one gap is a new call to a function already listed for that file, so don't rely on the linter to catch that.

## Never touch a real install

Tests and experiments must never use your real data directory, service or port.

- **Use a scratch root:** `--root` pointing at a throwaway directory. `make run` does this.
- **Use a scratch port:** anything in 24000-29999. The default real gateway is on 2273. Set the runtime ports too, such as `--llm-port`, because their defaults (`18000` and up) belong to a real install.
- **Don't run `local-base start`, `stop`, `restart` or `uninstall` casually.** They manage a user service, and `uninstall` deletes data.
- **Mind your memory.** A scratch gateway loading a model can make a real gateway on the same machine evict its own.

## Pull requests

- Keep each PR to one change, and say what it changes and why.
- Say what you ran and what happened. If you couldn't test something, say so; that's useful, not a failing.
- Add or update tests for behaviour changes, and docs for anything user-visible.
- Add a line to `CHANGELOG.md` under `[Unreleased]` for user-visible changes.
- Note any change to the database, on-disk files, `/v1` API or CLI flags. See [compatibility](#compatibility).
- CI must be green. PRs labelled `dual` also run the differential job against the TypeScript gateway.

**Commits and titles:** use an imperative summary that says what changed, such as `Refuse models that can never fit`. PR titles can use a type prefix: `fix:`, `feat:`, `docs:`, `ci:`. Branches are `type/short-description`.

**Licensing:** contributions are accepted under AGPL-3.0-or-later: inbound = outbound. New source files carry SPDX headers; see any file in `scripts/release/`.

## Compatibility

Existing data directories must keep working, and a rollback must stay a binary swap. Don't change the database schema or migration journal without discussing it first. The Go gateway must also keep the HTTP contracts existing clients use.

## How to...

### Add a model to the catalog

The built-in catalog is deliberately small: models tested on real hardware, with one chat model per memory band and one model per job. Please [request a model](.github/ISSUE_TEMPLATE/model_request.yml) before doing the work, because a maintainer has to approve catalog changes. Anyone can already use other models through imports or [their own catalog entries](docs/user-catalog.md).

1. Add a JSON file under `src/catalog/models/<kind>/` and import it in `src/catalog/models/index.ts`. The catalog source still lives there until the TypeScript version is removed.
2. Run `go generate ./internal/catalog` from `go/`. It copies the catalog into `go/internal/catalog/data`.
3. Use facts only: license, revision, sizes, hashes, context window and tasks. No quality claims or benchmark rankings. Memory figures must state whether they're measured or estimated.
4. Run `make test`.

### Add an API route

1. Register it in `Server` routing in `go/internal/gateway/server.go`, wrapped in `s.guard("<permission>", handler)`. Permissions are listed in `go/internal/auth/auth.go`.
2. Errors use the OpenAI envelope. Transient failures are 503 or 429 with `Retry-After`; permanent ones are 4xx.
3. If the browser needs it, allow it in `allowedUIRoute` in `go/internal/gateway/browser.go`.
4. Add a contract test in `go/contract/`, and list the route in [docs/api.md](docs/api.md).

### Add a UI page

1. Add a template in `go/internal/web/templates/` and a handler in `go/internal/web/`.
2. Register it in `go/internal/web/web.go`: `u.need("<permission>", handler)` for reads, and `u.mutating("<permission>", handler)` for changes, which adds the CSRF check.
3. If you changed Tailwind classes, run `go/tools/tailwind.sh` and commit `app.css`.
4. Add browser logic as small files in `go/internal/web/static/js/`, with tests in `go/tools/jstest/`. There's no Node or npm build. Product copy names no specific models.

To see pages with fixture data, run from `go/`:

```sh
LOCALBASE_CONTRACT_SERVE_UI=26401 go test ./contract -run TestServeForUI -args -impl=go
```

### Write a test

- **Unit tests** live beside the code. Prefer real behaviour over mocks; fakes exist only at real boundaries, such as `internal/runtime/fakebackend`.
- **Contract tests** in `go/contract/` drive a real gateway over HTTP on a scratch root. All writes go through `fsguard_test.go`, and `TestNoUnguardedWrites` enforces that.
- **Differential tests** compare the Go and TypeScript gateways with `-impl=both`. They need Bun and `LOCALBASE_CONTRACT_MODELS`.
- **Real inference** is opt-in through `LOCALBASE_CONTRACT_*` variables and is never required for a PR.
- **Goldens** in `testdata/` are generated from the TypeScript code by `go/tools/golden/`. Don't edit them by hand.

## Community

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md). See [SUPPORT.md](SUPPORT.md) for help, and [SECURITY.md](SECURITY.md) to report a vulnerability.
