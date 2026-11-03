# Tools

Most scripts here say what they do at the top. This covers what the reliability and
performance tools share. `docs/reliability.md` and `docs/performance.md` explain the tools
themselves.

| Run from `go/` | What it does |
|---|---|
| `make soak` (`tools/soak.sh`) | A real gateway over fake backends, through memory phases, drain and restart |
| `make chaos` (`tools/chaos`) | What breaks, and what a person sees when it does |
| `make bench` (`tools/bench.sh`) | What the gateway adds per request and per token, against budgets |
| `make e2e` (`tools/e2e.sh`) | Headless Chrome: journeys and a matrix of pages, widths and themes |
| `tools/fetch-ci-tools.sh <dir>` | The lint and secret-scan tools at CI's versions |

## Where things go

- **Built tools:** `~/.cache/localbase/`, or `LOCALBASE_TOOLS_CACHE` if set. They are
  replaced by the next run, so nothing needs cleaning up.
- **The browser suite's Chrome and axe-core:** `~/.cache/localbase-e2e/`, or
  `LOCALBASE_E2E_CACHE` if set.
- **Results:** a new directory under `$TMPDIR`, or `-out <dir>`.
- **Ports:** the lab uses 29000-29499, or `LOCALBASE_LAB_PORTS=<low>-<high>`. Go tests use
  `LOCALBASE_TEST_PORTS=<low>-<high>`, where the tree has `internal/testutil/ports.go`.

Nothing is installed system-wide.

## Anything fetched

**Nothing fetched is unpacked or run until its checksum has matched, and the first mismatch
stops the script.** That applies to scripts here and to any one-off fetch:

- Pin the sha256 in this repository where you can, and compare against it exactly.
- If the checksum comes from an upstream list, match the exact file name. A pattern can also
  match a neighbour, such as the same archive's `.sbom.json`.
- Use `set -e`, or an explicit exit, so that a failed compare stops everything after it.
  Printing a warning and carrying on is how a bad binary gets run.
