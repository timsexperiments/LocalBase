# LocalBase in Go

The Go rewrite of LocalBase. Start with [DESIGN.md](DESIGN.md).

```sh
cd go
go build -o local-base ./cmd/localbase       # one static binary, UI included
./local-base --root /path/to/root serve --port 26410
go test ./internal/...                        # unit tests (no models needed)
go test ./contract -args -impl=go             # HTTP contract suite against Go
go test ./contract -args -impl=both           # against TS and Go, plus differential checks
tools/tailwind.sh                             # rebuild internal/web/static/app.css (commit it)
go generate ./internal/catalog ./internal/runtime  # re-sync catalog and runtime pins from the TS tree
```

Tests and `tools/*-check.sh` take scratch ports from `LOCALBASE_TEST_PORTS` (`low-high`, default
`25000-25499`; CI uses `24000-29999`). Set it when something else on the machine uses that block.
24173 and 25611-25613 are never used.

`-impl=ts` and `-impl=both` need Bun and `LOCALBASE_CONTRACT_MODELS` pointing
at a directory that holds `qwen2.5-coder-1.5b-instruct-q4_k_m.gguf` (read only).
Add `LOCALBASE_CONTRACT_RUNTIMES` (a `bin/runtimes` directory, copied into each
scratch root) to run real inference. Golden plans come from the TS code:

```sh
bun run go/tools/golden/llm-plans.ts <models/llm dir> > go/internal/runtime/testdata/llm-plans.json
```
