# LocalBase in Go

The Go rewrite of LocalBase. Start with [DESIGN.md](DESIGN.md).

```sh
cd go
go build -o local-base ./cmd/localbase       # one static binary, UI included
scripts/build.sh dist/local-base              # the reproducible release build, stamped
./local-base --root /path/to/root serve --port 26410 --runtime-port-base 26411
go test ./internal/...                        # unit tests (no models needed)
scripts/contract.sh                             # the contract and system suites
scripts/tailwind.sh                             # rebuild internal/web/static/app.css (commit it)
go run ./tools/catalog-feed                     # regenerate catalog/feed*.json from internal/catalog/data
```

Tests and `scripts/*-check.sh` take scratch ports from `LOCALBASE_TEST_PORTS` (`low-high`, default
`25000-25499`; CI uses `24000-29999`). Set it when something else on the machine uses that block.
24173 and 25611-25613 are never used.

A second gateway on one computer needs its own backend ports as well as its own `--port`. Without
them it uses the configured ones, such as 18000 for the LLM. `--runtime-port-base <port>` takes five
in a row: LLM, STT, TTS, image, video. A start refuses a port another process holds (DESIGN §29).

`LOCALBASE_CONTRACT_MODELS` pointing at a directory that holds
`qwen2.5-coder-1.5b-instruct-q4_k_m.gguf` (read only), with `LOCALBASE_CONTRACT_RUNTIMES` (a
`bin/runtimes` directory, copied into each scratch root), runs real inference. The tests that
compared Go with TS until step 7 pin Go's results in `testdata/golden/<platform>/`; record them
again with `LOCALBASE_UPDATE_GOLDEN=1` when a change is intended, and review the diff. The golden plans in
`internal/runtime/testdata/llm-plans.json` were recorded from the TS code and are now
Go's own: a change to them is a behaviour change, reviewed by hand.

## Build identity

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
