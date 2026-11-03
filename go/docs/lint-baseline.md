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

The 54 path rules in `.golangci.yml` cover 138 calls. **They are a list of what has not been reviewed
as correct, not a claim that it is.** They exist so the job can start green and catch new problems.
A function not listed for a file still fails, but a new call to a listed function in that file does
not. Remove a rule when its calls are handled.

**Worth fixing first: these can hide a real failure.** Handling any of them changes behaviour, so
they are not in this change.

| Where | What is ignored | Why it matters |
| --- | --- | --- |
| `internal/auth/users.go` | `tx.ExecContext`, 6 calls inside transactions; `st.DB.ExecContext`, 6 | A failed write to a user's roles, or a session or link delete, is not seen. A revoke could report success. |
| `internal/auth/policy.go`, `users.go`, `guard.go` | `rows.Scan`, 9 calls | A failed scan yields an empty value in an access decision. |
| `internal/gateway/manage.go:497` | `auth.DisableAccess` | Disabling access could fail silently. |
| `internal/gateway/management.go`, `runs.go`, `obs/jsonl.go`, `install/install.go` | `os.Rename` | A failed publish or rotation is not seen. |
| `internal/video/jobs.go`, `convert.go`, `install/install.go`, `runtime_install.go` | `os.Chmod` | A private file could stay loose. |
| `internal/catalog/feed.go`, `runtime_install.go`, `web/csrf.go` | `os.MkdirAll` | Usually surfaces at the next write. |

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
