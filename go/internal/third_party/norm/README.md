# norm

Unicode normalization (NFC, NFD, NFKC, NFKD), copied unchanged from the Go
1.26 toolchain's vendored `golang.org/x/text/unicode/norm` (Unicode 15.0.0
tables), without the `transform` reader/writer. `LICENSE` is the x/text BSD
license. Voice names need NFC and NFKC to match the TypeScript gateway's
`String.prototype.normalize`; the standard library does not export them, and
the rewrite takes no module dependency beyond modernc.org/sqlite.

Files: composition.go, forminfo.go, input.go, iter.go, normalize.go, trie.go,
tables15.0.0.go (build tag removed). Edits: `transform.ErrEndOfSpan` and
`transform.ErrShortSrc` become the local errors in errors.go.
