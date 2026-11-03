# Lint baseline

`.golangci.yml` runs golangci-lint v2.15.0. On the first run with no exclusions it reported 1,095
findings. This is what happened to each, so nothing is hidden. Nothing here changes behaviour.

| Linter | Found | Fixed | Left, with a reason |
| --- | --- | --- | --- |
| `errcheck` | 700 | 0 | All below |
| `staticcheck` | 134 | 5 | 2 `SA1019`; style checks off |
| `gosec` | 251 | 0 | Selected rules only; 10 reviewed in place |
| `unused` | 8 | 5 | 3 in vendored `third_party` |
| `ineffassign` | 2 | 2 | |

## Checks that are off

- **`ST1005` (101):** our error strings are user-facing sentences, many matching TS exactly, so they
  start with a capital and end with a full stop.
- **`QF*` (18), `ST1013` (7) and other `ST` naming checks:** suggested rewrites, not defects.
- **`gosec` outside the selected rules:** `G304` (100) file paths from a variable, since a root's
  paths are this tool's job. `G115` (75) integer conversions, on sizes well in range. `G204` (33)
  launching subprocesses, which is what a runtime supervisor does. `G703` (15), `G124` (7), `G602`
  (3), `G710` (3) and `G702` (1) are off **without having been reviewed**. They were outside the
  selected set; turning one on is a useful later pass.
- **Test code:** `errcheck` (365) and `gosec` (115) are off in `_test.go`, and everywhere in
  `internal/testutil` and `internal/placement/placementtest`.
- **`internal/third_party`:** vendored as upstream wrote it.

## `errcheck`: excluded everywhere

Ignoring these is the right call, in every use:

- Closing something being read or discarded, and `Rollback` after a commit may have happened.
- `os.Remove` and `os.RemoveAll` as best-effort cleanup.
- Writing a response or a report: `fmt.Fprint*`, `ResponseWriter.Write`, `multipart` to a buffer,
  `binary.Write` to a buffer. A client that has gone away is not an error here.

## `errcheck`: the per-file baseline

The 49 path rules in `.golangci.yml` cover the calls left. **They are a list of what has not been reviewed
as correct, not a claim that it is.** They exist so the job can start green and catch new problems.
A function not listed for a file still fails, but a new call to a listed function in that file does
not. Remove a rule when its calls are handled.

**Worth fixing first: these can hide a real failure.** Handling any of them changes behaviour, so
they are not in this change.

| Where | What is ignored | Why it matters |
| --- | --- | --- |
| `internal/auth/policy.go` | `rows.Scan`, 1 call | A failed scan yields an empty value in an access decision. |
| `obs/jsonl.go`, `install/install.go` | `os.Rename`, those not handled yet | A failed publish or rotation is not seen. |
| `internal/video/jobs.go`, `convert.go`, `install/install.go`, `runtime_install.go` | `os.Chmod` | A private file could stay loose. |
| `internal/catalog/feed.go`, `runtime_install.go`, `web/csrf.go` | `os.MkdirAll` | Usually surfaces at the next write. |

Four files carry `is not checked`, which excuses any call there rather than named ones: `cmd/localbase/config.go`, `internal/catalog/feed.go`, `internal/runtime/service.go` and `internal/video/convert.go`. A new unchecked call in those isn't caught.

### Reviewed by risk

Every excused or swallowed call in `internal/auth`, `internal/store`, `internal/library`, `internal/acquire`, `internal/install` and the gateway and CLI management and configuration paths. `internal/store` and `internal/library` have none. Fixed means the error is now handled.

| Where | Call | Decision and why |
| --- | --- | --- |
| `gateway/manage.go`, the action read | `json.Unmarshal` | **Fixed.** It decides `access:read` or `access:manage`. It was already safe, because a body that can't be read is refused with a 400 whichever key sends it; now that is explicit and tested |
| `auth/policy.go` | `rows.Scan` | **Fixed in its own PR.** A role it can't read would get a new id |
| `cmd/localbase/config.go` | `MarkRestartPendingUnlessChanged` | **Fixed.** A restart owed but not recorded is now said |
| `install/install.go` | `os.Rename` of a truncated file, 2 | **Fixed.** Now reported, not downloaded past |
| `auth/email.go` | `conn.SetDeadline` | **Fixed.** Without it a silent mail server holds a sign-in |
| `auth/email.go` | `qp.Write`, `qp.Close` | Into a buffer: can't fail |
| `auth/email.go` | `client.Close` | The message has been sent or has already failed |
| `gateway/manage.go` | `json.Unmarshal` of `in.raw` | The strict decode has just accepted it |
| `gateway/manage.go` | `json.Unmarshal` in `nullField` | A body that isn't an object has no such member |
| `gateway/placement.go`, `cmd/localbase/placement.go` | `json.Unmarshal` | This process's own marshalled struct |
| `install/install.go` | `os.Chmod` of `.checksums.json.lock.db` | A lock file with no data, in a private folder |
| `install/install.go` | `conn.Close`, the deferred `ROLLBACK` | Cleanup of a lock held by the connection; the kernel releases it |
| `acquire/resolve.go` | `io.Copy` to discard | Draining a response before reuse |
| `cmd/localbase/config.go`, `maintenance.go` | `st.Close` | At the end of the command |

The rest I have not gone through one by one. By name they are mostly `json.Unmarshal` (24), JSON
encoding and decoding (19), `Close` on stores at exit (7), and `io.Copy` (5).

## Reviewed in place

Each has a `//nolint` with its reason:

- `G203` in `internal/web/chat.go`: `template.JS` of `json.Marshal` output, which escapes `<`, `>`, `&`.
- `G302` on five directories: a directory needs its execute bit.
- `G305` in `internal/runtime/runtime_install.go`: the next line is the traversal guard.
- `G404` in `internal/web/labimage.go`: an image seed, not a secret.
- `SA1019` in `internal/auth/jwt.go`: `ecdsa.PublicKey.X` and `IsOnCurve` are deprecated. Moving to
  `crypto/ecdh` is a real change to token checking, so it needs its own PR and tests.

## `errcheck`: handled in place

Each ignored error in the lint-zero pass has a comment saying why it can be ignored. Two became real
handling: `DeleteLocalSession` returns its error, since a failed delete leaves a session valid, and
`TightenPermissions` reports a failed walk. Writes of run checkpoints and model operations now warn
when refused as unsafe.
