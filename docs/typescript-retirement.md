# Retiring the TypeScript version

**Don't start new work in `src/`.** The Go version replaces it. This plan says what's removed when.

## Why it's still here

| It still provides | Used by |
| --- | --- |
| A rollback binary (`0.1.0`) | Anyone rolling back. The binary comes from the v0.1.0 release, not this tree |
| Catalog sources in `src/catalog/` | `go generate ./internal/catalog` |
| The runtime manifest in `src/manager/` | `go generate ./internal/runtime` |
| Migrations in `drizzle/` | Embedded by `go/internal/store` |
| The other side of differential tests | `go-dual.yml`, `-impl=both` |
| Golden generators in `go/tools/golden/` | Regenerating `testdata/` |
| Runtime build and release scripts in `scripts/` | `release-whisper.yml`, `release-sd-server.yml`, `release-tts-server.yml`, `release-kokoro.yml` and their sync workflows |

## Gate

Removal starts only after the maintainer signs off on all of these:

- [ ] 0.2.0 has shipped and has run for two weeks with no rollback.
- [ ] A real video run through the web page.
- [ ] A rollback rehearsal on a copy of a long-lived data directory.
- [ ] The NVIDIA decision is made: qualify on real hardware, or remove the claim from the docs and retire anyway. It isn't a technical dependency of removal.

## Order of removal

1. **Move sources into `go/`.** The catalog goes to `go/internal/catalog/data` as the source of truth, the runtime manifest to `go/internal/runtime`, and `drizzle/` SQL to `go/internal/store/migrations`. Remove the sync scripts and their `go:generate` lines. Bytes must stay identical, because migration checksums are in the journal.
2. **Freeze the goldens.** Keep `testdata/`, remove `go/tools/golden/`, and document that goldens are now reviewed by hand.
3. **Remove the differential tests.** Remove `go-dual.yml`, `-impl=ts`, `-impl=both` and the Bun harness in `go/test/harness/`.
4. **Port or keep runtime release scripts.** They're independent of the gateway. Move the ones still in use to `scripts/runtimes/`, keeping Bun for them or porting to Go. This is the largest single task, so it can land after the others.
5. **Remove the rest.** `src/`, `ci.yml`, `release.yml`, `Dockerfile.runtime-smoke`, `scripts/release-artifacts.ts` and related scripts, `scripts/release-notices/`, and `tsconfig.json`. Trim `package.json` and `bun.lock` to what `go/tools/jstest` and the browser checks still need, and update `AGENTS.md`, `CONTRIBUTING.md` and `DEVELOPMENT.md`.

**After step 1 and before step 5,** a rollback to 0.1.0 must keep working. No Go-only drizzle migration lands until a release explicitly drops that rollback, and the changelog says so.

Keep the old stack branches until step 5, so deployed SHAs stay easy to find.

## Known gaps, left as they are

The TypeScript gateway keeps these until it's removed. The Go gateway doesn't have them.

- **A request just after a backend crash gets 502.** Between a backend process dying and Bun reaping it, the supervisor still counts the backend as running. So the relay sends the request to the dead port and answers 502 `upstream_error` ("The upstream service could not be reached."). The first request after the reap restarts the backend as it should. The Go gateway answers a backend that's gone with 503 and `Retry-After`. Found on 2026-11-07 through a flaky `gateway-crash-recovery.test.ts`, which now waits for the gateway to log the crash before its next request.
