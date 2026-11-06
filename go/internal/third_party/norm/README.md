# norm

Unicode normalization (NFC, NFD, NFKC, NFKD) for voice names, which must
match the TypeScript gateway's `String.prototype.normalize`. The standard
library does not export it, and the rewrite takes no module dependency beyond
modernc.org/sqlite, so this is a copy of the Go toolchain's own vendored
package.

- Upstream: `$GOROOT/src/vendor/golang.org/x/text/unicode/norm` from the Go
  go1.26.0 toolchain, which vendors `golang.org/x/text v0.32.0`.
- Unicode version: 15.0.0 (`tables15.0.0.go`). The TS gateway's ICU
  implements Unicode 17; only characters added after 15.0 can differ.
- License: BSD-3-Clause, the x/text license, in `LICENSE`.
- Files copied: composition.go, forminfo.go, input.go, iter.go,
  normalize.go, trie.go, tables15.0.0.go. Not copied: the `transform`
  reader/writer (readwriter.go, transform.go) and the other Unicode
  versions' tables.
- Local edits: the `//go:build go1.21` constraint is removed from the
  tables; `transform.ErrEndOfSpan` and `transform.ErrShortSrc` become the
  errors in `errors.go`; gofmt (Go 1.26) whitespace.

`PINS` records the sha256 of every upstream file and of every local file.
`norm_pins_test.go` fails when a local file changes without an updated pin,
and, when the toolchain running the tests vendors the same x/text version,
when the upstream files differ from the pins.

To update: copy the files again from a newer toolchain, reapply the edits
above, regenerate `PINS`, and rerun the voices goldens
(`go/tools/golden/voices.ts`).
